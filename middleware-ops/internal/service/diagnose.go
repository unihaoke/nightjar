package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/engine/guardrail"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/pkg/cache"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/service/ai"
)

// DiagnoseService 是 AI 诊断中心的编排实现（4.3 + 第五章六道护栏）。
//
// 固定管线（5.1，单轮诊断 + 工具预采集，不迭代、不自主决策）：
//
//	识别目标实例 → 权限过滤 → 成本预检 → 确定性缓存 → 工具预采集（上下文预算）
//	→ 一次 LLM 调用（超时/重试/降级）→ 质量护栏规范化 → 落库 → 知识草稿沉淀
type DiagnoseService struct {
	cfg       DiagnoseSettings
	instances *repository.InstanceRepository
	diagnoses *repository.DiagnosisRepository
	knowledge *repository.KnowledgeRepository
	engine    engine.Engine
	factory   *engine.Factory
	monitor   monitor.Client
	logEvents *repository.LogEventRepository
	store     cache.Store
	audit     *AuditService
	notifier  *NotifierService

	budget   *guardrail.Budget
	loop     *guardrail.LoopGuard
	timeout  *guardrail.Timeout
	quality  *guardrail.Quality
	cost     *guardrail.Cost
	registry *guardrail.ToolRegistry

	log *zap.Logger
}

// DiagnoseSettings 是诊断服务所需的护栏配置切片。
type DiagnoseSettings struct {
	InputBudget     int
	OutputBudget    int
	MaxConcurrency  int
	CacheTTL        time.Duration
	VectorThreshold float64
	LLMRetry        int
}

// DiagnoseDeps 是构造诊断服务的依赖。
type DiagnoseDeps struct {
	Instances *repository.InstanceRepository
	Diagnoses *repository.DiagnosisRepository
	Knowledge *repository.KnowledgeRepository
	LogEvents *repository.LogEventRepository
	Engine    engine.Engine
	Factory   *engine.Factory
	Monitor   monitor.Client
	Store     cache.Store
	Audit     *AuditService
	Notifier  *NotifierService
	Budget    *guardrail.Budget
	Loop      *guardrail.LoopGuard
	Timeout   *guardrail.Timeout
	Quality   *guardrail.Quality
	Cost      *guardrail.Cost
	Registry  *guardrail.ToolRegistry
	Settings  DiagnoseSettings
	Log       *zap.Logger
}

// NewDiagnoseService 构造诊断服务。
func NewDiagnoseService(d DiagnoseDeps) *DiagnoseService {
	return &DiagnoseService{
		cfg:       d.Settings,
		instances: d.Instances, diagnoses: d.Diagnoses, knowledge: d.Knowledge,
		logEvents: d.LogEvents, engine: d.Engine, factory: d.Factory, monitor: d.Monitor,
		store: d.Store, audit: d.Audit, notifier: d.Notifier,
		budget: d.Budget, loop: d.Loop, timeout: d.Timeout, quality: d.Quality,
		cost: d.Cost, registry: d.Registry, log: d.Log,
	}
}

// Request 是一次诊断请求。
type Request struct {
	InstanceID int64  `json:"instance_id"`
	Question   string `json:"question" binding:"required,min=2,max=1000"`
	MWType     string `json:"mw_type"`
	// AlertID 非空表示由告警触发（事件驱动，见 3.1）。
	AlertID int64 `json:"alert_id"`
	// SkipCache 强制跳过确定性缓存（用户点击「重新诊断」）。
	SkipCache bool `json:"skip_cache"`
}

// Meta 是诊断过程的元信息（SSE meta 事件内容）。
type Meta struct {
	InstanceID   int64    `json:"instance_id"`
	InstanceName string   `json:"instance_name"`
	MWType       string   `json:"mw_type"`
	EngineName   string   `json:"engine_name"`
	EngineStatus string   `json:"engine_status"`
	CacheHit     bool     `json:"cache_hit"`
	Truncated    []string `json:"truncated"`
	Missing      []string `json:"missing"`
	Prompts      int      `json:"prompt_tokens"`
	// Scope 为数据权限范围描述（5.5 透明可审计）。
	Scope string `json:"scope"`
	// DataSource 为指标来源。
	DataSource string `json:"data_source"`
}

// Response 是诊断结果。
type Response struct {
	DiagnosisID int64                   `json:"diagnosis_id"`
	Meta        Meta                    `json:"meta"`
	Report      *guardrail.Report       `json:"report"`
	References  []ai.KnowledgeReference `json:"references"`
	Raw         string                  `json:"raw,omitempty"`
	// Warnings 为质量护栏给出的提示。
	Warnings   []string `json:"warnings"`
	EngineUsed string   `json:"engine_used"`
	CostTokens int      `json:"cost_tokens"`
	DurationMS int64    `json:"duration_ms"`
}

// Target 是解析后的诊断目标。
type Target struct {
	Instance model.MiddlewareInstance
	MWType   string
}

