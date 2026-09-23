package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
)

// 本文件是「外部 AI 分析服务」的 HTTP 客户端，采用主流的**异步任务 + 回调**模型：
//
//	平台 --提交问题--> AI 服务 --返回 task_id--> 平台
//	AI 服务 --分析（慢，可能几分钟）--> 回调平台的接收接口
//	平台 --（兜底）按 task_id 轮询--> AI 服务
//
// 为什么不用"提交后同步等结果"：AI 侧做代码分析经常是分钟级，
// 同步等会把 worker 的这一轮全部堵住（后面的告警排队），HTTP 连接也随时可能被网关掐断。
//
// 为什么还需要轮询：回调依赖 AI 服务能访问到平台，也依赖它没有丢消息。
// 只有"回调 + 轮询"两条路都在，丢一次回调才不至于让告警永远停在分析中。

// ErrAIAnalysisNotConfigured 表示平台没有配置外部 AI 分析服务。
var ErrAIAnalysisNotConfigured = errors.New("未配置外部 AI 分析服务（ai_analysis.base_url 为空或开关关闭）")

// AIAnalysisClient 调用外部 AI 分析服务。
type AIAnalysisClient struct {
	cfg  config.AIAnalysisConfig
	http *http.Client
	log  *zap.Logger
}

// NewAIAnalysisClient 构造客户端（timeout 未配置时取 15 秒）。
func NewAIAnalysisClient(cfg config.AIAnalysisConfig, log *zap.Logger) *AIAnalysisClient {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if log == nil {
		log = zap.NewNop()
	}
	return &AIAnalysisClient{
		cfg:  cfg,
		http: &http.Client{Timeout: timeout},
		log:  log,
	}
}

// Configured 报告是否具备调用条件（地址与开关都有）。
func (c *AIAnalysisClient) Configured() bool {
	return c != nil && c.cfg.Enabled && strings.TrimSpace(c.cfg.BaseURL) != ""
}

// SubmitInput 是一次提交的内容。
type SubmitInput struct {
	// TaskID 是平台生成的任务号（幂等键）：先生成再提交，
	// 这样即使 AI 服务不回 task_id、或回调比提交响应先到，也能对上号。
	TaskID string
	// Service 为服务名（AI 侧据此知道要分析哪个服务）。
	Service string
	// Question 为问题正文（已脱敏的错误信息 + 堆栈）。
	// generic 协议的主输入就是它；开放接口则改用 Stacktrace + Logs 两个结构化字段。
	Question string
	// CallbackURL 为平台接收结论的地址。
	CallbackURL string
	// EventID 为触发这次分析的日志事件（0 表示手工提交）。
	EventID int64

	// ---- 开放接口（openapi_v1）专用 ----

	// Stacktrace 为异常栈原文（开放接口的必填字段）。
	Stacktrace string
	// Message 为错误信息首行（开放接口里作为 logs 辅助定位）。
	Message string
	// Environment 为环境标识（如 prod）。
	Environment string
	// IdempotencyKey 为幂等键：相同 key 在服务端有效期内会复用同一分析任务，
	// 告警风暴下同一条错误只分析一次。
	IdempotencyKey string
	// Sync 为 true 表示本次走同步端点（开放接口下同步与异步是两个不同路径）。
	Sync bool
}

// TaskResult 是提交/查询/回调三种入口统一出来的结果形状。
type TaskResult struct {
	TaskID string
	// RunID 为开放接口的运行 ID（轮询与报告地址按它组织）；generic 协议下为空。
	RunID string
	// Status 取值：submitted（仍在处理）/ succeeded（有结论）/ failed（明确失败）。
	Status string
	// Answer 为结论原文（可能为空：还在处理时）。
	Answer string
	// Error 为 AI 侧给出的失败原因。
	Error string
}

// submitPayload 是提交请求的正文。
//
// 字段名用最通用的命名（question / service / callback_url），
// 并把平台自己的 task_id 也带上：多数 AI 服务都接受"调用方指定任务号"。
type submitPayload struct {
	TaskID      string `json:"task_id"`
	Service     string `json:"service"`
	Question    string `json:"question"`
	CallbackURL string `json:"callback_url"`
	EventID     int64  `json:"event_id,omitempty"`
}

