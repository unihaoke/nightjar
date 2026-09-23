package service

import (
	"context"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// LogAlertWorker 是日志告警的**后处理编排**：把一条已落库的日志事件推进到
// 「AI 分析 + 外发通知」两件事做完。
//
// 为什么不让 Ingest 直接做（同步）：AI 分析是分钟级的慢操作，放在采集路径上会把
// Kafka 消费拖垮（消费者一慢，位点就积压，整条日志链路跟着延迟）。
//
// AI 分析改成异步后，一条事件的处理被拆成两半：
//  1. 提交：RunOnce 扫到 pending → 把问题提交给 AI 服务 → 事件置 awaiting（球在对方场地）；
//  2. 收尾：AI 服务回调（或轮询查到结论）→ 写结论 → 外发通知 → 事件置 done/failed。
//
// 两半都靠库里的状态推进，因此进程重启、多副本都不会丢：awaiting 的事件
// 由轮询兜底（回调丢失时）与超时收尾（AI 服务彻底没回音时）保证最终有结论。
//
// 并发安全：用 repository.ClaimForAnalysis 的原子更新抢占（pending→running），
// 抢不到就安静跳过；进程内再加一把 busy 开关，避免上一轮没跑完就叠下一轮。
type LogAlertWorker struct {
	cfg      *config.Config
	events   *repository.LogEventRepository
	rules    *repository.LogAlertRuleRepository
	notifier *NotifierService
	analysis *CodeAnalysisService
	log      *zap.Logger

	busy atomic.Bool
}

// LogAlertWorkerDeps 是构造参数。
type LogAlertWorkerDeps struct {
	Config   *config.Config
	Events   *repository.LogEventRepository
	Rules    *repository.LogAlertRuleRepository
	Notifier *NotifierService
	Analysis *CodeAnalysisService
	Log      *zap.Logger
}

// NewLogAlertWorker 构造后处理器。
func NewLogAlertWorker(d LogAlertWorkerDeps) *LogAlertWorker {
	if d.Log == nil {
		d.Log = zap.NewNop()
	}
	return &LogAlertWorker{
		cfg: d.Config, events: d.Events, rules: d.Rules,
		notifier: d.Notifier, analysis: d.Analysis, log: d.Log,
	}
}

// systemOperator 是自动分析的操作者（没有真人点击，但仍要能写审计）。
//
// UserID 刻意留 0：Submit 用 UserID>0 判断"是否计入某个人的配额"，
// 自动分析不该记到某个人头上（否则他莫名其妙被限额）。
var systemOperator = Operator{Username: "system"}

// RunOnce 处理一批待处理事件（由定时任务调用）。返回本轮处理条数。
func (w *LogAlertWorker) RunOnce(ctx context.Context) (int, error) {
	if w == nil {
		return 0, nil
	}
	// 上一轮还在跑就跳过：AI 分析是慢操作，叠加执行只会把 AI 服务与 CPU 一起打满。
	if !w.busy.CompareAndSwap(false, true) {
		return 0, nil
	}
	defer w.busy.Store(false)

	batch, interval := 10, 15
	if w.cfg != nil {
		if w.cfg.LogAlert.WorkerBatch > 0 {
			batch = w.cfg.LogAlert.WorkerBatch
		}
		if w.cfg.LogAlert.WorkerInterval > 0 {
			interval = w.cfg.LogAlert.WorkerInterval
		}
	}
	items, err := w.events.ListPendingAnalysis(ctx, batch)
	if err != nil {
		return 0, err
	}
	processed := 0
	for i := range items {
		if ctx.Err() != nil {
			break
		}
		if err := w.ProcessEvent(ctx, items[i].ID); err != nil {
			w.log.Warn("日志告警后处理失败",
				zap.Int64("event_id", items[i].ID), zap.Error(err))
		}
		processed++
		// 同一轮内稍微错开：提交是快操作，但风暴期也不该一瞬间打出上百个任务。
		if i < len(items)-1 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(interval) * time.Millisecond):
			}
		}
	}
	return processed, nil
}

