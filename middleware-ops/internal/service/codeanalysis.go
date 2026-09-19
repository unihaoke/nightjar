package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/config"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/service/ai"
)

// CodeAnalysisService 实现 AI 代码分析（4.8.3 一期收敛方案）。
//
// 一期方案：
//  1. 首选第三方 AI API（需通过出网白名单 + 脱敏，见 6.5）；
//  2. 备选本地检索 + LLM（堆栈定位文件行 → 上下文切片 → 本地推理）。
//
// 自研三层索引（AST + 知识图谱 + 向量 + Rerank）为二期项，一期不投入。
type CodeAnalysisService struct {
	cfg      *config.Config
	engine   engine.Engine
	events   *repository.LogEventRepository
	repos    *repository.CodeRepoRepository
	analyses *repository.CodeAnalysisRepository
	redactor *Redactor
	audit    *AuditService
	cost     engineGuard
	// cipher 用于解密仓库凭据（见 coderepo_credential.go）：分析前拉代码要用它。
	cipher cipherCodec
	log    *zap.Logger
	// trackedCache 缓存"仓库里被 git 跟踪的文件清单"，避免每次分析都跑一次 git ls-files。
	trackedCache   map[string]trackedFilesEntry
	trackedCacheMu sync.Mutex
	// fetcher 可选：装配后，分析前会确保本地有一份代码（没有就 clone）。
	// 用 SetRepoFetcher 注入而不是构造参数，是因为装配顺序上 fetcher 要在 worker 之前建好，
	// 而 worker 又依赖本服务——拆成两步避免构造顺序上的循环。
	fetcher RepoFetcher
}

// trackedFilesEntry 是文件清单缓存的一条记录。
type trackedFilesEntry struct {
	files   []string
	fetched time.Time
}

// trackedFilesTTL 是文件清单的缓存时长。
//
// 为什么缓存：清单只随"代码更新"变化，而分析频率由日志量决定（风暴期可达每分钟数十次），
// 每次都 git ls-files 是白开销。取 2 分钟：既挡住风暴期的重复遍历，
// 又保证刚更新完代码后最多 2 分钟就能看到新文件（对定位精度的影响可忽略）。
const trackedFilesTTL = 2 * time.Minute

// SetRepoFetcher 装配代码仓库缓存（见字段注释里的装配顺序说明）。
func (s *CodeAnalysisService) SetRepoFetcher(f RepoFetcher) {
	s.fetcher = f
}

// engineGuard 抽象成本记账，避免代码分析服务直接依赖护栏实现细节。
type engineGuard interface {
	Check(userID int64) error
	Commit(userID int64, usage int) (float64, string)
}

// NewCodeAnalysisService 构造代码分析服务。
func NewCodeAnalysisService(
	cfg *config.Config,
	eng engine.Engine,
	events *repository.LogEventRepository,
	repos *repository.CodeRepoRepository,
	analyses *repository.CodeAnalysisRepository,
	redactor *Redactor,
	audit *AuditService,
	cost engineGuard,
	cipher cipherCodec,
	log *zap.Logger,
) *CodeAnalysisService {
	return &CodeAnalysisService{
		cfg: cfg, engine: eng, events: events, repos: repos, analyses: analyses,
		redactor: redactor, audit: audit, cost: cost, cipher: cipher, log: log,
		trackedCache: make(map[string]trackedFilesEntry),
	}
}

// thirdPartyAllowedFor 计算"本次是否允许把代码片段发给第三方 AI"。
//
// 三个条件同时成立才算允许（缺一即不允许，且**不允许就等于不调用外部引擎**）：
//  1. 该仓库映射勾选了「允许第三方 AI」（`CodeRepo.AllowThirdParty`）；
//  2. 本次请求没有显式要求 `force_local`；
//  3. 服务在出网白名单里（`security.outbound_whitelist`）。
//
// 抽成纯函数的原因：它是整条链路里最敏感的一条判定，必须能被逐格钉住
//（真实缺陷 INC-030 就是"算了但没用"，所以这里连"给使用者看的告警文案"一起钉）。
func thirdPartyAllowedFor(repo *model.CodeRepo, forceLocal, whitelisted bool, serviceName string) (allowed bool, warn string) {
	if repo == nil {
		return false, ""
	}
	if !repo.AllowThirdParty || forceLocal {
		return false, ""
	}
	if !whitelisted {
		return false, "服务 " + serviceName + " 不在出网白名单内，已自动降级为本地分析"
	}
	return true, ""
}

