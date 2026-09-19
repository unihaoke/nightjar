package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
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
	cipher    cipherCodec
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
	// HasCode 报告本地是否已经有可用代码：调用方靠它区分"刷新间隔内可复用本地副本"
	// 与"本地没有代码、必须 clone"。没有它，"刚刚拉过"这个记忆会让代码缺失的服务
	// 一直拿不到代码（容器重建丢缓存卷后最典型）。
	HasCode(ctx context.Context, req RepoFetchRequest) bool
	// TrackedFiles 返回本地缓存里被 git 跟踪的文件（相对路径）。
	//
	// 定位代码用它而不是遍历文件系统：工作区里有依赖目录、构建产物和未跟踪残留，
	// 遍历顺序撞上的第一个同名文件常常不是服务自己的源码（INC-032）。
	TrackedFiles(ctx context.Context, req RepoFetchRequest) ([]string, error)
	// Revision 返回本地缓存的当前提交（短 sha）：结论必须能归属到具体代码版本。
	Revision(ctx context.Context, req RepoFetchRequest) string
}

// RepoFetchRequest / RepoFetchResult 是跨层的数据形状（与 internal/repo 的 Request/Result 对应）。
//
// 只描述"这次要哪个服务的哪条分支"：缓存根目录、git 路径、超时都是进程级配置，
// 在容器装配时一次性交给 Fetcher（见 repo_adapter.go）。
type RepoFetchRequest struct {
	Service   string
	RepoURL   string
	Branch    string
	LocalPath string
	// Credential 是解密后的访问凭据（空表示不需要认证，或凭据内嵌在 RepoURL 里）。
	// 它只在执行 git 命令时使用，绝不落库、绝不回显。
	Credential    string
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
	// Cipher 用于解密仓库凭据（见 coderepo_credential.go）。缺省时凭据为空 → 只能拉公开仓库。
	Cipher cipherCodec
	Log    *zap.Logger
}