// Submit 提交一次分析任务（异步模式用）：默认短超时，只拿 task_id 就返回。
//
// 异步模式不关心"等多久出结论"：结论由回调或轮询带回，提交阶段只要把任务挂上去即可。
func (c *AIAnalysisClient) Submit(ctx context.Context, in SubmitInput) (*TaskResult, error) {
	return c.doSubmit(ctx, in, c.http)
}

// SubmitWithTimeout 提交并原地等待 AI 服务返回结论，最长 wait 时长。
//
// 用于"同步调用"模式：AI 服务在提交接口里就把分析做完一起返回，平台不再依赖回调。
// 注意 c.http 的超时是短超时（默认 15s），同步等几分钟必须换成更长的新 client，
// 否则 HTTP 层会在 AI 服务还在分析时就掐断连接。
func (c *AIAnalysisClient) SubmitWithTimeout(ctx context.Context, in SubmitInput, wait time.Duration) (*TaskResult, error) {
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	return c.doSubmit(waitCtx, in, &http.Client{Timeout: wait})
}

// doSubmit 是 Submit / SubmitWithTimeout 共享的提交执行；cli 由调用方决定（不同超时）。
func (c *AIAnalysisClient) doSubmit(ctx context.Context, in SubmitInput, cli *http.Client) (*TaskResult, error) {
	if !c.Configured() {
		return nil, ErrAIAnalysisNotConfigured
	}
	if cli == nil {
		cli = c.http
	}
	payload, err := c.marshalSubmit(ctx, in)
	if err != nil {
		return nil, err
	}
	url := joinURL(c.cfg.BaseURL, c.submitPath(in.Sync))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造提交请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("提交 AI 分析任务失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取提交响应失败: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("提交 AI 分析任务失败：%s", httpErrorHint(resp.StatusCode, body, c.isOpenAPI()))
	}
	res := c.parseResult(body)
	// AI 服务不认我们的 task_id 时以它返回的为准；都不给就用我们自己生成的那个。
	if strings.TrimSpace(res.TaskID) == "" {
		res.TaskID = in.TaskID
	}
	if res.Status == "" {
		res.Status = modelStatusSubmitted
	}
	c.log.Info("已提交 AI 分析任务",
		zap.String("task_id", res.TaskID), zap.String("status", res.Status),
		zap.String("service", in.Service), zap.Duration("wait", ctxTimeout(ctx)))
	return res, nil
}

// ctxTimeout 取 context 剩余的等待时长（仅用于日志，拿不到就返回 0）。
func ctxTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl)
	}
	return 0
}

// ProbeResult 是连通性探测的结果（只回答"能不能打通"，不产出结论）。
type ProbeResult struct {
	// StatusCode 为 AI 服务返回的 HTTP 状态码（连接失败时不会走到这里）。
	StatusCode int
	// Message 为本次探测拿到的任务状态（如 succeeded / running），可能为空。
	Message string
	// LatencyMS 为往返耗时。
	LatencyMS int64
}

// probeServiceName 是探测用的服务名：它大概率没在 AI 服务侧注册，
// 因此返回 404/422 反而能证明"地址、鉴权、报文形状"三件事都对。
const probeServiceName = "connectivity-probe"

// probeTimeout 是探测的单次上限。
//
// 刻意比正常提交短得多：这是设置页上等人看的按钮，卡几十秒没人受得了；
// 同时它也是开放接口同步请求里的 timeout 字段（服务端最多分析这么久就回 202）。
const probeTimeout = 10 * time.Second