// outboundGate 是"这次能不能调用 AI 引擎"的**唯一判定入口**。
//
// 返回：
//   - allowed：可以调用引擎（repos 与残留告警见 thirdPartyAllowedFor）；
//   - blocked：不能调用，且**原因必须给出**（写进报告、事件状态与接口错误）。
//
// 为什么把"引擎是否外发"和"是否许可"放进同一个函数：INC-030 的缺陷正是
// "两个条件都算出来了、但调用点只用了一个"。把判定收成一个函数，
// 调用点就只剩 `if blocked { ... }` —— 少一个可以忘记使用的变量。
func (s *CodeAnalysisService) outboundGate(
	repo *model.CodeRepo, forceLocal bool, whitelisted bool, serviceName string,
) (allowed, blocked bool, warn, reason string) {
	ext := engineExternal(s.engine)
	allowed, warn = thirdPartyAllowedFor(repo, forceLocal, whitelisted, serviceName)
	if ext && !allowed {
		return false, true, warn, s.outboundBlockReason(serviceName, repo, forceLocal)
	}
	return allowed, false, warn, ""
}

// engineNone 是"没有调用任何 AI 引擎"时写进报告的引擎名。
//
// 刻意不用引擎自己的名字：合规阻断的那条记录里必须一眼看出"这次根本没问 AI"，
// 否则使用者会以为结论来自模型（同 INC-016 的"不许有看起来像真的假数据"）。
const engineNone = "none"

// engineExternal 报告当前 AI 引擎是否可能把内容发往组织外部（第三方 AI）。
//
// 判定来源是引擎自己的状态声明（engine.Status.External）：混合链上只要含外部引擎就为真，
// 规则引擎与自建引擎为假。**不看配置字符串**是刻意的——工厂可能因为缺 base_url/未启用
// 把第三方降级成规则引擎，那种情况下确实不会出网，按配置判会误拦。
func engineExternal(eng engine.Engine) bool {
	if eng == nil {
		return false
	}
	return eng.Status().External
}

// outboundBlockReason 生成合规阻断时给使用者看的原因与出路。
//
// 必须"可操作"：使用者看到的不能只是"被拒绝了"，而要能判断是去勾开关、去加白名单，
// 还是去改引擎策略——这三件事发生在三个不同的页面/配置里。
func (s *CodeAnalysisService) outboundBlockReason(serviceName string, repo *model.CodeRepo, forceLocal bool) string {
	reasons := make([]string, 0, 3)
	if forceLocal {
		reasons = append(reasons, "本次请求显式要求「只用本地分析」（force_local）")
	}
	if repo == nil {
		reasons = append(reasons, "服务 "+serviceName+" 没有配置代码仓库映射")
	} else if !repo.AllowThirdParty {
		reasons = append(reasons,
			"仓库映射未开启「允许第三方 AI」（日志监控 → 代码仓库 → "+serviceName+"）")
	} else {
		reasons = append(reasons,
			"服务 "+serviceName+" 不在出网白名单里（security.outbound_whitelist）")
	}
	return "按合规设置，本次不允许把代码片段发送给第三方 AI：" + strings.Join(reasons, "；") +
		"。平台当前使用的 AI 引擎是第三方提供方，因此已阻止调用它（代码片段只留在平台内部）。" +
		"三种出路：① 在仓库映射里打开「允许第三方 AI」并把服务加入出网白名单；" +
		"② 把 ai_engine.strategy 改为 self_hosted 并配置内网 LLM（此时不受此限制）；" +
		"③ 只依赖平台给出的本地定位结果（文件与行号已经记录）。"
}

// migrateRepoCredential 迁移该仓库记录里可能内嵌在 URL 中的旧凭据（幂等，见 coderepo_credential.go）。
func (s *CodeAnalysisService) migrateRepoCredential(ctx context.Context, item *model.CodeRepo) *model.CodeRepo {
	return migrateRepoCredentialRow(ctx, s.repos, s.cipher, s.log, item)
}

// repoFetchRequest 把一条仓库映射转成拉取请求（凭据在这里解密）。
func (s *CodeAnalysisService) repoFetchRequest(item *model.CodeRepo) RepoFetchRequest {
	allowOutbound := true
	if s.cfg != nil {
		allowOutbound = s.cfg.CodeRepo.AllowOutbound
	}
	if item == nil {
		return RepoFetchRequest{AllowOutbound: allowOutbound}
	}
	return RepoFetchRequest{
		Service: item.ServiceName, RepoURL: item.RepoURL, Branch: item.Branch,
		LocalPath: item.LocalPath, AllowOutbound: allowOutbound,
		Credential: decryptRepoCredential(s.cipher, item),
	}
}

