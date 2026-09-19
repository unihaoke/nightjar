package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// engineGuard 抽象成本记账，避免代码分析服务直接依赖护栏实现细节。
type engineGuard interface {
	Check(userID int64) error
	Commit(userID int64, usage int) (float64, string)
}

// CodeAnalysisService 把日志告警的错误信息交给**外部 AI 分析服务**，并负责收结论。
//
// 链路（异步任务模型，与 ai_analysis_client.go 的注释配套）：
//
//	告警事件 → Submit（提交问题，拿到 task_id）→ 事件置 awaiting
//	                                              ↓
//	           AI 服务回调 / 平台轮询 → CompleteByTask → 写结论 → worker 通知
//
// 为什么不再是"拉代码 + 本地定位 + 调 LLM"：代码分析这件事交给专门的 AI 服务更合适
// （它自己有代码与上下文），平台只负责把"问题"问出去、把"答案"收回来并送到通知里。
// 平台侧因此不再需要 git、不再持有任何代码副本，也不再有"把代码片段外发"这个合规面。
//
// 仍然保留的两条合规规矩：
//  1. 出网白名单（security.outbound_whitelist）：服务不在名单里就不许问外部 AI；
//  2. 脱敏：错误信息与堆栈在送出前先过一遍脱敏器（IP/手机号/邮箱/口令…）。
type CodeAnalysisService struct {
	cfg  *config.Config
	mu   sync.RWMutex
	// client 由「AI 设置」保存时热替换，所以读写都要过 mu：
	// 提交链路正在用旧客户端发请求时，管理员点了保存，不能让读方拿到半个新对象。
	client   *AIAnalysisClient
	events   *repository.LogEventRepository
	analyses *repository.CodeAnalysisRepository
	tasks    *repository.AIAnalysisTaskRepository
	redactor *Redactor
	audit    *AuditService
	cost     engineGuard
	log      *zap.Logger
}

// Reload 按当前内存配置重建 AI 分析客户端（「AI 设置」保存后即时生效）。
func (s *CodeAnalysisService) Reload() {
	if s == nil {
		return
	}
	var next *AIAnalysisClient
	if s.cfg != nil {
		next = NewAIAnalysisClient(s.cfg.AIAnalysis, s.log)
	}
	s.mu.Lock()
	s.client = next
	s.mu.Unlock()
	if next != nil && next.Configured() {
		s.log.Info("AI 代码分析服务已按平台设置生效", zap.String("base_url", s.cfg.AIAnalysis.BaseURL))
	}
}

// currentClient 取当前客户端快照。
func (s *CodeAnalysisService) currentClient() *AIAnalysisClient {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}

// NewCodeAnalysisService 构造代码分析服务。
func NewCodeAnalysisService(
	cfg *config.Config,
	events *repository.LogEventRepository,
	analyses *repository.CodeAnalysisRepository,
	tasks *repository.AIAnalysisTaskRepository,
	redactor *Redactor,
	audit *AuditService,
	cost engineGuard,
	log *zap.Logger,
) *CodeAnalysisService {
	if log == nil {
		log = zap.NewNop()
	}
	var client *AIAnalysisClient
	if cfg != nil {
		client = NewAIAnalysisClient(cfg.AIAnalysis, log)
	}
	return &CodeAnalysisService{
		cfg: cfg, client: client, events: events, analyses: analyses, tasks: tasks,
		redactor: redactor, audit: audit, cost: cost, log: log,
	}
}

// Ready 报告 AI 分析能力是否可用（服务已装配且外部 AI 服务已配置）。
//
// 没配置时不能"假装能分析"：调用方据此把事件置为 disabled 并写明原因，
// 而不是让它一直停在待分析队列里。
func (s *CodeAnalysisService) Ready() bool {
	client := s.currentClient()
	return client != nil && client.Configured()
}

// ---------------------------------------------------------------------------
// 合规闸门
// ---------------------------------------------------------------------------

// thirdPartyAllowedFor 计算"本次是否允许把问题发给外部 AI 服务"。
//
// 两个条件同时成立才算允许（缺一即不允许，且**不允许就等于不调用**）：
//  1. 本次请求没有显式要求 force_local；
//  2. 服务在出网白名单里（security.outbound_whitelist）。
//
// 抽成纯函数的原因：它是整条链路里最敏感的一条判定，必须能被单测逐格钉住
//（真实缺陷 INC-030 就是"算了但没用"，所以连"给使用者看的告警文案"一起钉）。
func thirdPartyAllowedFor(forceLocal, whitelisted bool, serviceName string) (allowed bool, warn string) {
	if forceLocal {
		return false, ""
	}
	if !whitelisted {
		return false, "服务 " + serviceName + " 不在出网白名单内，已跳过 AI 分析"
	}
	return true, ""
}