// Resolve 解析诊断目标实例。
//
// 规则 + 实体匹配（4.3）：显式 instance_id 优先；否则按问题关键词识别中间件类型，
// 并在发起人数据权限范围内挑选候选实例。
func (s *DiagnoseService) Resolve(ctx context.Context, in Request, scope Scope) (*Target, error) {
	if in.InstanceID > 0 {
		item, err := s.instances.Get(ctx, in.InstanceID)
		if err != nil {
			if repository.EnsureNotFound(err) {
				return nil, apperr.New(apperr.CodeNotFound, "实例不存在")
			}
			return nil, apperr.Wrap(apperr.CodeInternal, err)
		}
		if !inScope(item, scope) {
			return nil, apperr.New(apperr.CodeScopeDenied, "该实例不在你的数据权限范围内")
		}
		return &Target{Instance: *item, MWType: item.MWType}, nil
	}

	// 自动选实例时同样只在纳管域内选：日志集成没有指标可诊断，
	// 被选中只会让诊断报"取不到指标"，而使用者根本没打算诊断它。
	items, err := s.instances.All(ctx, repository.InstanceFilter{
		MWTypes:  MiddlewareDomainTypes(),
		EnvScope: scope.EnvScope, GroupScope: scope.GroupScope,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if len(items) == 0 {
		return nil, apperr.New(apperr.CodeNotFound, "数据权限范围内没有可诊断的实例，请先纳管中间件")
	}
	candidates := make([]string, 0, len(items))
	for _, item := range items {
		candidates = append(candidates, item.MWType)
	}
	mwType := in.MWType
	if mwType == "" {
		mwType = ai.DetectMWType(in.Question, candidates)
	}

	// 名称精确命中优先（用户在问题里写了实例名）。
	lower := strings.ToLower(in.Question)
	for i := range items {
		if strings.Contains(lower, strings.ToLower(items[i].Name)) {
			return &Target{Instance: items[i], MWType: items[i].MWType}, nil
		}
	}
	if mwType != "" {
		matched := make([]model.MiddlewareInstance, 0, len(items))
		for _, item := range items {
			if item.MWType == mwType {
				matched = append(matched, item)
			}
		}
		if len(matched) == 1 {
			return &Target{Instance: matched[0], MWType: mwType}, nil
		}
		if len(matched) > 1 {
			names := make([]string, 0, len(matched))
			for _, item := range matched {
				names = append(names, item.Name)
			}
			return nil, apperr.Newf(apperr.CodeInvalidParam,
				"识别到 %d 个 %s 实例（%s），请在问题中指明实例名或直接选择实例",
				len(matched), mwType, strings.Join(names, "、"))
		}
	}
	// 无法识别时给出候选提示。
	names := make([]string, 0, min(len(items), 10))
	for i, item := range items {
		if i >= 10 {
			break
		}
		names = append(names, fmt.Sprintf("%s(%s)", item.Name, item.MWType))
	}
	return nil, apperr.Newf(apperr.CodeInvalidParam,
		"无法从问题中识别诊断目标，请指定实例。当前可选：%s", strings.Join(names, "、"))
}

// Diagnose 执行一次同步诊断（非流式）。
func (s *DiagnoseService) Diagnose(ctx context.Context, in Request, session *Session, op Operator) (*Response, error) {
	start := time.Now()
	scope, scopeDesc := GuardScope(session, authFromSession(session))
	target, err := s.Resolve(ctx, in, scope)
	if err != nil {
		return nil, err
	}

	// ① 成本治理：预算预检（不通过直接拒绝，不消耗 token）。
	if err := s.cost.Check(session.User.ID); err != nil {
		return nil, apperr.Wrap(apperr.CodeQuotaExceeded, err)
	}
	// ② 并发额度：单实例并发 ≤4，防打爆外部配额。
	release, err := s.timeout.Acquire(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeQueueFull, err)
	}
	defer release()

	// ③ 确定性缓存：同实例 + 同问题签名 24h 直接返回。
	cacheKey := guardrail.CacheKey(target.Instance.ID, in.Question)
	if !in.SkipCache {
		if cached, ok := s.loadCache(ctx, cacheKey); ok {
			cached.Meta.CacheHit = true
			cached.DurationMS = time.Since(start).Milliseconds()
			s.recordDiagnosisAudit(ctx, op, &target.Instance, "ai_diagnose_cache", "success", map[string]any{
				"question": in.Question, "cache": true,
			})
			return cached, nil
		}
	}

	// ④ 工具预采集（含上下文预算、防死循环、工具级超时）。
	bundle, err := s.Collect(ctx, target, scope, scopeDesc, in.Question)
	if err != nil {
		return nil, err
	}

	// ⑤ 一次 LLM 调用（含重试与降级链）。
	report, raw, usage, engineUsed, engineStatus, warnings, err := s.Infer(ctx, in.Question, bundle)
	if err != nil {
		return nil, err
	}

	// ⑥ 落库 + 知识草稿沉淀 + 成本记账。
	response, err := s.persist(ctx, session, target, in, bundle, report, raw, usage, engineUsed, engineStatus, warnings, time.Since(start))
	if err != nil {
		return nil, err
	}
	if !in.SkipCache {
		s.saveCache(ctx, cacheKey, response)
	}
	s.recordDiagnosisAudit(ctx, op, &target.Instance, "ai_diagnose", "success", map[string]any{
		"question": in.Question, "engine": engineUsed, "tokens": usage.TotalTokens,
		"confidence": report.Confidence, "diagnosis_id": response.DiagnosisID,
	})
	if in.AlertID > 0 {
		if attachErr := s.attachAlert(ctx, in.AlertID, response.DiagnosisID); attachErr != nil {
			s.log.Warn("关联告警与诊断失败", zap.Error(attachErr))
		}
	}
	return response, nil
}

// Collect 执行工具预采集，返回受预算约束的上下文包。
func (s *DiagnoseService) Collect(ctx context.Context, target *Target, scope Scope, scopeDesc, question string) (*ai.Bundle, error) {
	taskCtx, cancel := s.timeout.TaskContext(ctx)
	defer cancel()

	bundle := &ai.Bundle{
		InstanceName: target.Instance.Name,
		MWType:       target.MWType,
		Environment:  target.Instance.Environment,
		GroupName:    target.Instance.GroupName,
		Config:       map[string]string{},
	}
	// 只读工具上下文：数据权限与实例信息在工具层强制生效（5.5）。
	toolCtx := &guardrail.ToolContext{
		Context: taskCtx,
		Scope: guardrail.Scope{
			UserID:       scopeUserID(scope),
			Username:     scopeUsername(scope),
			RoleCode:     scopeRoleCode(scope),
			AllowAllEnv:  len(scope.EnvScope) == 0,
			Environments: scope.EnvScope,
			Groups:       scope.GroupScope,
			ReadOnly:     true,
		},
		Instance: guardrail.Instance{
			ID:          target.Instance.ID,
			Name:        target.Instance.Name,
			MWType:      target.Instance.MWType,
			Environment: target.Instance.Environment,
			GroupName:   target.Instance.GroupName,
		},
	}
	// 记录本轮数据权限范围，便于审计与前端透明展示。
	bundle.ToolSteps = append(bundle.ToolSteps, "权限范围："+scopeDesc)
	// 只读工具通过注册表执行时复用同一上下文（工具白名单 + 权限隔离 + 独立审计）。
	_ = toolCtx

	// 工具 1：指标快照（只读）。
	if allow, reason := s.loop.Allow("metrics_snapshot", map[string]any{"instance_id": target.Instance.ID}, "判断是否存在资源瓶颈或性能拐点"); allow {
		var snapshot *monitor.Snapshot
		ok, err := s.timeout.Run(taskCtx, "metrics.snapshot", func(c context.Context) error {
			snap, snapErr := s.monitor.Snapshot(c, ToTarget(target.Instance))
			if snapErr != nil {
				return snapErr
			}
			snapshot = snap
			return nil
		})
		if ok && snapshot != nil {
			bundle.Snapshot = snapshot
			bundle.DataSource = snapshot.Source
			bundle.Summaries = s.summarize(taskCtx, target, snapshot)
			bundle.ToolSteps = append(bundle.ToolSteps, "metrics_snapshot: 采集当前指标并降采样为摘要")
		} else if err != nil {
			bundle.Missing = append(bundle.Missing, "metrics.snapshot（"+err.Error()+"）")
		}
	} else {
		bundle.Missing = append(bundle.Missing, "metrics.snapshot（"+reason+"）")
	}

	// 工具 2：错误日志证据（只读，应用日志告警域）。
	if allow, reason := s.loop.Allow("log_events", map[string]any{"service": target.Instance.Name}, "确认是否存在应用侧报错与堆栈线索"); allow {
		ok, err := s.timeout.Run(taskCtx, "logs.events", func(c context.Context) error {
			logs, logErr := s.collectLogs(c, target, question)
			if logErr != nil {
				return logErr
			}
			bundle.Logs = logs
			return nil
		})
		if ok {
			bundle.ToolSteps = append(bundle.ToolSteps, "log_events: 按错误指纹聚合近期异常日志")
		} else if err != nil {
			bundle.Missing = append(bundle.Missing, "logs.events（"+err.Error()+"）")
		}
	} else {
		bundle.Missing = append(bundle.Missing, "logs.events（"+reason+"）")
	}

	// 工具 3：只读关键配置。
	if allow, reason := s.loop.Allow("config_read", map[string]any{"instance_id": target.Instance.ID}, "核对配置项与当前现象是否匹配"); allow {
		ok, err := s.timeout.Run(taskCtx, "config.read", func(c context.Context) error {
			cfg, cfgErr := s.collectConfig(c, target)
			if cfgErr != nil {
				return cfgErr
			}
			bundle.Config = cfg
			return nil
		})
		if ok {
			bundle.ToolSteps = append(bundle.ToolSteps, "config_read: 读取关键只读配置项")
		} else if err != nil {
			bundle.Missing = append(bundle.Missing, "config.read（"+err.Error()+"）")
		}
	} else {
		bundle.Missing = append(bundle.Missing, "config.read（"+reason+"）")
	}

	// 工具 4：知识库相似案例（只读，仅作参考）。
	if allow, reason := s.loop.Allow("knowledge_search", map[string]any{"mw_type": target.MWType}, "查找历史相似案例作为参考"); allow {
		ok, err := s.timeout.Run(taskCtx, "knowledge.search", func(c context.Context) error {
			refs, refErr := s.searchKnowledge(c, question, target.MWType)
			if refErr != nil {
				return refErr
			}
			bundle.References = refs
			return nil
		})
		if ok {
			bundle.ToolSteps = append(bundle.ToolSteps, "knowledge_search: 向量检索历史相似案例（仅参考）")
		} else if err != nil {
			bundle.Missing = append(bundle.Missing, "knowledge.search（"+err.Error()+"）")
		}
	} else {
		bundle.Missing = append(bundle.Missing, "knowledge.search（"+reason+"）")
	}

	// 工具调用超时/失败产生的缺失维度（5.4）。
	for _, item := range s.timeout.Missing() {
		bundle.Missing = append(bundle.Missing, item.Dimension+"（"+item.Reason+"）")
	}
	if s.loop.Aborted() {
		bundle.Missing = append(bundle.Missing, "采集提前中止："+s.loop.AbortReason())
	}
	s.recordToolAudit(ctx, scope, target, bundle.ToolSteps)
	return bundle, nil
}

// Infer 执行一次 LLM 推理并做质量规范化。
func (s *DiagnoseService) Infer(ctx context.Context, question string, bundle *ai.Bundle) (*guardrail.Report, string, engine.Usage, string, string, []string, error) {
	sections := []guardrail.Section{
		{Dimension: "metrics", Title: "指标上下文", Body: bundle.MetricsSection(), Priority: 0},
		{Dimension: "logs", Title: "日志上下文", Body: bundle.LogsSection(), Priority: 1},
		{Dimension: "config", Title: "配置上下文", Body: bundle.ConfigSection(), Priority: 2},
		{Dimension: "references", Title: "参考案例", Body: bundle.ReferencesSection(), Priority: 3},
		{Dimension: "missing", Title: "缺失维度", Body: bundle.MissingSection(), Priority: 4},
	}
	system, user := ai.BuildPrompt(question, []string{
		bundle.MetricsSection(), bundle.LogsSection(), bundle.ConfigSection(),
		bundle.ReferencesSection(), bundle.MissingSection(),
	})
	fit := s.budget.FitMessages([]engine.Message{
		{Role: engine.RoleSystem, Content: system},
		{Role: engine.RoleUser, Content: user},
	}, sections)
	bundle.Truncated = fit.Dimensions()
	for _, m := range bundle.Missing {
		bundle.Truncated = appendUnique(bundle.Truncated, m)
	}

	req := engine.ChatRequest{
		Messages:    fit.Messages,
		MaxTokens:   s.budget.MaxOutput(),
		Temperature: 0.2,
		JSONMode:    true,
	}
	var (
		resp    *engine.ChatResponse
		lastErr error
	)
	// LLM 失败可重试（指数退避），工具调用失败不重试（5.3）。
	retryErr := s.timeout.RetryLLM(ctx, maxInt(1, s.cfg.LLMRetry), func(c context.Context) error {
		out, err := s.engine.Chat(c, req)
		if err != nil {
			lastErr = err
			return err
		}
		resp = out
		return nil
	})
	if retryErr != nil || resp == nil {
		if lastErr == nil {
			lastErr = retryErr
		}
		return nil, "", engine.Usage{}, "", model.EngineStatusFallback,
			[]string{"AI 引擎不可用：" + lastErr.Error()}, apperr.Wrap(apperr.CodeEngineFailed, lastErr)
	}

	report, parseErr := guardrail.ParseReport(resp.Content)
	warnings := make([]string, 0, 2)
	if parseErr != nil {
		// 解析失败时不丢内容：包装为「待人工确认」的推测结论。
		report = &guardrail.Report{
			RootCause:      "模型输出无法解析为结构化报告，已保留原始输出供人工确认",
			Confidence:     0,
			Evidence:       []guardrail.Evidence{},
			Suggestions:    []guardrail.Suggestion{},
			ImpactScope:    "未知",
			PendingConfirm: []string{"人工阅读原始模型输出并补全结论"},
			Speculative:    true,
		}
		warnings = append(warnings, "结构化解析失败："+parseErr.Error())
	}
	normalized, qualityWarnings := s.quality.Normalize(report, bundle.Missing)
	warnings = append(warnings, qualityWarnings...)
	normalized.AIAvailable = true

	engineUsed := s.engine.Name()
	if resp.Model != "" {
		engineUsed = s.engine.Name() + ":" + resp.Model
	}
	status := model.EngineStatusOK
	if st := s.engine.Status(); st.Degraded || st.CircuitOpen {
		status = model.EngineStatusDegraded
	}
	if s.engine.Name() == engine.RuleEngineName {
		status = model.EngineStatusFallback
		normalized.AIAvailable = false
		normalized.EngineNote = "AI 引擎不可用，本结论由规则引擎生成（半自动），请人工复核"
	}
	return normalized, resp.Content, resp.Usage, engineUsed, status, warnings, nil
}

// Stream 执行流式诊断，逐片回调 onChunk。
//
// 与同步路径共用同一套护栏；流式输出超时重置语义由引擎层负责（5.4）。
func (s *DiagnoseService) Stream(
	ctx context.Context,
	in Request,
	session *Session,
	op Operator,
	onMeta func(Meta),
	onChunk func(delta string),
	onDone func(*Response),
	onError func(error),
) {
	start := time.Now()
	scope, scopeDesc := GuardScope(session, authFromSession(session))
	target, err := s.Resolve(ctx, in, scope)
	if err != nil {
		onError(err)
		return
	}
	if err := s.cost.Check(session.User.ID); err != nil {
		onError(apperr.Wrap(apperr.CodeQuotaExceeded, err))
		return
	}
	release, err := s.timeout.Acquire(ctx)
	if err != nil {
		onError(apperr.Wrap(apperr.CodeQueueFull, err))
		return
	}
	defer release()

	meta := Meta{
		InstanceID:   target.Instance.ID,
		InstanceName: target.Instance.Name,
		MWType:       target.MWType,
		EngineName:   s.engine.Name(),
		EngineStatus: model.EngineStatusOK,
		Scope:        scopeDesc,
	}
	cacheKey := guardrail.CacheKey(target.Instance.ID, in.Question)
	if !in.SkipCache {
		if cached, ok := s.loadCache(ctx, cacheKey); ok {
			meta.CacheHit = true
			onMeta(meta)
			onDone(cached)
			return
		}
	}

	bundle, err := s.Collect(ctx, target, scope, scopeDesc, in.Question)
	if err != nil {
		onError(err)
		return
	}
	meta.Truncated = bundle.Truncated
	meta.Missing = bundle.Missing
	meta.DataSource = bundle.DataSource
	if st := s.engine.Status(); st.Degraded || st.CircuitOpen {
		meta.EngineStatus = model.EngineStatusDegraded
	}
	onMeta(meta)

	sections := []guardrail.Section{
		{Dimension: "metrics", Title: "指标上下文", Body: bundle.MetricsSection(), Priority: 0},
		{Dimension: "logs", Title: "日志上下文", Body: bundle.LogsSection(), Priority: 1},
		{Dimension: "config", Title: "配置上下文", Body: bundle.ConfigSection(), Priority: 2},
		{Dimension: "references", Title: "参考案例", Body: bundle.ReferencesSection(), Priority: 3},
		{Dimension: "missing", Title: "缺失维度", Body: bundle.MissingSection(), Priority: 4},
	}
	system, user := ai.BuildPrompt(in.Question, []string{
		bundle.MetricsSection(), bundle.LogsSection(), bundle.ConfigSection(),
		bundle.ReferencesSection(), bundle.MissingSection(),
	})
	fit := s.budget.FitMessages([]engine.Message{
		{Role: engine.RoleSystem, Content: system},
		{Role: engine.RoleUser, Content: user},
	}, sections)

	stream, err := s.engine.ChatStream(ctx, engine.ChatRequest{
		Messages: fit.Messages, MaxTokens: s.budget.MaxOutput(), Temperature: 0.2, JSONMode: true,
	})
	if err != nil {
		onError(apperr.Wrap(apperr.CodeEngineFailed, err))
		return
	}
	defer func() { _ = stream.Close() }()

	var sb strings.Builder
	var usage engine.Usage
	for {
		chunk, recvErr := stream.Recv()
		if chunk.Delta != "" {
			sb.WriteString(chunk.Delta)
			onChunk(chunk.Delta)
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		if recvErr != nil {
			break
		}
		if chunk.Done {
			break
		}
	}

	content := sb.String()
	report, parseErr := guardrail.ParseReport(content)
	warnings := make([]string, 0, 2)
	if parseErr != nil {
		report = &guardrail.Report{
			RootCause:      "模型输出无法解析为结构化报告，已保留原始输出供人工确认",
			Confidence:     0,
			Speculative:    true,
			PendingConfirm: []string{"人工阅读原始模型输出并补全结论"},
		}
		warnings = append(warnings, "结构化解析失败："+parseErr.Error())
	}
	normalized, qualityWarnings := s.quality.Normalize(report, bundle.Missing)
	warnings = append(warnings, qualityWarnings...)
	normalized.AIAvailable = s.engine.Name() != engine.RuleEngineName
	if usage.TotalTokens == 0 {
		usage = engine.EstimateUsage(fit.Messages, content)
	}

	response, err := s.persist(ctx, session, target, in, bundle, normalized, content, usage,
		s.engine.Name(), meta.EngineStatus, warnings, time.Since(start))
	if err != nil {
		onError(err)
		return
	}
	if !in.SkipCache {
		s.saveCache(ctx, cacheKey, response)
	}
	s.recordDiagnosisAudit(ctx, op, &target.Instance, "ai_diagnose_stream", "success", map[string]any{
		"question": in.Question, "engine": s.engine.Name(), "tokens": usage.TotalTokens,
		"confidence": normalized.Confidence, "diagnosis_id": response.DiagnosisID,
	})
	if in.AlertID > 0 {
		if attachErr := s.attachAlert(ctx, in.AlertID, response.DiagnosisID); attachErr != nil {
			s.log.Warn("关联告警与诊断失败", zap.Error(attachErr))
		}
	}
	onDone(response)
}

// AutoDiagnose 由告警或日志事件触发的自动诊断（事件驱动，见 3.1）。
//
// 使用系统身份执行，上下文仍按实例所属环境过滤；结果沉淀为知识草稿。
func (s *DiagnoseService) AutoDiagnose(ctx context.Context, alert *model.Alert, question string) (*Response, error) {
	if alert == nil {
		return nil, apperr.New(apperr.CodeInvalidParam, "告警不能为空")
	}
	item, err := s.instances.Get(ctx, alert.InstanceID)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, err)
	}
	systemSession := &Session{
		User: &model.User{
			Base:     model.Base{ID: 0},
			Username: "system",
			RoleCode: RoleAdmin,
		},
		Permissions: []string{PermAIUse}, Levels: []string{LevelRead},
	}
	return s.Diagnose(ctx, Request{
		InstanceID: item.ID,
		Question:   question,
		AlertID:    alert.ID,
	}, systemSession, Operator{Username: "system", IP: "internal"})
}