// repoRevision 取本地缓存当前的提交（短 sha），用于把结论绑到具体代码版本。
func (s *CodeAnalysisService) repoRevision(ctx context.Context, item *model.CodeRepo) string {
	if s.fetcher == nil || item == nil {
		return ""
	}
	return s.fetcher.Revision(ctx, s.repoFetchRequest(item))
}

// CodeAnalysisRequest 是代码分析请求。
type CodeAnalysisRequest struct {
	EventID int64 `json:"event_id"`
	// ServiceName 允许直接提交堆栈（不关联事件），用于手工排查。
	ServiceName string `json:"service"`
	Stacktrace  string `json:"stacktrace"`
	Message     string `json:"message"`
	// ForceLocal 强制走本地检索 + LLM（合规场景，6.5）。
	ForceLocal bool `json:"force_local"`
}

// AnalyzeResult 是代码分析结果。
type AnalyzeResult struct {
	AnalysisID   int64          `json:"analysis_id"`
	EventID      int64          `json:"event_id"`
	Report       map[string]any `json:"report"`
	Evidence     map[string]any `json:"evidence"`
	OutboundOK   bool           `json:"outbound_ok"`
	EngineUsed   string         `json:"engine_used"`
	EngineStatus string         `json:"engine_status"`
	CostTokens   int            `json:"cost_tokens"`
	Warnings     []string       `json:"warnings"`
}