// LogAnalysisBrief 是随通知一起外发的 AI 结论摘要。
//
// 边界：只带结论与建议，不带可执行动作（高危操作必须回平台走审批，同 6.2）。
type LogAnalysisBrief struct {
	LocatedFile   string  `json:"located_file"`
	LocatedLine   int     `json:"located_line"`
	RootCause     string  `json:"root_cause"`
	FixSuggestion string  `json:"fix_suggestion"`
	ImpactScope   string  `json:"impact_scope"`
	Confidence    float64 `json:"confidence"`
	EngineUsed    string  `json:"engine_used"`
	// ReportURL 为外部 AI 服务给出的完整报告地址（开放接口才有）。
	// 卡片里渲染成「查看完整报告」按钮：分析结论在 IM 里只能给摘要，
	// 完整报告（含补丁与验证结果）必须能一键跳过去看。
	ReportURL string `json:"report_url"`
	// Markdown 为 AI 服务产出的完整报告 Markdown（可能很长，展示时按渠道截断）。
	Markdown string `json:"markdown"`
	// Degraded 表示结论来自规则引擎降级（AI 不可用），提醒使用者人工复核。
	Degraded bool `json:"degraded"`
}

// ProcessEvent 提交一条事件的分析（第二半在 CompleteTask / PollTasks 里收尾）。
//
// 顺序与产品要求一致：**先分析、再通知**，让消息里带上"根因是什么、怎么改"，
// 收消息的人才能直接判断要不要立刻处理。代价是通知会被分析拖后——
// 所以有超时兜底（ai_analysis.task_timeout），且**拿不到结论也会通知**（带上原因）。
func (w *LogAlertWorker) ProcessEvent(ctx context.Context, eventID int64) error {
	claimed, err := w.events.ClaimForAnalysis(ctx, eventID)
	if err != nil {
		return err
	}
	if !claimed {
		// 已被别的实例/别的入口拿走，或者状态不是 pending（例如冷却抑制）。
		return nil
	}
	event, err := w.events.Get(ctx, eventID)
	if err != nil {
		return err
	}
	rule := w.ruleOf(ctx, *event)

	// ① 先看"该不该问 AI"：规则没开、平台没配 AI 服务 → 明确置为 disabled 并通知。
	if state, reason := analysisDecision(rule, event.ServiceName, w.analysis != nil && w.analysis.Ready()); state != "" {
		return w.finish(ctx, *event, rule, &Completion{
			EventID:     eventID,
			ServiceName: event.ServiceName,
			Failed:      state == model.LogAnalysisFailed,
			Reason:      reason,
		}, state)
	}

	// ② 提交问题给外部 AI 服务（脱敏与合规闸门都在 Submit 里）。
	outcome, err := w.analysis.Submit(ctx, CodeAnalysisRequest{EventID: eventID}, systemOperator)
	if err != nil {
		// 合规阻断不是故障：它是明确的配置结果，页面上不该显示"分析失败"。
		state := model.LogAnalysisFailed
		if e := apperr.From(err); e != nil && e.Code == apperr.CodeOutboundDenied {
			state = model.LogAnalysisDisabled
		}
		w.log.Warn("提交 AI 分析失败", zap.Int64("event_id", eventID),
			zap.String("service", event.ServiceName), zap.Error(err))
		return w.finish(ctx, *event, rule, &Completion{
			EventID:     eventID,
			ServiceName: event.ServiceName,
			Failed:      true,
			Reason:      err.Error(),
		}, state)
	}

	// ③ 同步模式：结论已经回来了，直接收尾（通知 + done）。
	if !outcome.Async {
		return w.finish(ctx, *event, rule, &Completion{
			EventID:     eventID,
			ServiceName: event.ServiceName,
			TaskID:      outcome.TaskID,
			Brief:       outcome.Brief,
		}, "")
	}

	// ④ 异步模式：事件停在 awaiting，等回调或轮询把结论带回来。
	state := model.LogAnalysisAwaiting
	if err := w.events.SetAnalysisState(ctx, eventID, state,
		truncateText("已提交给 AI 分析服务（task_id="+outcome.TaskID+"），等待结论", 500)); err != nil {
		return err
	}
	// 告警本身要快：配置了"提交即通知"就先发一条不带结论的告警，
	// 结论到达后再发一条带结论的（默认关闭，避免一条告警变两条消息）。
	if w.notifyOnSubmit() {
		if notifyErr := w.notify(ctx, *event, rule, nil); notifyErr != nil {
			w.log.Warn("提交后立即通知失败", zap.Int64("event_id", eventID), zap.Error(notifyErr))
		}
	}
	w.log.Info("日志告警已提交 AI 分析", zap.Int64("event_id", eventID),
		zap.String("service", event.ServiceName), zap.String("task_id", outcome.TaskID))
	return nil
}