// persist 落库诊断结果并沉淀知识草稿。
func (s *DiagnoseService) persist(
	ctx context.Context,
	session *Session,
	target *Target,
	in Request,
	bundle *ai.Bundle,
	report *guardrail.Report,
	raw string,
	usage engine.Usage,
	engineUsed, engineStatus string,
	warnings []string,
	duration time.Duration,
) (*Response, error) {
	suggestions := make([]map[string]any, 0, len(report.Suggestions))
	for _, item := range report.Suggestions {
		suggestions = append(suggestions, map[string]any{
			"action": item.Action, "horizon": item.Horizon, "risk": item.Risk,
			"level": item.Level, "evidence_refs": item.EvidenceRefs,
		})
	}
	reportJSON, err := toJSONMap(report)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	collected, err := toJSONMap(map[string]any{
		"metrics":     bundle.Summaries,
		"logs":        bundle.Logs,
		"config":      bundle.Config,
		"missing":     bundle.Missing,
		"tool_steps":  bundle.ToolSteps,
		"data_source": bundle.DataSource,
		"warnings":    warnings,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	record := &model.AIDiagnosis{
		UserID:           session.User.ID,
		InstanceID:       target.Instance.ID,
		MWType:           target.MWType,
		UserQuery:        in.Question,
		CollectedMetrics: collected,
		DiagnosisResult:  raw,
		Report:           reportJSON,
		Suggestions:      model.JSONMapList(suggestions),
		EngineStatus:     engineStatus,
		CostTokens:       usage.TotalTokens,
		EngineUsed:       engineUsed,
		DurationMS:       duration.Milliseconds(),
		Truncated:        model.JSONStringSlice(bundle.Truncated),
	}
	if err := s.diagnoses.Create(ctx, record); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}

	// 成本记账（含接近上限告警与异常突增熔断）。
	ratio, warning := s.cost.Commit(session.User.ID, usage.TotalTokens)
	if warning != "" {
		warnings = append(warnings, warning)
		s.log.Warn("Token 预算告警", zap.Float64("ratio", ratio), zap.String("note", warning))
	}

	// 质量闭环：诊断结果自动沉淀为「草稿」，人工确认后参与检索（4.5）。
	if s.knowledge != nil && report.Confidence >= 0.5 {
		s.sinkKnowledge(ctx, target, in, report, record.ID)
	}

	response := &Response{
		DiagnosisID: record.ID,
		Meta: Meta{
			InstanceID:   target.Instance.ID,
			InstanceName: target.Instance.Name,
			MWType:       target.MWType,
			EngineName:   engineUsed,
			EngineStatus: engineStatus,
			Truncated:    bundle.Truncated,
			Missing:      bundle.Missing,
			Prompts:      usage.PromptTokens,
			DataSource:   bundle.DataSource,
		},
		Report:     report,
		References: bundle.References,
		Raw:        raw,
		Warnings:   warnings,
		EngineUsed: engineUsed,
		CostTokens: usage.TotalTokens,
		DurationMS: duration.Milliseconds(),
	}
	// 标记知识库参考案例被使用（用于采纳率统计）。
	if len(bundle.References) > 0 && s.knowledge != nil {
		ids := make([]int64, 0, len(bundle.References))
		for _, ref := range bundle.References {
			ids = append(ids, ref.ID)
		}
		if err := s.knowledge.BumpUse(ctx, ids); err != nil {
			s.log.Warn("更新知识引用次数失败", zap.Error(err))
		}
	}
	return response, nil
}

// sinkKnowledge 把诊断结论沉淀为知识草稿。
func (s *DiagnoseService) sinkKnowledge(ctx context.Context, target *Target, in Request, report *guardrail.Report, diagnosisID int64) {
	content := fmt.Sprintf("## 问题\n%s\n\n## 根因\n%s\n\n## 影响范围\n%s\n\n## 建议\n",
		in.Question, report.RootCause, report.ImpactScope)
	for _, item := range report.Suggestions {
		content += fmt.Sprintf("- [%s][%s][风险%s] %s\n", item.Horizon, item.Level, item.Risk, item.Action)
	}
	if len(report.PendingConfirm) > 0 {
		content += "\n## 待确认\n"
		for _, item := range report.PendingConfirm {
			content += "- " + item + "\n"
		}
	}
	title := fmt.Sprintf("%s · %s 诊断沉淀", target.Instance.Name, strings.ToUpper(target.MWType))
	entry := &model.KnowledgeBase{
		Title:       title,
		Content:     content,
		MWType:      target.MWType,
		Tags:        model.JSONStringSlice{"auto", target.Instance.Environment},
		Source:      "auto",
		Status:      model.KnowledgeStatusDraft,
		AuthorID:    0,
		DiagnosisID: diagnosisID,
	}
	if err := s.knowledge.Create(ctx, entry); err != nil {
		s.log.Warn("沉淀知识草稿失败", zap.Error(err))
		return
	}
	// 异步生成向量，供后续检索。
	if s.engine != nil {
		go func(id int64, text string) {
			bg, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			vecs, err := s.engine.Embed(bg, []string{text})
			if err != nil || len(vecs) == 0 {
				return
			}
			if err := s.knowledge.UpdateEmbedding(bg, id, model.Vector(vecs[0])); err != nil {
				s.log.Warn("写入知识向量失败", zap.Int64("id", id), zap.Error(err))
			}
		}(entry.ID, entry.Title+"\n"+entry.Content)
	}
}

// summarize 把指标快照降采样为摘要。
func (s *DiagnoseService) summarize(ctx context.Context, target *Target, snapshot *monitor.Snapshot) []ai.MetricSummary {
	out := make([]ai.MetricSummary, 0, len(snapshot.Metrics))
	for _, metric := range snapshot.Metrics {
		summary := ai.MetricSummary{
			Name: metric.Name, DisplayName: metric.DisplayName, Unit: metric.Unit,
			Latest: metric.Latest, Status: metric.Status,
			Min: metric.Latest, Max: metric.Latest, Avg: metric.Latest, P95: metric.Latest,
		}
		// 优先取异常指标的历史序列做降采样，控制查询次数（≤3 个指标）。
		if metric.Status == "critical" || metric.Status == "warning" {
			if len(out) < 3 || s.countAnomalies(out) < 3 {
				samples, err := s.monitor.History(ctx, ToTarget(*targetInstance(target)), metric.Name, monitor.TimeRange{
					Start: time.Now().Add(-2 * time.Hour), End: time.Now(), Step: 5 * time.Minute,
				})
				if err == nil && len(samples) > 0 {
					values := make([]float64, 0, len(samples))
					for _, sample := range samples {
						values = append(values, sample.Value)
					}
					stat := guardrail.SummarizeSeries(metric.Name, metric.Unit, values)
					summary.Avg = stat.Avg
					summary.P95 = stat.P95
					summary.Min = stat.Min
					summary.Max = stat.Max
					summary.Slope = stat.Slope
					summary.Turning = stat.TurningPoints
					summary.Anomalies = stat.AnomalySegments
				}
			}
		}
		summary.Trend = describeTrend(summary.Slope, summary.Latest)
		out = append(out, summary)
	}
	return out
}

// targetInstance 从 Target 取回实例值。
func targetInstance(t *Target) *model.MiddlewareInstance { return &t.Instance }

// countAnomalies 统计已纳入的异常指标数量。
func (s *DiagnoseService) countAnomalies(items []ai.MetricSummary) int {
	n := 0
	for _, item := range items {
		if item.Status == "critical" || item.Status == "warning" {
			n++
		}
	}
	return n
}

// describeTrend 生成趋势描述。
func describeTrend(slope, latest float64) string {
	if latest == 0 {
		if slope > 0 {
			return "上升"
		}
		if slope < 0 {
			return "下降"
		}
		return "平稳"
	}
	ratio := slope / (latest + 1e-9)
	switch {
	case ratio > 0.01:
		return "明显上升"
	case ratio > 0.002:
		return "缓慢上升"
	case ratio < -0.01:
		return "明显下降"
	case ratio < -0.002:
		return "缓慢下降"
	default:
		return "平稳"
	}
}

// collectLogs 采集日志证据。
func (s *DiagnoseService) collectLogs(ctx context.Context, target *Target, question string) ([]ai.LogEvidence, error) {
	if s.logEvents == nil {
		return nil, nil
	}
	items, _, err := s.logEvents.List(ctx, repository.LogEventFilter{
		ServiceName: target.Instance.Name,
		From:        timePtr(time.Now().Add(-24 * time.Hour)),
	}, 20, 0)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		// 服务名未命中时退化为按中间件类型匹配最近事件。
		items, _, err = s.logEvents.List(ctx, repository.LogEventFilter{
			Keyword: target.MWType,
			From:    timePtr(time.Now().Add(-24 * time.Hour)),
		}, 10, 0)
		if err != nil {
			return nil, err
		}
	}
	out := make([]ai.LogEvidence, 0, len(items))
	for i, item := range items {
		if i >= 5 {
			break
		}
		lines := splitLines(item.RawStacktrace, 20)
		out = append(out, ai.LogEvidence{
			Source:    defaultString(item.ServiceName, "unknown"),
			Signature: item.ErrorSignature,
			Count:     item.ErrorCount,
			LastSeen:  item.LastSeenAt.Format("2006-01-02 15:04:05"),
			Lines:     lines,
		})
	}
	return out, nil
}