// Analyze 执行一次代码分析。
func (s *CodeAnalysisService) Analyze(ctx context.Context, in CodeAnalysisRequest, operator Operator) (*AnalyzeResult, error) {
	var (
		serviceName = in.ServiceName
		stack       = in.Stacktrace
		message     = in.Message
		eventKey    = ""
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
		eventKey = item.EventID
	}
	if strings.TrimSpace(stack) == "" && strings.TrimSpace(message) == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "必须提供 event_id 或堆栈/错误信息")
	}

	warnings := make([]string, 0, 2)
	outboundOK := false
	if s.cost != nil && operator.UserID > 0 {
		if err := s.cost.Check(operator.UserID); err != nil {
			return nil, apperr.Wrap(apperr.CodeQuotaExceeded, err)
		}
	}

	// ① 出网合规校验：按服务维度的白名单（默认关闭，6.5）。
	var repo *model.CodeRepo
	if serviceName != "" {
		if item, err := s.repos.FindByService(ctx, serviceName); err == nil {
			repo = item
			// 旧数据可能把访问令牌内嵌在 repo_url 里（本开关之前的设计）：
			// 读到就顺手迁移成"干净 URL + 加密凭据"，并把它用于本次拉取。
			repo = s.migrateRepoCredential(ctx, repo)
		} else if !repository.EnsureNotFound(err) {
			s.log.Warn("查询服务仓库映射失败", zap.String("service", serviceName), zap.Error(err))
		}
	}
	thirdPartyAllowed, outboundBlocked, allowWarn, blockReason := s.outboundGate(
		repo, in.ForceLocal, inWhitelist(s.cfg.Security.OutboundWhitelist, serviceName), serviceName)
	if allowWarn != "" {
		warnings = append(warnings, allowWarn)
	}
	if thirdPartyAllowed {
		outboundOK = true
	} else if repo == nil {
		warnings = append(warnings, "未配置服务 "+serviceName+" 的仓库映射，按本地分析处理")
	}

	// ② 脱敏：堆栈去 IP / 用户名 / 手机号 / 请求 ID。
	redactedStack := s.redactor.Redact(stack)
	redactedMessage := s.redactor.Redact(message)

	// ③ 本地检索：堆栈定位文件行 → 上下文切片（合规兜底）。
	// 检索前先确保本地真的有代码：目录不存在时 WalkDir 只会静默跳过，
	// 分析照常出结论、却没有任何代码依据——使用者完全看不出少了什么。
	s.ensureCode(ctx, repo)
	snippet, locatedFile, locatedLine, snippetTruncated := s.locateCode(ctx, repo, stack)
	if snippetTruncated {
		warnings = append(warnings, fmt.Sprintf("代码片段超过 %d 行上限，已截断", s.redactor.MaxLines()))
	}
	contextText := s.redactor.Redact(snippet)

	// ③.5 **合规闸门**：平台当前的 AI 引擎若会把内容发往组织外部（第三方 AI），
	// 而该仓库没开「允许第三方」或服务不在出网白名单里 → 到此为止，绝不调用引擎。
	//
	// 为什么必须是硬闸门（真实缺陷 INC-030）：早期实现算出了 thirdPartyEnabled 却只把它
	// 写进证据与文案，`s.engine.Chat` 照常调用——于是"没勾选、不在白名单"的服务，
	// 私有代码片段照样发给了第三方模型，开关形同虚设，而文档却向使用者承诺了相反的事。
	// 合规开关的价值就在于"关掉之后真的出不去"，因此这里宁可让分析没有结论。
	//
	// 注意判定必须在**这一步之前**统一算好（outboundGate）：闸门只消费它的结论，
	// 不允许在这里重新推导条件——INC-030 就是"算了一处、用了一处"的产物。
	if outboundBlocked {
		reason := blockReason
		// 仍然把"本地定位到什么"落库：代码片段留在平台内部，对排障有价值，
		// 而且能让使用者看到"定位其实成功了，只是没发给外部 AI"。
		evidence := map[string]any{
			"stacktrace_redacted": redactedStack,
			"code_snippet":        truncateRunes(contextText, 4000),
			"repo":                repoSummary(repo),
			"third_party":         false,
			"outbound_blocked":    true,
			"event_key":           eventKey,
		}
		record := &model.AICodeAnalysis{
			EventID: in.EventID, EventKey: eventKey, ServiceName: serviceName,
			LocatedFile: locatedFile, LocatedLine: locatedLine,
			CodeSnippet:   truncateRunes(contextText, 8000),
			RootCause:     "未调用 AI：按合规设置，本次不允许把代码片段发送到第三方 AI",
			EmergencyPlan: "按错误指纹与堆栈先做人工定位（平台已给出本地检索到的文件与行号）",
			FixSuggestion: reason,
			EngineUsed:    engineNone, EngineStatus: model.EngineStatusFallback,
			Evidence: model.JSONMap(evidence), OutboundOK: false,
			// 定位结果照样绑到代码版本：这份记录是"平台自己看到的"，与是否发给 AI 无关。
			RepoRevision: s.repoRevision(ctx, repo),
		}
		if err := s.analyses.Create(ctx, record); err != nil {
			s.log.Warn("保存合规阻断记录失败", zap.Error(err))
		}
		if in.EventID > 0 {
			if err := s.events.MarkAnalyzed(ctx, in.EventID); err != nil {
				s.log.Warn("标记事件已分析失败", zap.Error(err))
			}
		}
		s.log.Warn("已阻止把代码片段发给第三方 AI（合规设置）",
			zap.String("service", serviceName), zap.String("engine", s.engine.Name()),
			zap.Bool("repo_allow_third_party", repo != nil && repo.AllowThirdParty),
			zap.Bool("force_local", in.ForceLocal))
		return nil, apperr.New(apperr.CodeOutboundDenied, reason)
	}

	// ④ 一次 LLM 调用（三点式模板）。
	system, user := ai.BuildCodePrompt(redactedStack, contextText, redactedMessage)
	resp, err := s.engine.Chat(ctx, engine.ChatRequest{
		Messages: []engine.Message{
			{Role: engine.RoleSystem, Content: system},
			{Role: engine.RoleUser, Content: user},
		},
		MaxTokens:   1024,
		Temperature: 0.2,
		JSONMode:    true,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeEngineFailed, err)
	}

	// ⑤ 解析三点式报告。
	report := parseCodeReport(resp.Content)
	evidence := map[string]any{
		"stacktrace_redacted": redactedStack,
		"code_snippet":        truncateRunes(contextText, 4000),
		"repo":                repoSummary(repo),
		"third_party":         thirdPartyAllowed,
		"event_key":           eventKey,
	}
	if locatedFile == "" {
		if v, ok := report["located_file"].(string); ok {
			locatedFile = v
		}
	}
	if locatedLine == 0 {
		if v, ok := report["located_line"].(float64); ok {
			locatedLine = int(v)
		}
	}

	engineUsed := s.engine.Name()
	status := model.EngineStatusOK
	if s.engine.Name() == engine.RuleEngineName {
		status = model.EngineStatusFallback
		warnings = append(warnings, "AI 引擎不可用，代码分析由规则引擎给出通用建议，请人工复核")
	}
	confidence := 0.4
	if v, ok := report["confidence"].(float64); ok {
		confidence = v
	}
	record := &model.AICodeAnalysis{
		EventID:       in.EventID,
		EventKey:      eventKey,
		ServiceName:   serviceName,
		LocatedFile:   locatedFile,
		LocatedLine:   locatedLine,
		CodeSnippet:   truncateRunes(contextText, 8000),
		RootCause:     strOf(report["root_cause"]),
		EmergencyPlan: strOf(report["emergency_plan"]),
		FixSuggestion: strOf(report["fix_suggestion"]),
		ImpactScope:   strOf(report["impact_scope"]),
		Confidence:    confidence,
		Evidence:      model.JSONMap(evidence),
		EngineUsed:    engineUsed,
		EngineStatus:  status,
		CostTokens:    resp.Usage.TotalTokens,
		OutboundOK:    outboundOK,
		// 代码版本：结论必须能归属到某一版代码，否则行号随时间失效后无法判断
		// 是"当时的判断错了"还是"代码后来变了"（INC-032 的一部分）。
		RepoRevision: s.repoRevision(ctx, repo),
	}
	if err := s.analyses.Create(ctx, record); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if in.EventID > 0 {
		if err := s.events.MarkAnalyzed(ctx, in.EventID); err != nil {
			s.log.Warn("标记事件已分析失败", zap.Error(err))
		}
	}
	if s.cost != nil && operator.UserID > 0 {
		if _, warning := s.cost.Commit(operator.UserID, resp.Usage.TotalTokens); warning != "" {
			warnings = append(warnings, warning)
		}
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, ActionType: "ai_code_analyze",
			Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{
				"event_id": in.EventID, "service": serviceName, "outbound": thirdPartyAllowed,
				"tokens": resp.Usage.TotalTokens, "engine": engineUsed,
			},
		})
	}
	return &AnalyzeResult{
		AnalysisID: record.ID, EventID: in.EventID, Report: report, Evidence: evidence,
		OutboundOK: outboundOK, EngineUsed: engineUsed, EngineStatus: status,
		CostTokens: resp.Usage.TotalTokens, Warnings: warnings,
	}, nil
}

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
			return nil, apperr.New(apperr.CodeNotFound, "该事件还没有代码分析报告")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// ensureCode 保证分析时本地有一份代码：**没有就 clone，有就直接用**（不重复 pull）。