// NewLogAlertWorker 构造后处理器。
func NewLogAlertWorker(d LogAlertWorkerDeps) *LogAlertWorker {
	return &LogAlertWorker{
		cfg: d.Config, events: d.Events, rules: d.Rules, codeRepos: d.CodeRepos,
		notifier: d.Notifier, analysis: d.Analysis, fetcher: d.Fetcher,
		cipher: d.Cipher, log: d.Log,
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
		// 合规阻断（CodeOutboundDenied）**不是故障**，是明确的配置结果：
		// 它和"规则关了 AI"一样属于 disabled。若按 failed 落库，使用者会去查
		// "AI 为什么挂了"，而真正该做的是去勾「允许第三方 AI」或改引擎策略（INC-030）。
		if apperr.From(analyzeErr).Code == apperr.CodeOutboundDenied {
			return nil, model.LogAnalysisDisabled, analyzeErr.Error()
		}
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
	if err := w.notifier.NotifyLogEvent(ctx, event, rule, channels, brief); err != nil {
		w.log.Warn("日志告警通知失败", zap.Int64("event_id", event.ID), zap.Error(err))
		return err
	}
	if err := w.events.MarkNotified(ctx, event.ID, time.Now().UTC()); err != nil {
		w.log.Warn("标记已通知失败", zap.Error(err))
	}
	return nil
}

// canReuseLocalCache 判断本次能否跳过远端、直接复用本地副本。
//
// 两个条件缺一不可：
//   - 本地确实有代码：**没有代码时必须去 clone**，"刚刚拉过"这种记忆不能替代代码本身；
//     容器重建丢了缓存卷之后，正是这条决定了服务能不能重新拿到代码；
//   - 距上次拉取还在最小间隔内：避免故障风暴里每条事件都去 pull 一次远端。
//
// interval <= 0 表示不设间隔（每次都向远端确认），last 为零值表示本次进程还没拉过。
func canReuseLocalCache(hasCode bool, last, now time.Time, interval time.Duration) bool {
	if !hasCode || last.IsZero() || interval <= 0 {
		return false
	}
	return now.Sub(last) < interval
}

// fetchRequest 把一条仓库映射转成拉取请求（进程级配置与凭据在这里补齐）。
//
// 凭据在**这里**解密（而不是让 fetcher 自己去查库）：internal/repo 是纯粹的 git 门面，
// 它不该知道"凭据从哪来、怎么加密"；同时这样也让"凭据只在内存里存在一小会儿"这件事
// 只有一个出口，便于审计。
func (w *LogAlertWorker) fetchRequest(repo model.CodeRepo) RepoFetchRequest {
	allowOutbound := true
	if w.cfg != nil {
		allowOutbound = w.cfg.CodeRepo.AllowOutbound
	}
	return RepoFetchRequest{
		Service: repo.ServiceName, RepoURL: repo.RepoURL, Branch: repo.Branch,
		LocalPath: repo.LocalPath, AllowOutbound: allowOutbound,
		Credential: decryptRepoCredential(w.cipher, &repo),
	}
}

// ensureRepo 保证本地有一份与远端一致的代码，并把路径写回 CodeRepo（页面可见）。
//
// 关键约束：只有**本地确实有代码**时，"最小拉取间隔"才允许跳过远端。
// 否则会出现「刚拉过 → 走缓存 → 但代码其实不在了（卷被重建/目录被清）」，
// 分析阶段拿着不存在的目录去定位代码，静默返回空结论——这是最难查的一类失败。
func (w *LogAlertWorker) ensureRepo(ctx context.Context, repo model.CodeRepo) (RepoFetchResult, error) {
	// 旧数据里内嵌的凭据先迁移（幂等）：迁移后的 URL 才是能安全回显、且 origin 干净的那一份。
	migrateRepoCredentialRow(ctx, w.codeRepos, w.cipher, w.log, &repo)
	result, err := w.syncRepo(ctx, repo)
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

// syncRepo 是 ensureRepo 里"只管把代码拉到位"的那一段（记忆刷新时间与落库留给外层，
// 这样这段判定可以在没有数据库的单测里被钉住——它是最容易退化的一条规则）。
func (w *LogAlertWorker) syncRepo(ctx context.Context, repo model.CodeRepo) (RepoFetchResult, error) {
	req := w.fetchRequest(repo)
	interval := 300
	if w.cfg != nil && w.cfg.CodeRepo.RefreshInterval > 0 {
		interval = w.cfg.CodeRepo.RefreshInterval
	}
	// "上次拉取时间"取两个来源里较新的那个：
	//   - 进程内记忆：本次启动以来成功拉过的时间（最准）；
	//   - DB 的 last_pull_at：**上次进程**留下的时间。
	// 只认内存记忆的话，每次容器重建/滚动升级都会把所有仓库重新 fetch 一遍
	//（缓存卷还在、代码也是新的，纯属白跑），仓库多的时候会把出网带宽和启动时间都吃掉（INC-031）。
	last := repoLastPull(&w.repoRefreshedAt, repo)
	if canReuseLocalCache(w.fetcher.HasCode(ctx, req), last, time.Now(), time.Duration(interval)*time.Second) {
		// 距离上次拉取还没到最小间隔：直接用本地副本（它刚刚被刷新过）。
		return RepoFetchResult{LocalPath: repo.LocalPath, Action: "cached"}, nil
	}
	return w.fetcher.Ensure(ctx, req)
}

// repoLastPull 取"该服务最近一次成功拉取"的时间（进程内记忆与 DB 记录取较新者）。
//
// 指针接收 sync.Map 是刻意的：sync.Map 含锁，按值传会被 go vet 判为复制锁
//（而且复制出来的 map 与原 map 共享内部结构，语义也很容易写错）。
func repoLastPull(memory *sync.Map, repo model.CodeRepo) time.Time {
	var last time.Time
	if memory != nil {
		if at, ok := memory.Load(repo.ServiceName); ok {
			if t, ok := at.(time.Time); ok {
				last = t
			}
		}
	}
	if repo.LastPullAt != nil && repo.LastPullAt.After(last) {
		last = *repo.LastPullAt
	}
	return last
}

// repoWarmLimit 是启动预热最多处理的仓库数（防御性上限：映射表不该有成千上万条，
// 真有也不该在启动阶段把代码托管打满）。
const repoWarmLimit = 200

// WarmRepos 在进程启动后把所有已配置的代码仓库补齐：本地没有代码 → clone，已有 → pull。
//
// 为什么必须有这一步（对应"服务重新构建"的场景）：平台容器重建后缓存目录常常是空的
// （数据卷没挂、被清理、或换成了新卷）。若等到第一条告警事件才去 clone，
// 那一次分析必然拿不到代码——AI 定位不到行，只能给出没有代码依据的结论；
// 而大仓库 clone 动辄几分钟，还会把事件处理堵住。预热把这笔时间花在没有事件压力的时候。
//
// 刻意异步、失败只记日志：预热是"让第一条告警更快更准"，它不该阻断启动，
// 也不该因为某个仓库的凭据过期就让平台起不来。失败的仓库会在事件处理里按正常逻辑重试。
func (w *LogAlertWorker) WarmRepos(ctx context.Context) (ok, failed int) {
	if w == nil || w.codeRepos == nil || w.fetcher == nil {
		return 0, 0
	}
	if ctx == nil {
		ctx = context.Background()
	}
	allowOutbound := true
	if w.cfg != nil {
		allowOutbound = w.cfg.CodeRepo.AllowOutbound
	}
	if !allowOutbound {
		w.log.Info("出网许可未开启，跳过启动预热（本次不会执行任何 git 命令）")
		return 0, 0
	}
	items, _, err := w.codeRepos.List(ctx, "", repoWarmLimit, 0)
	if err != nil {
		w.log.Warn("读取代码仓库映射失败，跳过启动预热", zap.Error(err))
		return 0, 0
	}
	// 预热也要认"最小拉取间隔"：容器重建后缓存卷里的代码往往还在、也还是新的，
	// 只因为进程内的记忆随着重启丢了就全量 fetch 一遍，属于纯浪费（仓库多时尤其明显）。
	interval := 300
	if w.cfg != nil && w.cfg.CodeRepo.RefreshInterval > 0 {
		interval = w.cfg.CodeRepo.RefreshInterval
	}
	for i := range items {
		if ctx.Err() != nil {
			return ok, failed
		}
		item := items[i]
		if strings.TrimSpace(item.RepoURL) == "" {
			// 没有仓库地址就没法 clone：这是配置问题，说清楚比默默跳过好。
			w.log.Warn("跳过预热：服务未配置仓库地址",
				zap.String("service", item.ServiceName))
			failed++
			continue
		}
		// 旧数据里内嵌的凭据先迁移成"干净 URL + 加密凭据"（幂等）。
		migrateRepoCredentialRow(ctx, w.codeRepos, w.cipher, w.log, &item)
		req := w.fetchRequest(item)
		if canReuseLocalCache(w.fetcher.HasCode(ctx, req), repoLastPull(&w.repoRefreshedAt, item),
			time.Now(), time.Duration(interval)*time.Second) {
			w.log.Info("跳过预热：距上次拉取还在最小间隔内，且本地已有代码",
				zap.String("service", item.ServiceName),
				zap.Time("last_pull_at", repoLastPull(&w.repoRefreshedAt, item)))
			ok++
			continue
		}
		res, err := w.fetcher.Ensure(ctx, req)
		if err != nil {
			failed++
			w.log.Warn("启动预热失败（下次分析会重试）",
				zap.String("service", item.ServiceName), zap.Error(err))
			continue
		}
		ok++
		// 成功才记刷新时间：失败的仓库不该被"最小拉取间隔"挡住重试。
		w.repoRefreshedAt.Store(item.ServiceName, time.Now())
		if res.LocalPath != "" && res.LocalPath != item.LocalPath {
			item.LocalPath = res.LocalPath
		}
		now := time.Now().UTC()
		item.LastPullAt = &now
		if err := w.codeRepos.Update(ctx, &item); err != nil {
			w.log.Warn("更新代码仓库拉取时间失败", zap.String("service", item.ServiceName), zap.Error(err))
		}
		w.log.Info("代码缓存预热完成",
			zap.String("service", item.ServiceName), zap.String("action", res.Action),
			zap.String("revision", res.Revision), zap.String("local_path", res.LocalPath))
	}
	return ok, failed
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

// failSoft 把"业务失败/正常跳过"写成事件状态，而不是让整轮任务报错。
func (w *LogAlertWorker) failSoft(ctx context.Context, eventID int64, state, reason string) error {
	if err := w.events.SetAnalysisState(ctx, eventID, state, truncateText(reason, 500)); err != nil {
		return err
	}
	w.log.Info("日志告警后处理结束（未产出结论）",
		zap.Int64("event_id", eventID), zap.String("state", state), zap.String("reason", reason))
	return nil
}