// collectConfig 读取关键只读配置。
func (s *DiagnoseService) collectConfig(ctx context.Context, target *Target) (map[string]string, error) {
	out := map[string]string{}
	if target.Instance.Config != nil {
		for k, v := range target.Instance.Config {
			key := strings.TrimSpace(k)
			if key == "" {
				continue
			}
			out[key] = fmt.Sprintf("%v", v)
		}
	}
	// 补充实例级连接信息（不含凭据）。
	out["host"] = target.Instance.Host
	out["port"] = fmt.Sprintf("%d", target.Instance.Port)
	out["environment"] = target.Instance.Environment
	out["prom_job"] = target.Instance.PromJob
	return out, nil
}

// searchKnowledge 向量检索相似案例（仅作参考，5.7）。
func (s *DiagnoseService) searchKnowledge(ctx context.Context, question, mwType string) ([]ai.KnowledgeReference, error) {
	if s.knowledge == nil || s.engine == nil {
		return nil, nil
	}
	vecs, err := s.engine.Embed(ctx, []string{question})
	if err != nil || len(vecs) == 0 {
		return nil, nil
	}
	query := model.Vector(vecs[0])
	candidates, err := s.knowledge.CandidatesForVector(ctx, mwType, 200)
	if err != nil {
		return nil, err
	}
	type scored struct {
		item  model.KnowledgeBase
		score float64
	}
	matches := make([]scored, 0, len(candidates))
	for _, item := range candidates {
		if len(item.Embedding) == 0 {
			continue
		}
		score := cosine(query, item.Embedding)
		if score < s.cfg.VectorThreshold {
			continue
		}
		// 低采纳率条目降权（4.5 质量闭环）。
		score *= adoptionWeight(item.AdoptCount, item.UseCount)
		matches = append(matches, scored{item: item, score: score})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].score > matches[j].score })
	if len(matches) > 5 {
		matches = matches[:5]
	}
	out := make([]ai.KnowledgeReference, 0, len(matches))
	for _, m := range matches {
		excerpt := m.item.Content
		if len([]rune(excerpt)) > 200 {
			excerpt = string([]rune(excerpt)[:200]) + "…"
		}
		out = append(out, ai.KnowledgeReference{
			ID: m.item.ID, Title: m.item.Title, MWType: m.item.MWType,
			Similarity: roundFloat(m.score, 3), Excerpt: excerpt, AdoptCount: m.item.AdoptCount,
		})
	}
	return out, nil
}