//
// 为什么两处都要兜（这里是分析入口，worker 里还有一次）：
//   - 手工分析（页面直接提交堆栈）与事件重试都可能不走 worker 的拉取步骤；
//   - 容器重建后缓存目录可能整块消失，此时"本地没有代码"是常态而不是异常。
//
// 有代码时**刻意不拉远端**：一次手工分析不应触发一次 pull（既慢又可能被代码托管限流），
// 拉到最新是 worker/启动预热的事；这里只保证"分析时手上有代码"。
func (s *CodeAnalysisService) ensureCode(ctx context.Context, repo *model.CodeRepo) {
	if s == nil || s.fetcher == nil || repo == nil {
		return
	}
	if strings.TrimSpace(repo.RepoURL) == "" {
		return
	}
	req := s.repoFetchRequest(repo)
	if s.fetcher.HasCode(ctx, req) {
		return
	}
	res, err := s.fetcher.Ensure(ctx, req)
	if err != nil {
		// 拉取失败不阻断分析：本地检索拿不到代码片段时，LLM 仍可基于堆栈给出建议，
		// 只是结论里没有代码上下文——把原因记进日志，界面上由 warning 反映。
		s.log.Warn("分析前拉取代码失败（本次分析没有代码上下文）",
			zap.String("service", repo.ServiceName), zap.Error(err))
		return
	}
	if res.LocalPath != "" {
		repo.LocalPath = res.LocalPath
	}
	s.log.Info("分析前已克隆代码",
		zap.String("service", repo.ServiceName), zap.String("revision", res.Revision))
}

