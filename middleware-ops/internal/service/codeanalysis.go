package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
	log      *zap.Logger
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
	log *zap.Logger,
) *CodeAnalysisService {
	return &CodeAnalysisService{
		cfg: cfg, engine: eng, events: events, repos: repos, analyses: analyses,
		redactor: redactor, audit: audit, cost: cost, log: log,
	}
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
		} else if !repository.EnsureNotFound(err) {
			s.log.Warn("查询服务仓库映射失败", zap.String("service", serviceName), zap.Error(err))
		}
	}
	thirdPartyEnabled := repo != nil && repo.AllowThirdParty && !in.ForceLocal
	if thirdPartyEnabled {
		if !inWhitelist(s.cfg.Security.OutboundWhitelist, serviceName) {
			thirdPartyEnabled = false
			warnings = append(warnings, "服务 "+serviceName+" 不在出网白名单内，已自动降级为本地分析")
		} else {
			outboundOK = true
		}
	} else if repo == nil {
		warnings = append(warnings, "未配置服务 "+serviceName+" 的仓库映射，按本地分析处理")
	}

	// ② 脱敏：堆栈去 IP / 用户名 / 手机号 / 请求 ID。
	redactedStack := s.redactor.Redact(stack)
	redactedMessage := s.redactor.Redact(message)

	// ③ 本地检索：堆栈定位文件行 → 上下文切片（合规兜底）。
	snippet, locatedFile, locatedLine, snippetTruncated := s.locateCode(repo, stack)
	if snippetTruncated {
		warnings = append(warnings, fmt.Sprintf("代码片段超过 %d 行上限，已截断", s.redactor.MaxLines()))
	}
	contextText := s.redactor.Redact(snippet)

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
		"third_party":         thirdPartyEnabled,
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
				"event_id": in.EventID, "service": serviceName, "outbound": thirdPartyEnabled,
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

// locateCode 依据堆栈定位本地代码文件并切片。
//
// 一期为「简单检索」：正则提取类名/文件名与方法名，在仓库本地路径下按文件名搜索，
// 命中后截取方法附近 N 行作为上下文（4.8.3 备选方案）。
func (s *CodeAnalysisService) locateCode(repo *model.CodeRepo, stacktrace string) (snippet, file string, line int, truncated bool) {
	if repo == nil || strings.TrimSpace(repo.LocalPath) == "" || strings.TrimSpace(stacktrace) == "" {
		return "", "", 0, false
	}
	index := locateIndex(stacktrace)
	if index == nil {
		return "", "", 0, false
	}
	root := repo.LocalPath
	found := ""
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.EqualFold(d.Name(), index.fileName) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
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
	return body, found, start + 1, truncated
}

// locateIndexInfo 描述从堆栈中提取的定位信息。
type locateIndexInfo struct {
	fileName string
	method   string
}

var (
	reJavaFrame = regexp.MustCompile(`at\s+([\w.$]+)\.(\w+)\(([\w.$]+)(?::(\d+))?\)`)
	rePyFrame   = regexp.MustCompile(`File\s+"([^"]+)",\s+line\s+(\d+),\s+in\s+(\w+)`)
	reGoFrame   = regexp.MustCompile(`([\w./-]+\.go):(\d+)`)
)

// locateIndex 从堆栈提取文件名与方法名。
func locateIndex(stacktrace string) *locateIndexInfo {
	if m := reJavaFrame.FindStringSubmatch(stacktrace); m != nil {
		file := m[3]
		if !strings.HasSuffix(file, ".java") {
			file += ".java"
		}
		if idx := strings.LastIndex(file, "."); idx > 0 {
			file = file[idx+1:]
		}
		return &locateIndexInfo{fileName: file, method: m[2]}
	}
	if m := rePyFrame.FindStringSubmatch(stacktrace); m != nil {
		file := filepath.Base(m[1])
		return &locateIndexInfo{fileName: file, method: m[3]}
	}
	if m := reGoFrame.FindStringSubmatch(stacktrace); m != nil {
		file := filepath.Base(m[1])
		name := strings.TrimSuffix(file, ".go")
		return &locateIndexInfo{fileName: file, method: name}
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