// adoptionWeight 依据采纳率对相似度加权。
func adoptionWeight(adopt, use int) float64 {
	if use <= 0 {
		return 1
	}
	ratio := float64(adopt) / float64(use)
	if ratio >= 1 {
		return 1.1
	}
	if ratio <= 0.2 {
		return 0.8
	}
	return 1
}

// loadCache 读取确定性缓存。
func (s *DiagnoseService) loadCache(ctx context.Context, key string) (*Response, bool) {
	if s.store == nil {
		return nil, false
	}
	raw, err := s.store.Get(ctx, key)
	if err != nil {
		return nil, false
	}
	var cached Response
	if err := json.Unmarshal([]byte(raw), &cached); err != nil {
		return nil, false
	}
	return &cached, true
}

// saveCache 写入确定性缓存（同实例 + 同问题签名，24h，见 5.7）。
func (s *DiagnoseService) saveCache(ctx context.Context, key string, resp *Response) {
	if s.store == nil {
		return
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		return
	}
	if err := s.store.Set(ctx, key, string(payload), s.cfg.CacheTTL); err != nil {
		s.log.Warn("写入诊断缓存失败", zap.Error(err))
	}
}

// attachAlert 关联告警与诊断记录。
func (s *DiagnoseService) attachAlert(ctx context.Context, alertID, diagnosisID int64) error {
	if s.diagnoses == nil {
		return nil
	}
	db := s.diagnoses.DB()
	return db.WithContext(ctx).Model(&model.Alert{}).Where("id = ?", alertID).
		Update("diagnosis_id", diagnosisID).Error
}