// locateCode 依据堆栈定位本地代码文件并切片。
//
// 定位策略（INC-032 重写）：
//  1. 候选集来自 **git 索引**（`git ls-files`）而不是遍历文件系统：工作区里还有依赖目录、
//     构建产物和未跟踪残留，遍历顺序撞上的同名文件往往不是服务自己的源码；
//  2. 按"堆栈给出的路径线索"打分：Java 的 `a.b.OrderService` → `a/b/OrderService.java`，
//     Python/Go 的原始路径后缀，越匹配分越高；
//  3. 依赖/产物/测试目录参与扣分而不是直接排除：源码确实缺失时，退而求其次给一个候选
//     也比"什么都找不到"有用，但绝不能让它压过真正的源码；
//  4. 取分最高者；并列时取路径最浅的（更靠近仓库根 = 更可能是该服务自身的源码）。
//
// 为什么打分而不是"第一个同名文件"：日志分析的价值全在"行号对不对"，
// 给一个来自 vendor 目录的同名文件的第 120 行，比诚实地说"没定位到"更有害。
func (s *CodeAnalysisService) locateCode(ctx context.Context, repo *model.CodeRepo, stacktrace string) (snippet, file string, line int, truncated bool) {
	if repo == nil || strings.TrimSpace(repo.LocalPath) == "" || strings.TrimSpace(stacktrace) == "" {
		return "", "", 0, false
	}
	index := locateIndex(stacktrace)
	if index == nil {
		return "", "", 0, false
	}
	root := repo.LocalPath
	candidates := s.candidateFiles(ctx, repo, index.fileName)
	if len(candidates) == 0 {
		return "", "", 0, false
	}
	best := pickBestCandidate(candidates, index)
	if best == "" {
		return "", "", 0, false
	}
	found := filepath.Join(root, filepath.FromSlash(best))
	// 只读普通文件：符号链接可能指向仓库外部（一个 `Foo.java -> /etc/passwd` 的软链
	// 就能让平台把任意文件内容读出来并送进分析/结论）。宁可少一个候选也不越界读。
	if info, statErr := os.Lstat(found); statErr != nil || !info.Mode().IsRegular() {
		return "", "", 0, false
	}
	data, err := os.ReadFile(found)
	if err != nil {
		return "", "", 0, false
	}
	lines := strings.Split(string(data), "\n")
	// 以方法名定位行号，提升片段相关性。
	target := 0
	for i, l := range lines {
		if index.method != "" && strings.Contains(l, index.method) {
			target = i
			break
		}
	}
	start := target - 20
	if start < 0 {
		start = 0
	}
	end := target + 40
	if end > len(lines) {
		end = len(lines)
	}
	body := strings.Join(lines[start:end], "\n")
	body, truncated = s.redactor.TruncateCode(body)
	// 返回仓库内相对路径（不是宿主绝对路径）：结论会被展示、通知、甚至发给第三方 AI，
	// 绝对路径会顺带泄露平台的缓存目录布局。
	return body, best, start + 1, truncated
}

// candidateFiles 返回"文件名与堆栈一致"的候选（仓库内相对路径）。
//
// 优先用 git 索引；索引不可用时（非 git 目录、git 不可执行）退化为带排除目录的遍历，
// 保证功能不至于整块失效。
func (s *CodeAnalysisService) candidateFiles(ctx context.Context, repo *model.CodeRepo, fileName string) []string {
	if fileName == "" {
		return nil
	}
	listed, err := s.trackedFiles(ctx, repo)
	if err != nil {
		s.log.Warn("读取 git 索引失败，退化为遍历文件系统（定位精度可能下降）",
			zap.String("service", repo.ServiceName), zap.Error(err))
		listed = walkSourceFiles(repo.LocalPath)
	}
	matched := make([]string, 0, 8)
	for _, rel := range listed {
		if strings.EqualFold(pathBase(rel), fileName) {
			matched = append(matched, rel)
		}
	}
	return matched
}

// trackedFiles 取该仓库被 git 跟踪的文件清单（带缓存）。
func (s *CodeAnalysisService) trackedFiles(ctx context.Context, repo *model.CodeRepo) ([]string, error) {
	if s.fetcher == nil {
		return nil, errNoFetcher
	}
	key := repo.LocalPath
	s.trackedCacheMu.Lock()
	// 惰性初始化：也用结构体字面量构造（测试、以及将来别的装配路径）时这个 map 可能是 nil，
	// 直接写会 panic。缓存本身是优化，不该成为"少写一个字段就崩"的坑。
	if s.trackedCache == nil {
		s.trackedCache = make(map[string]trackedFilesEntry)
	}
	if entry, ok := s.trackedCache[key]; ok && time.Since(entry.fetched) < trackedFilesTTL {
		s.trackedCacheMu.Unlock()
		return entry.files, nil
	}
	s.trackedCacheMu.Unlock()

	files, err := s.fetcher.TrackedFiles(ctx, s.repoFetchRequest(repo))
	if err != nil {
		return nil, err
	}
	s.trackedCacheMu.Lock()
	// 简单上限：缓存条目数跟着仓库数走，真到几千个仓库时也没有内存压力，
	// 但仍加一道闸，避免 LocalPath 变化（用户改配置）时无限增长。
	if len(s.trackedCache) > 512 {
		s.trackedCache = make(map[string]trackedFilesEntry)
	}
	s.trackedCache[key] = trackedFilesEntry{files: files, fetched: time.Now()}
	s.trackedCacheMu.Unlock()
	return files, nil
}

var errNoFetcher = errors.New("代码仓库缓存未装配（无法读取 git 索引）")

