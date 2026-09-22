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
	Question string
	// CallbackURL 为平台接收结论的地址。
	CallbackURL string
	// EventID 为触发这次分析的日志事件（0 表示手工提交）。
	EventID int64
}

// TaskResult 是提交/查询/回调三种入口统一出来的结果形状。
type TaskResult struct {
	TaskID string
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
	url := joinURL(c.cfg.BaseURL, c.cfg.SubmitPath)
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
		return nil, fmt.Errorf("提交 AI 分析任务失败：HTTP %d %s", resp.StatusCode, truncateText(string(body), 300))
	}
	res := parseTaskResult(body)
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

// Query 主动查询任务状态（回调的兜底路径）。
func (c *AIAnalysisClient) Query(ctx context.Context, taskID string) (*TaskResult, error) {
	if !c.Configured() {
		return nil, ErrAIAnalysisNotConfigured
	}
	path := strings.ReplaceAll(c.cfg.QueryPath, "{task_id}", taskID)
	url := joinURL(c.cfg.BaseURL, path)
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
		return nil, fmt.Errorf("查询 AI 分析任务失败：HTTP %d %s", resp.StatusCode, truncateText(string(body), 300))
	}
	res := parseTaskResult(body)
	if strings.TrimSpace(res.TaskID) == "" {
		res.TaskID = taskID
	}
	if res.Status == "" {
		res.Status = modelStatusSubmitted
	}
	return res, nil
}

// setAuth 统一注入调用凭据。
func (c *AIAnalysisClient) setAuth(req *http.Request) {
	if strings.TrimSpace(c.cfg.APIKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.cfg.APIKey))
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