// CompleteTask 是**回调入口**：AI 服务把结论送回来时调用。
//
// status 取 succeeded / failed；answer 为结论原文；errMsg 为 AI 侧的失败原因。
func (w *LogAlertWorker) CompleteTask(ctx context.Context, taskID, runID, status, answer, errMsg string) error {
	if w == nil || w.analysis == nil {
		return apperr.New(apperr.CodeInternal, "AI 分析能力未装配")
	}
	completion, err := w.analysis.CompleteByTask(ctx, taskID, runID, status, answer, errMsg)
	if err != nil {
		return err
	}
	return w.finishByCompletion(ctx, completion)
}

// PollTasks 轮询兜底：查一次仍在等待的任务，并把超时的收尾。
//
// 为什么必须有它：回调依赖 AI 服务能访问到平台、也依赖它不丢消息。
// 只有"回调 + 轮询"两条路都在，丢一次回调才不会让告警永远停在分析中。
func (w *LogAlertWorker) PollTasks(ctx context.Context) (int, error) {
	if w == nil || w.analysis == nil {
		return 0, nil
	}
	limit := 20
	if w.cfg != nil && w.cfg.AIAnalysis.PollBatch > 0 {
		limit = w.cfg.AIAnalysis.PollBatch
	}
	tasks, err := w.analysis.PendingTasks(ctx, limit)
	if err != nil {
		return 0, err
	}
	done := 0
	for i := range tasks {
		if ctx.Err() != nil {
			break
		}
		completion, finished, err := w.analysis.RefreshTask(ctx, tasks[i])
		if err != nil {
			// 单次查询失败只记日志：下一轮还会再试，不该让整轮轮询挂掉。
			w.log.Warn("查询 AI 分析任务失败", zap.String("task_id", tasks[i].TaskID), zap.Error(err))
			continue
		}
		if !finished || completion == nil {
			continue
		}
		if err := w.finishByCompletion(ctx, completion); err != nil {
			w.log.Warn("分析结论收尾失败", zap.String("task_id", tasks[i].TaskID), zap.Error(err))
			continue
		}
		done++
	}
	// 超时收尾：超过 task_timeout 仍未有结论的任务按超时处理并通知。
	timedOut, err := w.analysis.TimeoutTasks(ctx, time.Now().UTC(), limit)
	if err != nil {
		w.log.Warn("扫描超时分析任务失败", zap.Error(err))
	}
	for _, completion := range timedOut {
		if err := w.finishByCompletion(ctx, completion); err != nil {
			w.log.Warn("超时任务收尾失败", zap.String("task_id", completion.TaskID), zap.Error(err))
			continue
		}
		done++
	}
	return done, nil
}

// finishByCompletion 按 completion 里的事件号取出事件与规则，再走统一的收尾。
func (w *LogAlertWorker) finishByCompletion(ctx context.Context, completion *Completion) error {
	if completion == nil || completion.EventID <= 0 {
		// 手工提交的分析没有关联事件：不需要通知，结论已经在库里了。
		return nil
	}
	event, err := w.events.Get(ctx, completion.EventID)
	if err != nil {
		return err
	}
	return w.finish(ctx, *event, w.ruleOf(ctx, *event), completion, "")
}

// finish 是唯一的收尾出口：通知 + 落状态。
//
// state 为空时按 completion 推导（有结论 → done；没有 → failed）。
func (w *LogAlertWorker) finish(ctx context.Context, event model.LogAlertEvent, rule model.LogAlertRule,
	completion *Completion, state string) error {
	var brief *LogAnalysisBrief
	if completion != nil {
		brief = completion.Brief
	}
	// 已经投递过、且这次没有新结论（分析失败/超时/被跳过）：不再重复投递。
	// 通知的意义是"把结论送到人手里"；已经发过一次告警、又没有新信息时再发一条，
	// 只会把一次失败的分析放大成两倍的打扰（"提交即通知"场景尤其明显）。
	// 有结论时照发：那才是"带根因的告警"的价值所在。
	var notifyErr error
	notified := false
	if event.NotifiedAt != nil && brief == nil {
		w.log.Info("已投递过通知且本次没有新结论，跳过重复投递",
			zap.Int64("event_id", event.ID), zap.String("service", event.ServiceName))
	} else {
		notifyErr = w.notify(ctx, event, rule, brief)
		notified = notifyErr == nil
	}

	finalState := state
	reason := ""
	if completion != nil {
		reason = completion.Reason
	}
	if finalState == "" {
		if brief != nil {
			finalState = model.LogAnalysisDone
		} else {
			finalState = model.LogAnalysisFailed
		}
	}
	finalReason := reason + notifySuffix(notifyErr)
	if err := w.events.SetAnalysisState(ctx, event.ID, finalState, truncateText(finalReason, 500)); err != nil {
		return err
	}
	w.log.Info("日志告警后处理完成",
		zap.Int64("event_id", event.ID), zap.String("service", event.ServiceName),
		zap.String("state", finalState), zap.Bool("notified", notified),
		zap.Bool("has_analysis", brief != nil))
	return nil
}