// pickBestCandidate 从同名候选里挑出最可能是"这条堆栈"的那一个。
//
// 打分（分高者胜）：
//
//	+100 路径以堆栈给出的路径线索结尾（例：a/b/OrderService.java）
//	 +20 路径与线索的最后一段目录一致（线索不完整时的次优匹配）
//	  +0 仅文件名一致
//	 -50 命中依赖/产物目录（vendor、node_modules、target、dist…）
//	 -25 命中测试文件或测试目录
//
// 并列时取路径更浅（目录层级更少）的那个。
func pickBestCandidate(candidates []string, index *locateIndexInfo) string {
	best := ""
	bestScore := -1 << 30
	bestDepth := 1 << 30
	for _, rel := range candidates {
		score := scoreCandidate(rel, index)
		depth := strings.Count(rel, "/")
		if score > bestScore || (score == bestScore && depth < bestDepth) {
			best, bestScore, bestDepth = rel, score, depth
		}
	}
	return best
}

// scoreCandidate 计算单个候选的得分（见 pickBestCandidate 的打分表）。
func scoreCandidate(rel string, index *locateIndexInfo) int {
	score := 0
	lower := strings.ToLower(rel)
	hints := hintSuffixes(strings.ToLower(index.pathHint))
	for i, hint := range hints {
		if lower == hint || strings.HasSuffix(lower, "/"+hint) {
			// 线索匹配得越长越可信（完整路径 > 只有中间几层 > 只有文件名），
			// 因此按后缀层级递减给分，而不是所有匹配都给满分。
			score += 100 - i*10
			break
		}
	}
	if score == 0 && hintDirMatches(lower, strings.ToLower(index.pathHint)) {
		score += 20
	}
	if hitVendorDir(lower) {
		score -= 50
	}
	if isTestPath(lower) {
		score -= 25
	}
	return score
}

// hintSuffixes 把路径线索拆成"由长到短"的后缀列表。
//
//	app/foo/bar.py → [app/foo/bar.py, foo/bar.py, bar.py]
//
// 为什么要逐级放宽：堆栈里的路径是**运行时**路径（容器内 /app/foo/bar.py），
// 而仓库里的布局可能是 src/foo/bar.py —— 完整路径匹配不上，但 foo/bar.py 这一级能对上。
func hintSuffixes(hint string) []string {
	hint = strings.Trim(strings.TrimSpace(hint), "/")
	if hint == "" {
		return nil
	}
	segments := strings.Split(hint, "/")
	out := make([]string, 0, len(segments))
	for i := range segments {
		out = append(out, strings.Join(segments[i:], "/"))
	}
	return out
}

// hintDirMatches 判断候选是否落在"线索里的最后一层目录"下。
//
// 用途：堆栈里的路径可能被裁剪（`bar.py` 其实来自 `app/foo/bar.py`），
// 这时目录名一致仍是有效信号。
func hintDirMatches(relLower, hintLower string) bool {
	dir := pathDir(hintLower)
	if dir == "" {
		return false
	}
	last := pathBase(dir)
	if last == "" {
		return false
	}
	return strings.Contains(relLower, "/"+last+"/")
}

// vendorDirs 是"依赖/构建产物"目录名（命中扣分，但不排除）。
var vendorDirs = map[string]bool{
	"vendor": true, "node_modules": true, "target": true, "build": true,
	"dist": true, "out": true, "bin": true, "obj": true, ".git": true,
	"third_party": true, "external": true, "site-packages": true,
	"__pycache__": true, ".venv": true, "venv": true, "bower_components": true,
	"generated": true, "gen": true, "coverage": true,
}

func hitVendorDir(relLower string) bool {
	dir := pathDir(relLower)
	if dir == "" {
		return false
	}
	for _, seg := range strings.Split(dir, "/") {
		if vendorDirs[seg] {
			return true
		}
	}
	return false
}

// isTestPath 判断是否落在测试目录或测试文件上。
//
// 降权而不是排除：有些仓库把"可执行规格"当源码（例如 scripts/test_*.py 是生产脚本），
// 排除会让这些服务永远定位不到东西。
func isTestPath(relLower string) bool {
	dir := pathDir(relLower)
	for _, seg := range strings.Split(dir, "/") {
		if seg == "test" || seg == "tests" || seg == "spec" || seg == "specs" ||
			seg == "__tests__" || seg == "testdata" || seg == "fixtures" {
			return true
		}
	}
	base := pathBase(relLower)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.py") || strings.HasSuffix(base, "test.py") ||
		strings.HasPrefix(base, "test") && strings.HasSuffix(base, ".java")
}