// outboundGate 是"这次能不能调用外部 AI 服务"的**唯一判定入口**。
//
// 返回：
//   - allowed：可以调用；
//   - blocked：不能调用，且**原因必须给出**（写进报告、事件状态与接口错误）。
//
// 为什么把"是否外发"和"是否许可"放进同一个函数：INC-030 的缺陷正是
// "两个条件都算出来了、但调用点只用了一个"。收成一个函数后，
// 调用点就只剩 `if blocked { ... }` —— 少一个可以忘记使用的变量。
func (s *CodeAnalysisService) outboundGate(
	forceLocal bool, whitelisted bool, serviceName string,
) (allowed, blocked bool, warn, reason string) {
	// 外部 AI 服务**天然是组织外部的一方**：调用它就是出网，
	// 这与"引擎是不是自建"无关（本服务不再走平台内置引擎）。
	allowed, warn = thirdPartyAllowedFor(forceLocal, whitelisted, serviceName)
	if !allowed {
		return false, true, warn, s.outboundBlockReason(serviceName, forceLocal)
	}
	return allowed, false, warn, ""
}

// engineNone 是"没有调用任何 AI"时写进报告的引擎名。
//
// 刻意不用服务自己的名字：合规阻断的那条记录里必须一眼看出"这次根本没问 AI"，
// 否则使用者会以为结论来自模型（同 INC-016 的"不许有看起来像真的假数据"）。
const engineNone = "none"

// engineExternal 是外部 AI 分析服务在报告里的名字。
const engineExternal = "external_ai_analysis"

// defaultCallbackPath 是平台接收 AI 结论的路径（注册在公开路由上，靠令牌鉴权）。
const defaultCallbackPath = "/api/ai/analysis/callback"

// outboundBlockReason 生成合规阻断时给使用者看的原因与出路（必须可操作）。
func (s *CodeAnalysisService) outboundBlockReason(serviceName string, forceLocal bool) string {
	reasons := make([]string, 0, 3)
	if forceLocal {
		reasons = append(reasons, "本次请求显式要求「不调用外部 AI」（force_local）")
	}
	reasons = append(reasons,
		"服务 "+serviceName+" 不在出网白名单里（security.outbound_whitelist）")
	return "按合规设置，本次不允许把问题发送给外部 AI：" + strings.Join(reasons, "；") +
		"。两条出路：① 把该服务加入出网白名单（MWOPS_SECURITY_OUTBOUND_WHITELIST）；" +
		"② 改用平台内置引擎做本地诊断（此时不会把任何内容送出平台）。"
}

// ---------------------------------------------------------------------------
// 请求 / 结果
// ---------------------------------------------------------------------------

// CodeAnalysisRequest 是一次分析请求。
type CodeAnalysisRequest struct {
	EventID int64 `json:"event_id"`
	// ServiceName 允许直接提交问题（不关联事件），用于手工排查。
	ServiceName string `json:"service"`
	Stacktrace  string `json:"stacktrace"`
	Message     string `json:"message"`
	// ForceLocal 强制不调用外部 AI（合规场景，6.5）。
	ForceLocal bool `json:"force_local"`
}

// SubmitOutcome 是提交的结果。
type SubmitOutcome struct {
	TaskID      string `json:"task_id"`
	EventID     int64  `json:"event_id"`
	ServiceName string `json:"service_name"`
	// Async 为 true 表示结论要等回调/轮询（事件应置为 awaiting）；
	// false 表示 AI 服务同步给了结论（sync_mode 或未配置异步能力）。
	Async bool `json:"async"`
	// Report 只在同步模式下有值。
	Report map[string]any `json:"report,omitempty"`
	// Brief 为同步模式下的通知摘要（异步模式由回调/轮询回来时再生成）。
	Brief *LogAnalysisBrief `json:"-"`
}

// Completion 是"结论到达"的结果，供 worker 落状态与外发通知。
type Completion struct {
	EventID     int64
	ServiceName string
	TaskID      string
	// Brief 为通知用的结论摘要；失败时为 nil。
	Brief *LogAnalysisBrief
	// Failed 表示这次没有拿到可用结论（AI 侧失败 / 解析失败 / 超时）。
	Failed bool
	// Reason 为失败原因（页面上直接展示）。
	Reason string
}

