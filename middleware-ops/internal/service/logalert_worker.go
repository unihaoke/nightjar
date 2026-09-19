package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// LogAlertWorker 是日志告警的**后处理编排**：把一条已落库的日志事件推进到
// 「通知渠道 + AI 代码分析」两件事做完。
//
// 为什么不让 Ingest 直接做（同步）：
//   - AI 分析要拉代码、要调 LLM，耗时几十秒，放在采集路径上会把 Kafka 消费拖垮
//     （消费者一慢，位点就积压，整条日志链路跟着延迟）；
//   - 进程重启时"还没处理的事件"必须不丢——所以状态落在 DB 的 analysis_state 上，
//     由定时任务按队列扫描重试，而不是靠内存里的 goroutine。
//
// 并发安全：多副本部署时用 repository.ClaimForAnalysis 的原子更新抢占（pending→running），
// 抢不到就安静跳过；进程内再加一把 busy 开关，避免上一轮没跑完就叠下一轮。
type LogAlertWorker struct {
	cfg       *config.Config
	events    *repository.LogEventRepository
	rules     *repository.LogAlertRuleRepository
	codeRepos *repository.CodeRepoRepository
	notifier  *NotifierService
	analysis  *CodeAnalysisService
	fetcher   RepoFetcher
	log       *zap.Logger

	busy atomic.Bool
	// repoRefreshedAt 记录每个服务最近一次拉代码的时间，用于"最小拉取间隔"：
	// 一次故障风暴里同服务的多条事件不应该每条都去 pull 一次远端。
	repoRefreshedAt sync.Map
}

// RepoFetcher 是代码仓库缓存的最小依赖面（由 internal/repo 的 Fetcher 适配而来）。
//
// 刻意不直接依赖 internal/repo 的具体类型：服务层只需要"给我一份最新的代码"这件事，
// 换成别的实现（比如本地镜像、对象存储解包）时不需要动这里。
type RepoFetcher interface {
	Ensure(ctx context.Context, req RepoFetchRequest) (RepoFetchResult, error)
}

// RepoFetchRequest / RepoFetchResult 是跨层的数据形状（与 internal/repo 的 Request/Result 对应）。
//
// 只描述"这次要哪个服务的哪条分支"：缓存根目录、git 路径、超时都是进程级配置，
// 在容器装配时一次性交给 Fetcher（见 repo_adapter.go）。
type RepoFetchRequest struct {
	Service       string
	RepoURL       string
	Branch        string
	LocalPath     string
	AllowOutbound bool
}

type RepoFetchResult struct {
	LocalPath string
	Action    string
	Revision  string
	Branch    string
}

// LogAlertWorkerDeps 是构造参数。
type LogAlertWorkerDeps struct {
	Config    *config.Config
	Events    *repository.LogEventRepository
	Rules     *repository.LogAlertRuleRepository
	CodeRepos *repository.CodeRepoRepository
	Notifier  *NotifierService
	Analysis  *CodeAnalysisService
	Fetcher   RepoFetcher
	Log       *zap.Logger
}

// NewLogAlertWorker 构造后处理器。
func NewLogAlertWorker(d LogAlertWorkerDeps) *LogAlertWorker {
	return &LogAlertWorker{
		cfg: d.Config, events: d.Events, rules: d.Rules, codeRepos: d.CodeRepos,
		notifier: d.Notifier, analysis: d.Analysis, fetcher: d.Fetcher, log: d.Log,
	}
}

// systemOperator 是自动分析的操作者（没有真人点击，但仍要能写审计）。
//
// UserID 刻意留 0：Analyze 用 UserID>0 判断"是否计入某个人的配额"，
// 自动分析不该记到某个人头上（否则他莫名其妙被限额）。
var systemOperator = Operator{Username: "system"}

// RunOnce 处理一批待处理事件（由定时任务调用）。返回本轮处理条数。
func (w *LogAlertWorker) RunOnce(ctx context.Context) (int, error) {
	if w == nil {
		return 0, nil
	}
	// 上一轮还在跑就跳过：AI 分析是慢操作，叠加执行只会把 LLM 配额和 CPU 一起打满。
	if !w.busy.CompareAndSwap(false, true) {
		return 0, nil
	}
	defer w.busy.Store(false)

	batch := 10
	interval := 15
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
		// 同一轮内稍微错开，避免把 LLM 与代码托管瞬间打满。
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
	// Degraded 表示结论来自规则引擎降级（AI 不可用），提醒使用者人工复核。
	Degraded bool `json:"degraded"`
}