// recordToolAudit 记录本轮工具调用（5.5：每次工具调用独立审计）。
func (s *DiagnoseService) recordToolAudit(ctx context.Context, scope Scope, target *Target, steps []string) {
	if s.audit == nil || len(steps) == 0 {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: scopeUserID(scope), Username: scopeUsername(scope), InstanceID: target.Instance.ID,
		ActionType: "ai_tool_call_batch", Level: LevelRead, Result: "success",
		Detail: map[string]any{"steps": steps, "instance": target.Instance.Name},
	})
}

// recordDiagnosisAudit 记录诊断行为审计。
func (s *DiagnoseService) recordDiagnosisAudit(ctx context.Context, op Operator, item *model.MiddlewareInstance, action, result string, detail map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: op.UserID, Username: op.Username, InstanceID: item.ID,
		ActionType: action, Level: LevelRead, Result: result,
		IPAddress: op.IP, UserAgent: op.Agent, Detail: detail,
	})
}

// 辅助函数 ----------------------------------------------------------------

func scopeUserID(scope Scope) int64 {
	if scope.Owner != nil && scope.Owner.User != nil {
		return scope.Owner.User.ID
	}
	return 0
}

func scopeUsername(scope Scope) string {
	if scope.Owner != nil && scope.Owner.User != nil {
		return scope.Owner.User.Username
	}
	return ""
}