// Submit 提交一次分析：组装问题 → 合规判定 → 调用外部 AI 服务 → 落任务记录。
func (s *CodeAnalysisService) Submit(ctx context.Context, in CodeAnalysisRequest, operator Operator) (*SubmitOutcome, error) {
	if s == nil {
		return nil, apperr.New(apperr.CodeInternal, "代码分析服务未装配")
	}
	var (
		serviceName = in.ServiceName
		stack       = in.Stacktrace
		message     = in.Message
	)
	if in.EventID > 0 {
		item, err := s.events.Get(ctx, in.EventID)
		if err != nil {
			if repository.EnsureNotFound(err) {
				return nil, apperr.New(apperr.CodeNotFound, "日志事件不存在")
			}
			return nil, apperr.Wrap(apperr.CodeInternal, err)
		}
		serviceName = item.ServiceName
		stack = item.RawStacktrace
		message = item.ErrorSignature
	}
	if strings.TrimSpace(stack) == "" && strings.TrimSpace(message) == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "必须提供 event_id 或错误信息/堆栈")
	}
	if !s.Ready() {
		return nil, apperr.New(apperr.CodeOutboundDenied,
			"未配置外部 AI 分析服务：请在「AI 设置 → AI 代码分析」中启用并填写服务地址")
	}
	if s.cost != nil && operator.UserID > 0 {
		if err := s.cost.Check(operator.UserID); err != nil {
			return nil, apperr.Wrap(apperr.CodeQuotaExceeded, err)
		}
	}

	// ① 合规闸门：不合格就到此为止，绝不把问题发出去。
	_, blocked, _, reason := s.outboundGate(in.ForceLocal,
		inWhitelist(s.cfg.Security.OutboundWhitelist, serviceName), serviceName)
	if blocked {
		s.log.Warn("已阻止把问题发给外部 AI（合规设置）",
			zap.String("service", serviceName), zap.Bool("force_local", in.ForceLocal))
		return nil, apperr.New(apperr.CodeOutboundDenied, reason)
	}

	// ② 脱敏：错误信息与堆栈先过一遍脱敏器，再拼成问题。
	question := s.buildQuestion(serviceName, message, stack)

	// ③ 提交：平台先生成 task_id（幂等键），AI 服务不认就以它返回的为准。
	taskID := newAnalysisTaskID(in.EventID)
	deadline := time.Now().UTC().Add(s.taskTimeout())
	client := s.currentClient()
	if client == nil {
		return nil, apperr.New(apperr.CodeOutboundDenied,
			"未配置外部 AI 分析服务：请在「AI 设置 → AI 代码分析」中填写服务地址并启用")
	}
	res, err := client.Submit(ctx, SubmitInput{
		TaskID:      taskID,
		Service:     serviceName,
		Question:    question,
		CallbackURL: s.callbackURL(),
		EventID:     in.EventID,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeEngineFailed, err)
	}
	task := &model.AIAnalysisTask{
		EventID:     in.EventID,
		ServiceName: serviceName,
		TaskID:      res.TaskID,
		Status:      model.AIAnalysisTaskSubmitted,
		Question:    question,
		DeadlineAt:  &deadline,
	}
	if err := s.tasks.Create(ctx, task); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}

	out := &SubmitOutcome{
		TaskID:      res.TaskID,
		EventID:     in.EventID,
		ServiceName: serviceName,
		Async:       res.Status != modelStatusSucceeded,
	}

	// 同步模式：提交响应里就带着结论，直接收尾，不进等待队列。
	if res.Status == modelStatusSucceeded {
		completion, err := s.CompleteByTask(ctx, res.TaskID, model.AIAnalysisTaskSucceeded, res.Answer, "")
		if err != nil {
			return nil, err
		}
		out.Async = false
		out.Report = completion.report()
		out.Brief = completion.Brief
		if completion.Failed {
			return nil, apperr.New(apperr.CodeEngineFailed, completion.Reason)
		}
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, ActionType: "ai_code_analyze_submit",
			Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{
				"event_id": in.EventID, "service": serviceName,
				"task_id": res.TaskID, "async": out.Async,
			},
		})
	}
	return out, nil
}