// ProcessEvent 处理单条事件，顺序与产品要求一致：
//
//	拉取代码 + AI 定位分析 →（拿到结论）→ 外发通知渠道
//
// 为什么不是"先通知再分析"：告警消息里带上"是哪一行代码、根因是什么、怎么改"，
// 收消息的人才能直接判断要不要立刻处理；否则还得回平台点一次。
// 代价是通知会被分析拖后——所以分析有硬超时（默认 2 分钟），且**分析失败同样会通知**
// （带上失败原因），不会因为 AI 不可用就没人知道出事了。
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

	// ① AI 代码分析（拉代码 → 定位 → 三点式结论）
	brief, state, reason := w.analyze(ctx, *event, rule)

	// ② 外发通知（带上①的结论；抑制中的事件不通知——那正是冷却期的意义）
	notifyErr := w.notify(ctx, *event, rule, brief)

	// ③ 落状态：分析没产出结论时用 analyze 给的状态，并保留通知失败的原因
	finalState, finalReason := state, reason
	if finalState == "" {
		finalState = model.LogAnalysisDone
	}
	finalReason += notifySuffix(notifyErr)
	if err := w.events.SetAnalysisState(ctx, eventID, finalState,
		truncateText(finalReason, 500)); err != nil {
		return err
	}
	w.log.Info("日志告警后处理完成",
		zap.Int64("event_id", eventID), zap.String("service", event.ServiceName),
		zap.String("state", finalState), zap.Bool("notified", notifyErr == nil),
		zap.Bool("has_analysis", brief != nil))
	return nil
}

// analyze 执行 AI 代码分析，返回（结论摘要, 状态, 原因）。
//
// 状态为空表示"分析产出了结论"；否则状态/原因用于页面解释"为什么没有结论"。
func (w *LogAlertWorker) analyze(ctx context.Context, event model.LogAlertEvent, rule model.LogAlertRule) (*LogAnalysisBrief, string, string) {
	ready := w.analysis != nil && w.fetcher != nil && w.codeRepos != nil
	if state, reason := analysisDecision(rule, event.ServiceName, ready, true); state != "" {
		return nil, state, reason
	}
	repo, err := w.codeRepos.FindByService(ctx, event.ServiceName)
	if err != nil || repo == nil {
		return nil, model.LogAnalysisDisabled,
			fmt.Sprintf("服务 %s 未配置代码仓库映射：到「日志监控 → 代码仓库」添加后点「重新分析」", event.ServiceName)
	}
	// 拉代码（首次 clone、之后更新；同一服务有最小间隔，避免风暴期反复拉远端）。
	if _, fetchErr := w.ensureRepo(ctx, *repo); fetchErr != nil {
		return nil, model.LogAnalysisFailed, "拉取代码失败：" + fetchErr.Error()
	}

	timeout := 2 * time.Minute
	if w.cfg != nil && w.cfg.LogAlert.AnalyzeTimeout > 0 {
		timeout = w.cfg.LogAlert.AnalyzeTimeout
	}
	result, analyzeErr := w.analysis.AnalyzeWithTimeout(ctx,
		CodeAnalysisRequest{EventID: event.ID}, systemOperator, timeout)
	if analyzeErr != nil {
		return nil, model.LogAnalysisFailed, "AI 分析失败：" + analyzeErr.Error()
	}
	return briefOf(result), "", ""
}

// briefOf 从分析结果里抽取外发通知需要的字段。
func briefOf(result *AnalyzeResult) *LogAnalysisBrief {
	if result == nil {
		return nil
	}
	brief := &LogAnalysisBrief{
		RootCause:     strOf(result.Report["root_cause"]),
		FixSuggestion: strOf(result.Report["fix_suggestion"]),
		ImpactScope:   strOf(result.Report["impact_scope"]),
		EngineUsed:    result.EngineUsed,
		Degraded:      result.EngineStatus == model.EngineStatusFallback,
	}
	if v, ok := result.Report["confidence"].(float64); ok {
		brief.Confidence = v
	}
	if located, ok := result.Evidence["located_file"].(string); ok {
		brief.LocatedFile = located
	}
	if result.Report["located_file"] != nil {
		brief.LocatedFile = strOf(result.Report["located_file"])
	}
	if v, ok := result.Report["located_line"].(float64); ok {
		brief.LocatedLine = int(v)
	}
	return brief
}

// notify 外发通知并把结论带上。
//
// 通知失败**不阻断**状态落库：分析结论在平台上照样有价值，不该因为 IM 网关抖动一起丢掉；
// 失败原因会由调用方拼进 analysis_error（notifySuffix），页面上看得见。
func (w *LogAlertWorker) notify(ctx context.Context, event model.LogAlertEvent, rule model.LogAlertRule, brief *LogAnalysisBrief) error {
	if w.notifier == nil {
		return fmt.Errorf("通知服务未装配")
	}
	if event.Suppressed {
		w.log.Debug("日志告警处于冷却抑制中，跳过通知",
			zap.Int64("event_id", event.ID), zap.String("service", event.ServiceName))
		return fmt.Errorf("冷却期内抑制")
	}
	channels := []string(rule.NotifyChannels)
	if err := w.notifier.NotifyLogEvent(ctx, event, channels, brief); err != nil {
		w.log.Warn("日志告警通知失败", zap.Int64("event_id", event.ID), zap.Error(err))
		return err
	}
	if err := w.events.MarkNotified(ctx, event.ID, time.Now().UTC()); err != nil {
		w.log.Warn("标记已通知失败", zap.Error(err))
	}
	return nil
}