// Probe 做一次连通性探测：向同步端点发一个最小请求，只关心"能不能打通"。
//
// 为什么走同步端点：开放接口的异步端点要求 callbackUrl 必填，而探测时
// 平台的回调地址未必已配好——为了测连通性先逼着配回调是本末倒置。
// 同步端点不需要回调，正好用来做这件事。
func (c *AIAnalysisClient) Probe(ctx context.Context) (*ProbeResult, error) {
	if !c.Configured() {
		return nil, ErrAIAnalysisNotConfigured
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	in := SubmitInput{
		TaskID:     fmt.Sprintf("probe-%d", time.Now().UnixNano()),
		Service:    probeServiceName,
		Question:   "connectivity probe",
		Stacktrace: "connectivity probe",
		Sync:       true,
	}
	payload, err := c.marshalSubmit(probeCtx, in)
	if err != nil {
		return nil, err
	}
	url := joinURL(c.cfg.BaseURL, c.submitPath(true))
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("构造探测请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	start := time.Now()
	resp, err := (&http.Client{Timeout: probeTimeout}).Do(req)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		return nil, fmt.Errorf("连接 AI 分析服务失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		// 4xx/5xx 也是"打通了"的证据：把状态码带回去，由调用方翻译成人能看懂的原因。
		return &ProbeResult{StatusCode: resp.StatusCode, LatencyMS: latency}, nil
	}
	return &ProbeResult{
		StatusCode: resp.StatusCode,
		Message:    c.parseResult(body).Status,
		LatencyMS:  latency,
	}, nil
}

// Query 主动查询任务状态（回调的兜底路径）。
//
// taskID 与 runID 都传：开放接口的查询地址按 runId 组织（GET /api/v1/runs/{runId}），
// 而 generic 协议只有 task_id。取哪个由协议决定，调用方不必关心。
func (c *AIAnalysisClient) Query(ctx context.Context, taskID, runID string) (*TaskResult, error) {
	if !c.Configured() {
		return nil, ErrAIAnalysisNotConfigured
	}
	url := joinURL(c.cfg.BaseURL, c.queryPath(taskID, runID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造查询请求失败: %w", err)
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("查询 AI 分析任务失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取查询响应失败: %w", err)
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("查询 AI 分析任务失败：%s", httpErrorHint(resp.StatusCode, body, c.isOpenAPI()))
	}
	res := c.parseResult(body)
	if strings.TrimSpace(res.TaskID) == "" {
		res.TaskID = taskID
	}
	if res.Status == "" {
		res.Status = modelStatusSubmitted
	}
	return res, nil
}

// setAuth 统一注入调用凭据。
//
// 头名可配（AuthHeader）：开放接口按《CodeAgent 接入文档》默认用 X-API-Key: <key>
// （不带 Bearer 前缀），而 generic 的历史实现是 Authorization: Bearer <key>，两者不能混用。
// 不显式配置 AuthHeader 时，按协议自动选择：openapi_v1 → X-API-Key，generic → Bearer。
func (c *AIAnalysisClient) setAuth(req *http.Request) {
	key := strings.TrimSpace(c.cfg.APIKey)
	if key == "" {
		return
	}
	header := strings.TrimSpace(c.cfg.AuthHeader)
	if header == "" {
		if c.isOpenAPI() {
			req.Header.Set("X-API-Key", key)
			return
		}
		req.Header.Set("Authorization", "Bearer "+key)
		return
	}
	if strings.EqualFold(header, "Authorization") {
		req.Header.Set(header, "Bearer "+key)
		return
	}
	req.Header.Set(header, key)
}

// ---------------------------------------------------------------------------
// 协议分派
// ---------------------------------------------------------------------------

// 对接协议取值。
const (
	// ProtocolGeneric 是平台自研的"提交问题—回调结论"形状（历史默认）。
	ProtocolGeneric = "generic"
	// ProtocolOpenAPIV1 是《AI 代码分析接口文档 v1》的形状。
	ProtocolOpenAPIV1 = "openapi_v1"
)

// 开放接口的默认端点（文档 §5 / §6 / 轮询）。
const (
	openAPISubmitPathAsync = "/api/v1/openapi/tasks"
	openAPISubmitPathSync  = "/api/v1/openapi/analyze"
	openAPIQueryPath       = "/api/v1/runs/{run_id}"
	// openAPIMaxTimeout 是文档规定的同步等待上限（秒）。
	openAPIMaxTimeout = 300
)

// isOpenAPI 报告当前是否走开放接口协议。
func (c *AIAnalysisClient) isOpenAPI() bool {
	return c != nil && strings.EqualFold(strings.TrimSpace(c.cfg.Protocol), ProtocolOpenAPIV1)
}

// submitPath 按协议与调用方式选择提交端点。
//
// 开放接口的同步与异步是两个不同端点，因此 call_mode 一变就必须换路径——
// 这也是为什么配置里要单独留一个 SyncSubmitPath（generic 协议下它可选）。
func (c *AIAnalysisClient) submitPath(sync bool) string {
	if c.isOpenAPI() {
		if sync {
			return defaultString(strings.TrimSpace(c.cfg.SyncSubmitPath), openAPISubmitPathSync)
		}
		return defaultString(strings.TrimSpace(c.cfg.SubmitPath), openAPISubmitPathAsync)
	}
	if sync {
		return defaultString(strings.TrimSpace(c.cfg.SyncSubmitPath), c.cfg.SubmitPath)
	}
	return c.cfg.SubmitPath
}

// queryPath 按协议拼出查询路径并替换占位符（同时支持 {task_id} 与 {run_id}）。
func (c *AIAnalysisClient) queryPath(taskID, runID string) string {
	// 仍是 generic 的默认路径（说明管理员没改过）时，开放接口必须让位给 /api/v1/runs/{run_id}，
	// 否则轮询会打到一个不存在的端点上。setting 层在切换协议时也会做同样的改写，这里是双保险。
	path := strings.TrimSpace(c.cfg.QueryPath)
	if c.isOpenAPI() && (path == "" || path == defaultQueryPath) {
		path = openAPIQueryPath
	}
	path = strings.ReplaceAll(path, "{run_id}", runID)
	return strings.ReplaceAll(path, "{task_id}", taskID)
}

// marshalSubmit 按协议序列化提交正文。
func (c *AIAnalysisClient) marshalSubmit(ctx context.Context, in SubmitInput) ([]byte, error) {
	if c.isOpenAPI() {
		return json.Marshal(c.openAPIPayload(ctx, in))
	}
	payload, err := json.Marshal(submitPayload{
		TaskID:      in.TaskID,
		Service:     in.Service,
		Question:    in.Question,
		CallbackURL: in.CallbackURL,
		EventID:     in.EventID,
	})
	if err != nil {
		return nil, fmt.Errorf("序列化提交请求失败: %w", err)
	}
	return payload, nil
}

// openAPIRepoLocator 是开放接口的仓库定位字段（文档 §4）。
type openAPIRepoLocator struct {
	GitURL string `json:"gitUrl,omitempty"`
	Host   string `json:"host,omitempty"`
}

// openAPISubmitPayload 是开放接口的提交正文（文档 §5.1 / §6.1）。
//
// 字段顺序按文档排列；可选字段一律 omitempty，避免把零值塞给服务端
// （例如 priority=0 会被当成"最高优先级"而不是"不指定"）。
type openAPISubmitPayload struct {
	Mode           string              `json:"mode"`
	RepoID         string              `json:"repoId,omitempty"`
	RepoLocator    *openAPIRepoLocator `json:"repoLocator,omitempty"`
	Stacktrace     string              `json:"stacktrace"`
	Logs           string              `json:"logs,omitempty"`
	EntryFiles     []string            `json:"entryFiles,omitempty"`
	Environment    string              `json:"environment,omitempty"`
	AutoVerify     *bool               `json:"autoVerify,omitempty"`
	Priority       *int                `json:"priority,omitempty"`
	IdempotencyKey string              `json:"idempotencyKey,omitempty"`
	CallbackURL    string              `json:"callbackUrl,omitempty"`
	Timeout        *int                `json:"timeout,omitempty"`
}

// openAPIPayload 把平台侧的提交入参翻译成开放接口的正文。
func (c *AIAnalysisClient) openAPIPayload(ctx context.Context, in SubmitInput) openAPISubmitPayload {
	out := openAPISubmitPayload{
		Mode:           openAPIModeAnalyze,
		Stacktrace:     c.openAPIStacktrace(in),
		Logs:           strings.TrimSpace(in.Message),
		Environment:    strings.TrimSpace(defaultString(in.Environment, c.cfg.Environment)),
		IdempotencyKey: defaultString(strings.TrimSpace(in.IdempotencyKey), in.TaskID),
		CallbackURL:    strings.TrimSpace(in.CallbackURL),
	}
	// 仓库定位（开放接口）：
	// 1) 服务名在「服务名 → git 地址」映射表命中 → 直接发该 gitUrl（按服务分流到各自仓库，最高优先级）；
	// 2) 全局 git_url 模式且配了 RepoGitURL → 发固定 gitUrl（单仓场景）；
	// 3) 否则把服务名当 host 提交，依赖 AI 侧 matchRules 匹配（hostPatterns/keywords）。
	// 仓库定位（文档 §4，优先级 repoId > repoLocator）：repoId 命中直接用，否则回落 gitUrl/host 方案。
	if repoID := c.resolveRepoID(strings.TrimSpace(in.Service)); repoID != "" {
		out.RepoID = repoID
	} else if gitURL := c.resolveRepoGitURL(strings.TrimSpace(in.Service)); gitURL != "" {
		out.RepoLocator = &openAPIRepoLocator{GitURL: gitURL}
	} else if strings.EqualFold(strings.TrimSpace(c.cfg.RepoLocatorMode), repoLocatorModeGitURL) &&
		strings.TrimSpace(c.cfg.RepoGitURL) != "" {
		out.RepoLocator = &openAPIRepoLocator{GitURL: strings.TrimSpace(c.cfg.RepoGitURL)}
	} else if strings.TrimSpace(in.Service) != "" {
		out.RepoLocator = &openAPIRepoLocator{Host: strings.TrimSpace(in.Service)}
	}
	if c.cfg.Priority > 0 {
		p := c.cfg.Priority
		out.Priority = &p
	}
	if c.cfg.AutoVerify {
		v := true
		out.AutoVerify = &v
	}
	// 同步模式把等待上限一并告诉服务端（文档上限 300 秒，超时它会回 202）。
	if remain := ctxTimeout(ctx); remain > 0 {
		secs := int(remain.Seconds())
		if secs > openAPIMaxTimeout {
			secs = openAPIMaxTimeout
		}
		out.Timeout = &secs
	}
	return out
}

// openAPIModeAnalyze 是开放接口的提交模式（文档 §一 示例 mode=analyze）。
const openAPIModeAnalyze = "analyze"

// resolveRepoGitURL 在「服务名 → git 地址」映射表里查服务名；命中且地址非空时返回该地址。
// 用于开放接口下按服务名定位仓库，避免依赖 AI 服务侧的 matchRules（hostPatterns/keywords）。
func (c *AIAnalysisClient) resolveRepoGitURL(service string) string {
	if strings.TrimSpace(service) == "" || c.cfg.ServiceRepoMap == nil {
		return ""
	}
	return strings.TrimSpace(c.cfg.ServiceRepoMap[service])
}

// resolveRepoID 在「服务名 → repoId」映射表里查服务名；命中且非空时返回该 repoId（文档 §4 优先级最高）。
func (c *AIAnalysisClient) resolveRepoID(service string) string {
	if strings.TrimSpace(service) == "" || c.cfg.ServiceRepoIDMap == nil {
		return ""
	}
	return strings.TrimSpace(c.cfg.ServiceRepoIDMap[service])
}

// openAPIStacktrace 取异常栈：栈为空时用错误信息顶上。
//
// 为什么必须兜底：开放接口把 stacktrace 定为必填，缺了直接 400；
// 而真实日志里有相当一部分只有一行错误信息、没有堆栈（例如业务异常）。
// 宁可让 AI 侧拿到一行文本，也不能让整次分析因为字段缺失失败。
func (c *AIAnalysisClient) openAPIStacktrace(in SubmitInput) string {
	if s := strings.TrimSpace(in.Stacktrace); s != "" {
		return s
	}
	return strings.TrimSpace(in.Message)
}

// parseResult 按协议解析 AI 服务的响应。
func (c *AIAnalysisClient) parseResult(body []byte) *TaskResult {
	if c.isOpenAPI() {
		return parseOpenAPIResult(body)
	}
	return parseTaskResult(body)
}

// ---------------------------------------------------------------------------
// 开放接口：响应解析
// ---------------------------------------------------------------------------

// openAPIResultPayload 是开放接口同步/受理响应的结构（文档 §5.3 / §6.3）。
//
// 只声明平台真正会用的字段：未声明的字段被丢弃不影响分析，
// 但"结论"相关的字段一个都不能少（少了就是"分析成功但没有内容"）。
type openAPIResultPayload struct {
	RunID     string             `json:"runId"`
	TaskID    string             `json:"taskId"`
	Status    string             `json:"status"`
	Severity  string             `json:"severity"`
	Repo      map[string]any     `json:"repo"`
	RootCause *openAPIRootCause  `json:"rootCause"`
	Patches   []openAPIPatch     `json:"patches"`
	ReportID  string             `json:"reportId"`
	ReportURL string             `json:"reportUrl"`
	Markdown  string             `json:"markdown"`
	Warnings  []string           `json:"warnings"`
	Degraded  bool               `json:"degraded"`
	Error     string             `json:"error"`
	Message   string             `json:"message"`
}

// openAPIRootCause 是根因结构（文档 §5.5）。
type openAPIRootCause struct {
	Summary     string   `json:"summary"`
	Category    string   `json:"category"`
	Confidence  float64  `json:"confidence"`
	Detail      string   `json:"detail"`
	Evidence    []string `json:"evidence"`
	BlastRadius []string `json:"blastRadius"`
}

// openAPIPatch 是候选补丁（文档 §5.5）。
type openAPIPatch struct {
	RepositoryID string `json:"repositoryId"`
	RepoKey      string `json:"repoKey"`
	FilePath     string `json:"filePath"`
	Action       string `json:"action"`
	UnifiedDiff  string `json:"unifiedDiff"`
	Rationale    string `json:"rationale"`
}

// parseOpenAPIResult 把开放接口的响应翻译成平台内部的 TaskResult。
//
// 关键取舍：结论被合成一段**平台内部约定的 JSON**（root_cause / fix_suggestion /
// impact_scope / confidence / located_file / report_url / markdown …），
// 而不是把原始报文原样塞进 Answer。这样下游的 parseCodeReport、buildBrief、
// saveReport 一行都不用改，且报告里能保留 reportUrl 与 markdown 供后续使用。
func parseOpenAPIResult(body []byte) *TaskResult {
	var raw openAPIResultPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		// 不是开放接口的 JSON（例如网关返回的纯文本错误页）：退回宽松解析，
		// 至少别把整次提交判成"成功但没结论"。
		return parseTaskResult(body)
	}
	res := &TaskResult{
		TaskID: defaultString(strings.TrimSpace(raw.TaskID), strings.TrimSpace(raw.RunID)),
		RunID:  strings.TrimSpace(raw.RunID),
		Status: normalizeOpenAPIStatus(raw.Status, raw.Degraded),
		Answer: openAPIAnswerFrom(raw),
		Error:  strings.TrimSpace(raw.Error),
	}
	if res.Status == modelStatusFailed && res.Error == "" {
		res.Error = defaultString(strings.TrimSpace(raw.Message), "AI 服务报告分析失败")
	}
	return res
}

// normalizeOpenAPIStatus 把开放接口的终态映射到平台三态。
//
// 与 generic 的差别是 needs_review / degraded 也按"有结论"处理：
// 它们是终态而不是"还在跑"，当成没完成会白等到 task_timeout。
func normalizeOpenAPIStatus(status string, degraded bool) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "needs_review", "degraded":
		return modelStatusSucceeded
	case "failed", "cancelled":
		return modelStatusFailed
	default:
		// queued / running / 未知：仍在处理。degraded 但状态缺失时也按有结论处理，
		// 因为服务端只有在分析结束后才会带上 degraded 标记。
		if degraded {
			return modelStatusSucceeded
		}
		return ""
	}
}