// CompleteByTask 收下一条结论（回调与轮询共用），返回可供通知的结果。
//
// 幂等是关键：回调可能重复投递（AI 服务没收到 2xx 会重试），
// 也可能与轮询撞在一起。仓储层的 Complete 用"当前状态必须是 submitted"做条件更新，
// 第二个到达的调用者会拿到 applied=false，这里直接返回已存在的结论而不重复写报告。
func (s *CodeAnalysisService) CompleteByTask(ctx context.Context, taskID, status, answer, errMsg string) (*Completion, error) {
	if s == nil || s.tasks == nil {
		return nil, apperr.New(apperr.CodeInternal, "代码分析服务未装配")
	}
	task, err := s.tasks.GetByTaskID(ctx, taskID)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "分析任务不存在："+taskID)
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	completion := &Completion{
		EventID:     task.EventID,
		ServiceName: task.ServiceName,
		TaskID:      task.TaskID,
	}
	if task.Status != model.AIAnalysisTaskSubmitted {
		// 已经有结论了（重复回调 / 已被超时收尾）：返回既有结论，不再写一份。
		return s.completionFromExisting(ctx, task), nil
	}
	// 结论为空但状态是成功 → 按"没有可用结论"处理，而不是写一个空报告。
	if status == model.AIAnalysisTaskSucceeded && strings.TrimSpace(answer) == "" {
		status = model.AIAnalysisTaskFailed
		errMsg = "AI 服务返回成功但没有结论内容"
	}
	applied, err := s.tasks.Complete(ctx, taskID, status, answer, errMsg, time.Now().UTC())
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if !applied {
		return s.completionFromExisting(ctx, task), nil
	}
	if status != model.AIAnalysisTaskSucceeded {
		completion.Failed = true
		completion.Reason = truncateText(defaultString(errMsg, "AI 分析未返回结论"), 500)
		s.log.Warn("AI 分析任务未产出结论",
			zap.String("task_id", taskID), zap.String("status", status), zap.String("reason", completion.Reason))
		return completion, nil
	}
	completion.Brief = s.buildBrief(task, answer)
	if err := s.saveReport(ctx, task, answer); err != nil {
		s.log.Warn("保存 AI 分析报告失败", zap.Error(err))
	}
	return completion, nil
}

// completionFromExisting 把已有任务记录翻译成 Completion（重复回调/已超时场景）。
func (s *CodeAnalysisService) completionFromExisting(ctx context.Context, task *model.AIAnalysisTask) *Completion {
	completion := &Completion{
		EventID:     task.EventID,
		ServiceName: task.ServiceName,
		TaskID:      task.TaskID,
	}
	if task.Status == model.AIAnalysisTaskSucceeded && strings.TrimSpace(task.Answer) != "" {
		completion.Brief = s.buildBrief(task, task.Answer)
		return completion
	}
	completion.Failed = true
	completion.Reason = truncateText(defaultString(task.Error, "AI 分析未返回结论"), 500)
	return completion
}

// saveReport 把结论落成一条分析报告。
func (s *CodeAnalysisService) saveReport(ctx context.Context, task *model.AIAnalysisTask, answer string) error {
	if s.analyses == nil {
		return nil
	}
	report := parseCodeReport(answer)
	record := &model.AICodeAnalysis{
		EventID:       task.EventID,
		ServiceName:   task.ServiceName,
		RootCause:     strOf(report["root_cause"]),
		EmergencyPlan: strOf(report["emergency_plan"]),
		FixSuggestion: strOf(report["fix_suggestion"]),
		ImpactScope:   strOf(report["impact_scope"]),
		Confidence:    confidenceOf(report),
		EngineUsed:    engineExternal,
		EngineStatus:  model.EngineStatusOK,
		Evidence: model.JSONMap{
			"task_id":  task.TaskID,
			"question": truncateRunes(task.Question, 2000),
			"answer":   truncateRunes(answer, 8000),
		},
	}
	if v, ok := report["located_file"].(string); ok {
		record.LocatedFile = v
	}
	if v, ok := report["located_line"].(float64); ok {
		record.LocatedLine = int(v)
	}
	return s.analyses.Create(ctx, record)
}