// ensureRepo 保证本地有一份与远端一致的代码，并把路径写回 CodeRepo（页面可见）。
func (w *LogAlertWorker) ensureRepo(ctx context.Context, repo model.CodeRepo) (RepoFetchResult, error) {
	interval := 300
	if w.cfg != nil && w.cfg.CodeRepo.RefreshInterval > 0 {
		interval = w.cfg.CodeRepo.RefreshInterval
	}
	if last, ok := w.repoRefreshedAt.Load(repo.ServiceName); ok {
		if at, ok := last.(time.Time); ok && time.Since(at) < time.Duration(interval)*time.Second {
			// 距离上次拉取还没到最小间隔：直接用本地副本（它刚刚被刷新过）。
			return RepoFetchResult{LocalPath: repo.LocalPath, Action: "cached"}, nil
		}
	}
	allowOutbound := true
	if w.cfg != nil {
		allowOutbound = w.cfg.CodeRepo.AllowOutbound
	}
	result, err := w.fetcher.Ensure(ctx, RepoFetchRequest{
		Service: repo.ServiceName, RepoURL: repo.RepoURL, Branch: repo.Branch,
		LocalPath: repo.LocalPath, AllowOutbound: allowOutbound,
	})
	if err != nil {
		return result, err
	}
	w.repoRefreshedAt.Store(repo.ServiceName, time.Now())
	if result.LocalPath != "" && result.LocalPath != repo.LocalPath {
		// 把本地路径写回映射：后续分析（与页面上展示）都用它，避免每次重新推导。
		repo.LocalPath = result.LocalPath
	}
	now := time.Now().UTC()
	repo.LastPullAt = &now
	if err := w.codeRepos.Update(ctx, &repo); err != nil {
		w.log.Warn("更新代码仓库拉取时间失败", zap.String("service", repo.ServiceName), zap.Error(err))
	}
	return result, nil
}

// analysisDecision 决定"这条事件在 AI 分析这一步该怎么收场"，并给出给使用者看的原因。
//
// 抽成纯函数的原因：这三个判断（规则开关 / 平台能力 / 仓库映射）是整条链路里最容易配错的，
// 而它们的结果直接决定页面上显示"未启用"还是"分析失败"——必须能被单测逐条钉住。
// 返回 state 为空表示"可以继续分析"。
func analysisDecision(rule model.LogAlertRule, service string, analysisReady, repoFound bool) (state, reason string) {
	if !rule.AIEnabled {
		return model.LogAnalysisDisabled,
			"该规则未启用 AI 分析（如需结论：把规则的「AI 分析」打开，或对该条点「重新分析」）"
	}
	if !analysisReady {
		return model.LogAnalysisDisabled, "平台未装配代码分析能力（缺少 AI 引擎或代码仓库缓存）"
	}
	if !repoFound {
		return model.LogAnalysisDisabled,
			fmt.Sprintf("服务 %s 未配置代码仓库映射：到「日志监控 → 代码仓库」添加后点「重新分析」", service)
	}
	return "", ""
}

// ruleOf 取出事件命中的规则；事件没记规则（老数据/默认值）时回落到平台默认规则。
func (w *LogAlertWorker) ruleOf(ctx context.Context, event model.LogAlertEvent) model.LogAlertRule {
	if event.RuleID > 0 && w.rules != nil {
		if rule, err := w.rules.Get(ctx, event.RuleID); err == nil && rule != nil {
			return *rule
		}
	}
	fallback := model.LogAlertRule{Name: "（平台默认）", DedupWindow: 5, Cooldown: 10, AIEnabled: true}
	if w.cfg != nil {
		fallback.DedupWindow = w.cfg.LogAlert.DefaultDedupWindow
		fallback.Cooldown = w.cfg.LogAlert.DefaultCooldown
		fallback.AIEnabled = w.cfg.LogAlert.DefaultAIEnabled
		fallback.NotifyChannels = model.JSONStringSlice(w.cfg.LogAlert.DefaultNotifyChannels)
	}
	return fallback
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

// failSoft 把"业务失败/正常跳过"写成事件状态，而不是让整轮任务报错。
func (w *LogAlertWorker) failSoft(ctx context.Context, eventID int64, state, reason string) error {
	if err := w.events.SetAnalysisState(ctx, eventID, state, truncateText(reason, 500)); err != nil {
		return err
	}
	w.log.Info("日志告警后处理结束（未产出结论）",
		zap.Int64("event_id", eventID), zap.String("state", state), zap.String("reason", reason))
	return nil
}