// notify 外发通知并把结论带上。
//
// 通知失败**不阻断**状态落库：分析结论在平台上照样有价值，不该因为 IM 网关抖动一起丢掉；
// 失败原因会由调用方拼进 analysis_error（notifySuffix），页面上看得见。
func (w *LogAlertWorker) notify(ctx context.Context, event model.LogAlertEvent, rule model.LogAlertRule, brief *LogAnalysisBrief) error {
	if w.notifier == nil {
		return apperr.New(apperr.CodeInternal, "通知服务未装配")
	}
	if event.Suppressed {
		w.log.Debug("日志告警处于冷却抑制中，跳过通知",
			zap.Int64("event_id", event.ID), zap.String("service", event.ServiceName))
		return apperr.New(apperr.CodeInvalidParam, "冷却期内抑制")
	}
	channels := []string(rule.NotifyChannels)
	if err := w.notifier.NotifyLogEvent(ctx, event, rule, channels, brief); err != nil {
		w.log.Warn("日志告警通知失败", zap.Int64("event_id", event.ID), zap.Error(err))
		return err
	}
	if err := w.events.MarkNotified(ctx, event.ID, time.Now().UTC()); err != nil {
		w.log.Warn("标记已通知失败", zap.Error(err))
	}
	return nil
}

// analysisDecision 决定"这条事件在 AI 分析这一步该怎么收场"，并给出给使用者看的原因。
//
// 抽成纯函数的原因：这两个判断（规则开关 / 平台能力）是整条链路里最容易配错的，
// 而它们的结果直接决定页面上显示"未启用"还是"分析失败"——必须能被单测逐条钉住。
// 返回 state 为空表示"可以继续分析"。
func analysisDecision(rule model.LogAlertRule, service string, analysisReady bool) (state, reason string) {
	if !rule.AIEnabled {
		return model.LogAnalysisDisabled,
			"该规则未启用 AI 分析（如需结论：把规则的「AI 分析」打开，或对该条点「重新分析」）"
	}
	if !analysisReady {
		return model.LogAnalysisDisabled,
			"平台未装配 AI 分析能力（请在「AI 设置 → AI 代码分析」中启用并填写服务地址）"
	}
	_ = service
	return "", ""
}

// notifyOnSubmit 是否在提交后立刻发一条（不带结论的）告警通知。
func (w *LogAlertWorker) notifyOnSubmit() bool {
	return w.cfg != nil && w.cfg.AIAnalysis.NotifyOnSubmit
}

// ruleOf 取出事件命中的规则。
//
// 事件没有 rule_id 只可能是规则化之前的**历史数据**（现在没命中规则的日志根本不会入库）：
// 给一条"零窗口零冷却、允许 AI"的最小规则，让它们仍能被通知与分析一次，
// 而不是回落任何平台默认参数——默认值已按产品要求彻底移除。
func (w *LogAlertWorker) ruleOf(ctx context.Context, event model.LogAlertEvent) model.LogAlertRule {
	if event.RuleID > 0 && w.rules != nil {
		if rule, err := w.rules.Get(ctx, event.RuleID); err == nil && rule != nil {
			return *rule
		}
	}
	return model.LogAlertRule{Name: "（历史事件）", AIEnabled: true, Enabled: true}
}

// notifySuffix 把通知失败的原因拼进分析状态的说明里（没有失败时返回空串）。
//
// 为什么两者要放在同一个字段：页面上"分析状态"是使用者唯一会看的结论位。
// 若分析成功就把通知失败覆盖掉，会出现"显示已分析、但谁都没收到通知"且无处解释的情况。
func notifySuffix(notifyErr error) string {
	if notifyErr == nil {
		return ""
	}
	return "｜通知未送达：" + truncateText(notifyErr.Error(), 200)
}