// buildBrief 把 AI 的结论整理成通知卡片用的摘要。
func (s *CodeAnalysisService) buildBrief(task *model.AIAnalysisTask, answer string) *LogAnalysisBrief {
	report := parseCodeReport(answer)
	brief := &LogAnalysisBrief{
		RootCause:     strOf(report["root_cause"]),
		FixSuggestion: strOf(report["fix_suggestion"]),
		ImpactScope:   strOf(report["impact_scope"]),
		Confidence:    confidenceOf(report),
		EngineUsed:    engineExternal,
	}
	if v, ok := report["located_file"].(string); ok {
		brief.LocatedFile = v
	}
	if v, ok := report["located_line"].(float64); ok {
		brief.LocatedLine = int(v)
	}
	// AI 给的不是结构化 JSON 时，把原文放进根因，避免通知里什么都没有。
	if brief.RootCause == "" && strings.TrimSpace(answer) != "" {
		brief.RootCause = truncateRunes(answer, 800)
	}
	_ = task
	return brief
}

// ---------------------------------------------------------------------------
// 轮询兜底与超时收尾
// ---------------------------------------------------------------------------

// PendingTasks 取仍在等待结论的任务（轮询入口）。
func (s *CodeAnalysisService) PendingTasks(ctx context.Context, limit int) ([]model.AIAnalysisTask, error) {
	if s == nil || s.tasks == nil {
		return nil, nil
	}
	return s.tasks.ListUnfinished(ctx, limit)
}

// RefreshTask 主动查一次任务状态；有结论就收尾并返回 completion。
//
// 只在"已经过了最小等待时间"时才查：刚提交就去查，既浪费一次请求，
// 也容易让还没开始处理的任务被误判。最小等待时间取轮询间隔的一半。
func (s *CodeAnalysisService) RefreshTask(ctx context.Context, task model.AIAnalysisTask) (*Completion, bool, error) {
	client := s.currentClient()
	if client == nil {
		return nil, false, nil
	}
	if wait := s.pollInterval() / 2; wait > 0 && time.Since(task.CreatedAt) < wait {
		return nil, false, nil
	}
	res, err := client.Query(ctx, task.TaskID)
	if err != nil {
		return nil, false, err
	}
	switch res.Status {
	case modelStatusSucceeded:
		completion, err := s.CompleteByTask(ctx, task.TaskID, model.AIAnalysisTaskSucceeded, res.Answer, "")
		return completion, true, err
	case modelStatusFailed:
		completion, err := s.CompleteByTask(ctx, task.TaskID, model.AIAnalysisTaskFailed, "",
			defaultString(res.Error, "AI 服务报告分析失败"))
		return completion, true, err
	default:
		return nil, false, nil
	}
}