// openAPIAnswerFrom 把开放接口的结论合成平台内部约定的 JSON 文本。
func openAPIAnswerFrom(raw openAPIResultPayload) string {
	if raw.RootCause == nil && len(raw.Patches) == 0 && strings.TrimSpace(raw.Markdown) == "" {
		return ""
	}
	out := map[string]any{}
	if rc := raw.RootCause; rc != nil {
		root := strings.TrimSpace(rc.Summary)
		if detail := strings.TrimSpace(rc.Detail); detail != "" {
			root = strings.TrimSpace(root + "\n\n" + detail)
		}
		out["root_cause"] = root
		out["category"] = strings.TrimSpace(rc.Category)
		out["confidence"] = rc.Confidence
		out["impact_scope"] = strings.Join(nonEmpty(rc.BlastRadius), "；")
		out["evidence"] = nonEmpty(rc.Evidence)
	}
	if len(raw.Patches) > 0 {
		lines := make([]string, 0, len(raw.Patches))
		for _, p := range raw.Patches {
			line := strings.TrimSpace(p.FilePath)
			if r := strings.TrimSpace(p.Rationale); r != "" {
				line = strings.TrimSpace(line + "：" + r)
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
		out["fix_suggestion"] = strings.Join(lines, "\n")
		if file := strings.TrimSpace(raw.Patches[0].FilePath); file != "" {
			out["located_file"] = file
		}
	}
	if s := strings.TrimSpace(raw.Severity); s != "" {
		out["severity"] = s
	}
	if s := strings.TrimSpace(raw.ReportURL); s != "" {
		out["report_url"] = s
	}
	if s := strings.TrimSpace(raw.ReportID); s != "" {
		out["report_id"] = s
	}
	if s := strings.TrimSpace(raw.Markdown); s != "" {
		out["markdown"] = s
	}
	if len(raw.Warnings) > 0 {
		out["warnings"] = nonEmpty(raw.Warnings)
	}
	if raw.Degraded {
		out["degraded"] = true
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		// 极端情况下合成失败：至少把摘要带回去，不能让结论整段丢失。
		if raw.RootCause != nil {
			return strings.TrimSpace(raw.RootCause.Summary)
		}
		return ""
	}
	return string(encoded)
}

// nonEmpty 过滤掉空白字符串。
func nonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s := strings.TrimSpace(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// httpErrorHint 把 HTTP 错误码翻译成给使用者看的原因。
//
// 开放接口的 404/422 有明确含义（仓库定位失败），直接把状态码抛出去，
// 管理员只会看到"提交失败：HTTP 422"而不知道要去哪修。
func httpErrorHint(status int, body []byte, openAPI bool) string {
	hint := fmt.Sprintf("HTTP %d %s", status, truncateText(string(body), 300))
	if !openAPI {
		return hint
	}
	switch status {
	case 400:
		return hint + "（请求参数不合法：常见原因是缺 stacktrace、异步模式缺 callbackUrl，或栈超过 maxStacktraceBytes）"
	case 401:
		return hint + "（API Key 无效或未携带：请确认「鉴权头」填的是 X-API-Key 且密钥正确）"
	case 403:
		return hint + "（密钥缺少 task:write 权限）"
	case 404, 422:
		return hint + "（仓库定位失败：请先在 AI 服务控制台注册该仓库并配置 hostPatterns/keywords，或改用固定 gitUrl）"
	case 429:
		return hint + "（配额或队列超限：请下调提交频率，稍后会自动重试）"
	default:
		return hint
	}
}

// parseTaskResult 解析 AI 服务的响应。
//
// 刻意做成"宽松解析"：不同 AI 服务的字段名各不相同（task_id/id、status/state、
// answer/result/content…），逐个适配不现实；认不出来的字段只是少一点信息，
// 不该让整次分析失败。status 会归一化到 submitted/succeeded/failed 三态。
func parseTaskResult(body []byte) *TaskResult {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		// 不是 JSON：把原文当结论（有些服务直接返回纯文本）。
		text := strings.TrimSpace(string(body))
		if text == "" {
			return &TaskResult{}
		}
		return &TaskResult{Status: modelStatusSucceeded, Answer: text}
	}
	res := &TaskResult{
		TaskID: firstString(raw, "task_id", "taskId", "id", "job_id"),
		Status: normalizeTaskStatus(firstString(raw, "status", "state", "result_status")),
		Answer: firstString(raw, "answer", "result", "content", "output", "answer_text", "report"),
		Error:  firstString(raw, "error", "error_message", "message", "reason"),
	}
	// 有些服务把结论放在 data 里。
	if res.Answer == "" {
		if data, ok := raw["data"].(map[string]any); ok {
			res.TaskID = defaultString(res.TaskID, firstString(data, "task_id", "taskId", "id"))
			res.Answer = firstString(data, "answer", "result", "content", "output", "report")
			if res.Status == "" {
				res.Status = normalizeTaskStatus(firstString(data, "status", "state"))
			}
			if res.Error == "" {
				res.Error = firstString(data, "error", "error_message", "message")
			}
		}
	}
	return res
}

// normalizeTaskStatus 把各种写法归一化成三态。
//
// 认不出来的状态一律按"还在处理"：过早判定失败会把一次本来会成功的分析掐掉，
// 而"继续等"最多是晚一点，超时由 DeadlineAt 兜底。
func normalizeTaskStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "succeeded", "success", "successful", "done", "completed", "complete", "finished", "ok":
		return modelStatusSucceeded
	case "failed", "failure", "error", "errored", "cancelled", "canceled":
		return modelStatusFailed
	default:
		return ""
	}
}