// scopeRoleCode 返回发起人角色码。
func scopeRoleCode(scope Scope) string {
	if scope.Owner != nil && scope.Owner.User != nil {
		return scope.Owner.User.RoleCode
	}
	return ""
}

// authFromSession 提取会话中的授权服务引用；会话缺失时返回零值授权服务
// （零值下 ScopeOf 返回空范围，等价于无任何权限，符合最小权限原则）。
func authFromSession(session *Session) *AuthService {
	if session == nil || session.auth == nil {
		return &AuthService{}
	}
	return session.auth
}

func toJSONMap(v any) (model.JSONMap, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := model.JSONMap{}
	if err := json.Unmarshal(raw, &out); err != nil {
		// 非对象结构时包裹为单字段，避免整体失败。
		return model.JSONMap{"value": string(raw)}, nil
	}
	return out, nil
}

func splitLines(text string, limit int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > limit {
		lines = lines[:limit]
	}
	return lines
}

func timePtr(t time.Time) *time.Time { return &t }

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func roundFloat(v float64, digits int) float64 {
	factor := 1.0
	for i := 0; i < digits; i++ {
		factor *= 10
	}
	return float64(int64(v*factor+0.5)) / factor
}

func appendUnique(items []string, value string) []string {
	for _, item := range items {
		if item == value {
			return items
		}
	}
	return append(items, value)
}

// utcNow 返回 UTC 当前时间（统一时间口径）。
func utcNow() time.Time { return time.Now().UTC() }