// TimeoutTasks 把已超过截止时间的任务判为超时（避免回调丢失导致事件永远等待）。
func (s *CodeAnalysisService) TimeoutTasks(ctx context.Context, now time.Time, limit int) ([]*Completion, error) {
	if s == nil || s.tasks == nil {
		return nil, nil
	}
	items, err := s.tasks.ListTimedOut(ctx, now, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	out := make([]*Completion, 0, len(items))
	for i := range items {
		task := items[i]
		reason := "AI 分析超时（超过 " + s.taskTimeout().String() + " 仍未返回结论）"
		if _, err := s.tasks.Complete(ctx, task.TaskID, model.AIAnalysisTaskTimeout, "", reason, now); err != nil {
			s.log.Warn("标记分析任务超时失败", zap.String("task_id", task.TaskID), zap.Error(err))
			continue
		}
		s.log.Warn("AI 分析任务超时", zap.String("task_id", task.TaskID),
			zap.String("service", task.ServiceName), zap.Int64("event_id", task.EventID))
		out = append(out, &Completion{
			EventID:     task.EventID,
			ServiceName: task.ServiceName,
			TaskID:      task.TaskID,
			Failed:      true,
			Reason:      reason,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 查询
// ---------------------------------------------------------------------------

// List 分页查询分析报告。
func (s *CodeAnalysisService) List(ctx context.Context, serviceName string, limit, offset int) ([]model.AICodeAnalysis, int64, error) {
	items, total, err := s.analyses.List(ctx, serviceName, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// GetByEvent 按事件查询报告。
func (s *CodeAnalysisService) GetByEvent(ctx context.Context, eventID int64) (*model.AICodeAnalysis, error) {
	item, err := s.analyses.GetByEvent(ctx, eventID)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "该事件还没有 AI 分析报告")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// TaskOf 取某条事件最近一次分析任务（页面展示"AI 分析进行到哪一步"）。
func (s *CodeAnalysisService) TaskOf(ctx context.Context, eventID int64) (*model.AIAnalysisTask, error) {
	if s == nil || s.tasks == nil || eventID <= 0 {
		return nil, nil
	}
	item, err := s.tasks.GetByEvent(ctx, eventID)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// buildQuestion 把错误信息与堆栈拼成"问给 AI 的一句话"。
//
// 刻意只带问题本身：平台不再持有代码，也不再代 AI 做定位——
// 那是 AI 服务的事，平台只保证送去的内容已经脱敏。
func (s *CodeAnalysisService) buildQuestion(service, message, stacktrace string) string {
	redactedMessage := s.redact(message)
	redactedStack := s.redact(stacktrace)
	var b strings.Builder
	b.WriteString("服务：")
	b.WriteString(defaultString(service, "unknown"))
	b.WriteString("\n错误信息：")
	b.WriteString(defaultString(redactedMessage, "（无）"))
	if strings.TrimSpace(redactedStack) != "" {
		b.WriteString("\n堆栈：\n")
		b.WriteString(truncateRunes(redactedStack, 4000))
	}
	b.WriteString("\n请分析根因、影响范围与修复建议，并尽量给出出错的文件与行号。")
	return b.String()
}

// redact 脱敏（redactor 未装配时原样返回：宁可不脱敏也要让调用方拿到内容去判断，
// 但正常装配路径上它一定存在）。
func (s *CodeAnalysisService) redact(text string) string {
	if s.redactor == nil {
		return text
	}
	return s.redactor.Redact(text)
}

// callbackURL 生成本平台接收结论的地址。
func (s *CodeAnalysisService) callbackURL() string {
	if s.cfg == nil {
		return ""
	}
	base := strings.TrimSpace(s.cfg.AIAnalysis.CallbackURL)
	if base == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + defaultCallbackPath
}

// taskTimeout 返回任务级超时（未配置时 30 分钟）。
func (s *CodeAnalysisService) taskTimeout() time.Duration {
	if s.cfg != nil && s.cfg.AIAnalysis.TaskTimeout > 0 {
		return s.cfg.AIAnalysis.TaskTimeout
	}
	return 30 * time.Minute
}

// pollInterval 返回轮询间隔（未配置时 60 秒）。
func (s *CodeAnalysisService) pollInterval() time.Duration {
	if s.cfg != nil && s.cfg.AIAnalysis.PollInterval > 0 {
		return s.cfg.AIAnalysis.PollInterval
	}
	return 60 * time.Second
}

// newAnalysisTaskID 生成任务号：可读（含事件号）+ 唯一（纳秒时间戳）。
func newAnalysisTaskID(eventID int64) string {
	return fmt.Sprintf("mwo-%d-%d", eventID, time.Now().UnixNano())
}

// confidenceOf 取结论里的置信度（没有给定时用 0.4：既不敢说确定，也不是零）。
func confidenceOf(report map[string]any) float64 {
	if v, ok := report["confidence"].(float64); ok {
		return v
	}
	return 0.4
}

// report 把 completion 的结论还原成报告 map（同步模式回给接口用）。
func (c *Completion) report() map[string]any {
	if c == nil || c.Brief == nil {
		return nil
	}
	out := map[string]any{
		"root_cause":     c.Brief.RootCause,
		"fix_suggestion": c.Brief.FixSuggestion,
		"impact_scope":   c.Brief.ImpactScope,
		"confidence":     c.Brief.Confidence,
	}
	if c.Brief.LocatedFile != "" {
		out["located_file"] = c.Brief.LocatedFile
	}
	if c.Brief.LocatedLine > 0 {
		out["located_line"] = c.Brief.LocatedLine
	}
	return out
}

// parseCodeReport 解析 AI 的输出（尽量按 JSON，解析不了就当纯文本）。
func parseCodeReport(content string) map[string]any {
	out := map[string]any{}
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return out
	}
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(trimmed[start:end+1]), &out); err == nil {
			return out
		}
	}
	// 无法解析为 JSON 时按纯文本保留，保证信息不丢失。
	out["root_cause"] = truncateRunes(trimmed, 800)
	out["confidence"] = 0.3
	return out
}

// strOf 安全取字符串。
func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

// truncateRunes 按 rune 截断。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// inWhitelist 判断服务是否在出网白名单内。
func inWhitelist(whitelist []string, serviceName string) bool {
	for _, item := range whitelist {
		if item == "*" || strings.EqualFold(item, serviceName) {
			return true
		}
	}
	return false
}