// firstString 按顺序取第一个非空字符串字段。
func firstString(raw map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := raw[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// joinURL 拼接服务地址与路径（路径为空时退回根路径）。
func joinURL(base, path string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	p := strings.TrimSpace(path)
	if p == "" {
		return base
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return base + p
}

// 任务状态（本地语义，与 model 的常量分开：这里是"AI 服务侧"的三态）。
const (
	modelStatusSubmitted = "submitted"
	modelStatusSucceeded = "succeeded"
	modelStatusFailed    = "failed"
)

// ParseCallback 解析 AI 服务回调过来的正文，直接给出平台侧的任务状态。
//
// 为什么回调的"空状态"按**成功**处理：AI 服务只在分析完成后才会回调，
// 它把状态写在哪一个字段里没有统一约定；认不出状态就当"有结论了"，
// 结论为空的情况由 CompleteByTask 再兜一道（转成失败并写明原因）。
func ParseCallback(body []byte) (taskID, status, answer, errMsg string, err error) {
	return ParseCallbackWithProtocol(body, ProtocolGeneric)
}

// ParseCallbackWithProtocol 按协议解析回调正文。
//
// 开放接口的回调报文与同步响应**不是**同一套字段（文档 §6.4）：
// 它用 state 表示终态、用 summary + rootCause + patches 表示结论，
// 既没有 answer 也没有 status/result。若沿用 generic 的宽松解析，
// 结论会被判成"返回成功但没有内容"。
func ParseCallbackWithProtocol(body []byte, protocol string) (taskID, status, answer, errMsg string, err error) {
	if strings.EqualFold(strings.TrimSpace(protocol), ProtocolOpenAPIV1) {
		return parseOpenAPICallback(body)
	}
	res := parseTaskResult(body)
	if strings.TrimSpace(res.TaskID) == "" {
		return "", "", "", "", errors.New("回调缺少 task_id，无法对上分析任务")
	}
	status = model.AIAnalysisTaskSucceeded
	if res.Status == modelStatusFailed {
		status = model.AIAnalysisTaskFailed
	}
	return res.TaskID, status, res.Answer, res.Error, nil
}

// openAPICallbackPayload 是开放接口的终态回调报文（文档 §6.4）。
type openAPICallbackPayload struct {
	RunID     string            `json:"runId"`
	State     string            `json:"state"`
	ReportID  string            `json:"reportId"`
	Severity  string            `json:"severity"`
	Summary   string            `json:"summary"`
	TenantID  string            `json:"tenantId"`
	TaskID    string            `json:"taskId"`
	Attempt   int               `json:"attempt"`
	Error     string            `json:"error"`
	RootCause *openAPIRootCause `json:"rootCause"`
	Patches   []openAPIPatch    `json:"patches"`
	ReportURL string            `json:"reportUrl"`
}

// parseOpenAPICallback 解析开放接口的回调。
func parseOpenAPICallback(body []byte) (taskID, status, answer, errMsg string, err error) {
	var raw openAPICallbackPayload
	if e := json.Unmarshal(body, &raw); e != nil {
		return "", "", "", "", errors.New("回调不是合法的开放接口报文：" + e.Error())
	}
	// 对号用 taskId（平台提交后存的就是它）；都没有就只能退回 runId。
	taskID = defaultString(strings.TrimSpace(raw.TaskID), strings.TrimSpace(raw.RunID))
	if taskID == "" {
		return "", "", "", "", errors.New("回调缺少 taskId/runId，无法对上分析任务")
	}
	status = model.AIAnalysisTaskSucceeded
	if normalizeOpenAPIStatus(raw.State, false) == modelStatusFailed {
		status = model.AIAnalysisTaskFailed
	}
	// 回调与同步响应的结论字段一一对应，复用同一个合成函数，
	// 保证"回调拿到的结论"与"轮询拿到的结论"在报告里长得一样。
	answer = openAPIAnswerFrom(openAPIResultPayload{
		RunID:     raw.RunID,
		TaskID:    raw.TaskID,
		Severity:  raw.Severity,
		RootCause: raw.RootCause,
		Patches:   raw.Patches,
		ReportID:  raw.ReportID,
		ReportURL: raw.ReportURL,
	})
	// 回调报文不带 markdown；有 summary 时以它兜底，避免"成功但没内容"。
	if strings.TrimSpace(answer) == "" && strings.TrimSpace(raw.Summary) != "" {
		answer = strings.TrimSpace(raw.Summary)
	}
	return taskID, status, answer, strings.TrimSpace(raw.Error), nil
}