// walkSourceFiles 是 git 索引不可用时的兜底：遍历目录但跳过依赖/产物目录与 .git。
//
// 返回仓库内相对路径（与原实现返回绝对路径不同：对外只暴露相对路径，见 locateCode 的说明）。
func walkSourceFiles(root string) []string {
	out := make([]string, 0, 128)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := strings.ToLower(d.Name())
		if d.IsDir() {
			if path != root && vendorDirs[name] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	return out
}

// pathBase / pathDir 用 "/" 分隔处理仓库内相对路径（Windows 上 filepath 用的是 "\"，
// 但仓库内的相对路径统一以 "/" 表达，避免同一份数据两套分隔符）。
func pathBase(rel string) string {
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		return rel[idx+1:]
	}
	return rel
}

func pathDir(rel string) string {
	if idx := strings.LastIndex(rel, "/"); idx >= 0 {
		return rel[:idx]
	}
	return ""
}

// locateIndexInfo 描述从堆栈中提取的定位信息。
type locateIndexInfo struct {
	fileName string
	method   string
	// pathHint 是堆栈里给出的**路径线索**（相对仓库根的形态），用于给候选打分。
	// 空表示堆栈里只有文件名（例如 Java 的 `at a.b.C.m(C.java:1)` 里类名才是线索）。
	pathHint string
}

var (
	reJavaFrame = regexp.MustCompile(`at\s+([\w.$]+)\.(\w+)\(([\w.$]+)(?::(\d+))?\)`)
	rePyFrame   = regexp.MustCompile(`File\s+"([^"]+)",\s+line\s+(\d+),\s+in\s+(\w+)`)
	reGoFrame   = regexp.MustCompile(`([\w./-]+\.go):(\d+)`)
)

// locateIndex 从堆栈提取文件名、方法名与路径线索。
//
// 三种语言都**尽量带上路径线索**：只按文件名找，等于把"哪个同名文件"这件事交给
// 文件系统的遍历顺序（INC-032 的根因）。
func locateIndex(stacktrace string) *locateIndexInfo {
	if m := reJavaFrame.FindStringSubmatch(stacktrace); m != nil {
		file := m[3]
		if !strings.HasSuffix(file, ".java") {
			file += ".java"
		}
		className := strings.TrimSuffix(file, ".java")
		if idx := strings.LastIndex(className, "."); idx > 0 {
			className = className[idx+1:]
		}
		// 类名转路径：a.b.OrderService → a/b/OrderService.java（Java 的包名与目录一一对应）。
		hint := strings.ReplaceAll(m[1], ".", "/") + ".java"
		return &locateIndexInfo{fileName: className + ".java", method: m[2], pathHint: hint}
	}
	if m := rePyFrame.FindStringSubmatch(stacktrace); m != nil {
		// Python 的 traceback 给的是**运行时**路径（可能是容器内绝对路径），
		// 所以只把去掉前导斜杠后的整体当线索，打分时会逐级取后缀匹配。
		return &locateIndexInfo{
			fileName: filepath.Base(m[1]),
			method:   m[3],
			pathHint: strings.TrimPrefix(strings.ReplaceAll(m[1], "\\", "/"), "/"),
		}
	}
	if m := reGoFrame.FindStringSubmatch(stacktrace); m != nil {
		file := filepath.Base(m[1])
		name := strings.TrimSuffix(file, ".go")
		return &locateIndexInfo{
			fileName: file, method: name,
			pathHint: strings.TrimPrefix(strings.ReplaceAll(m[1], "\\", "/"), "./"),
		}
	}
	return nil
}

// parseCodeReport 解析代码分析输出（三点式）。
func parseCodeReport(content string) map[string]any {
	out := map[string]any{}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(content[start:end+1]), &out); err == nil {
			return out
		}
	}
	// 无法解析为 JSON 时按纯文本保留，保证信息不丢失。
	out["root_cause"] = truncateRunes(content, 800)
	out["confidence"] = 0.3
	out["emergency_plan"] = "输出未遵循 JSON 模板，请人工阅读原始结论"
	return out
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

// repoSummary 生成仓库摘要（不含本地绝对路径）。
func repoSummary(repo *model.CodeRepo) map[string]any {
	if repo == nil {
		return map[string]any{"configured": false}
	}
	return map[string]any{
		"configured": true, "service": repo.ServiceName, "repo_url": repo.RepoURL,
		"branch": repo.Branch, "language": repo.Language, "allow_third_party": repo.AllowThirdParty,
	}
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

// AnalyzeWithTimeout 是带任务级超时的分析入口（定时任务与手动触发共用）。
func (s *CodeAnalysisService) AnalyzeWithTimeout(parent context.Context, in CodeAnalysisRequest, operator Operator, deadline time.Duration) (*AnalyzeResult, error) {
	ctx, cancel := context.WithTimeout(parent, deadline)
	defer cancel()
	return s.Analyze(ctx, in, operator)
}
