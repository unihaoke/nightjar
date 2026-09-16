package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/pkg/cache"
)

// errEmptyResult 表示 Prometheus 正常响应、但选择器没有匹配到任何时序。
//
// 必须与「请求失败」区分开：
//   - 空结果 = 接入配置错（实例名/标签/job 对不上），是**真实故障信号**，不能降级掩盖；
//   - 请求失败 = 上游不可用，此时才允许回退模拟器保证页面可用。
//
// 早期实现把两者都当作普通 error，导致 Prometheus 完全不可达时快照依然"成功"
// （全部指标 unknown），回退模拟器成了死代码；同时"选择器无匹配"又被历史查询
// 静默替换成模拟曲线，把接入错误彻底藏了起来。
var errEmptyResult = errors.New("查询结果为空")

// promClient 通过 Prometheus HTTP API 查询指标（4.2：不自建采集器）。
type promClient struct {
	baseURL   string
	jobPrefix string
	timeout   time.Duration
	cacheTTL  time.Duration
	store     cache.Store
	client    *http.Client
	log       *zap.Logger
}

// NewPrometheusClient 构造 Prometheus 查询客户端。
func NewPrometheusClient(cfg *config.Config, store cache.Store, log *zap.Logger) Client {
	return &promClient{
		baseURL:   strings.TrimRight(cfg.Prometheus.BaseURL, "/"),
		jobPrefix: cfg.Prometheus.ExporterJobPrefix,
		timeout:   cfg.Prometheus.Timeout,
		cacheTTL:  cfg.Prometheus.CacheTTL,
		store:     store,
		client:    &http.Client{Timeout: cfg.Prometheus.Timeout},
		log:       log,
	}
}

// Kind 返回实现类型。
func (p *promClient) Kind() string { return "prometheus" }

// Healthy 探测 Prometheus 可用性。
func (p *promClient) Healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/-/healthy", nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode < 400
}

// Selector 返回该实例的 PromQL 标签匹配串（实现 SelectorReporter）。
func (p *promClient) Selector(target Target) string { return buildSelector(target, p.jobPrefix) }

// Endpoint 返回 Prometheus 查询地址（实现 EndpointReporter）。
func (p *promClient) Endpoint() string { return p.baseURL }

// maxTargetStatus 限制返回的目标条数（只用于提示，不需要全量）。
const maxTargetStatus = 20

// Targets 查询抓取目标状态（实现 TargetReporter）。
//
// 走 GET /api/v1/targets?state=active：up=0 时 data.activeTargets[].lastError
// 就是"为什么抓不到"的原始答案（如 Access denied / invalid DSN / connection refused）。
func (p *promClient) Targets(ctx context.Context, job string) ([]TargetStatus, error) {
	body, err := p.do(ctx, "/api/v1/targets", url.Values{"state": {"active"}})
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ActiveTargets []struct {
				Labels     map[string]string `json:"labels"`
				ScrapeURL  string            `json:"scrapeUrl"`
				LastError  string            `json:"lastError"`
				LastScrape string            `json:"lastScrape"`
				Health     string            `json:"health"`
				ScrapePool string            `json:"scrapePool"`
			} `json:"activeTargets"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析 targets 响应失败: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus 返回 %s: %s", parsed.Status, parsed.Error)
	}
	out := make([]TargetStatus, 0, len(parsed.Data.ActiveTargets))
	for _, item := range parsed.Data.ActiveTargets {
		itemJob := item.Labels["job"]
		if job != "" && itemJob != job {
			continue
		}
		out = append(out, TargetStatus{
			Job: itemJob, Instance: item.Labels["instance"], Health: item.Health,
			LastError: item.LastError, LastScrape: item.LastScrape,
			ScrapeURL: item.ScrapeURL, Labels: item.Labels,
		})
		if len(out) >= maxTargetStatus {
			break
		}
	}
	return out, nil
}

// LabelValues 查询标签取值（实现 LabelReporter）。
//
// 走 GET /api/v1/label/<label>/values，可带 match[] 限定序列范围。
// 结果截断到 maxLabelValues 条：只用于给使用者提示，不需要全量。
func (p *promClient) LabelValues(ctx context.Context, label string, matchers ...string) ([]string, error) {
	trimmed := strings.TrimSpace(label)
	if trimmed == "" {
		return nil, fmt.Errorf("标签名不能为空")
	}
	params := url.Values{}
	for _, matcher := range matchers {
		if strings.TrimSpace(matcher) != "" {
			params.Add("match[]", matcher)
		}
	}
	body, err := p.do(ctx, "/api/v1/label/"+url.PathEscape(trimmed)+"/values", params)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Status string   `json:"status"`
		Error  string   `json:"error"`
		Data   []string `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析标签取值失败: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus 返回 %s: %s", parsed.Status, parsed.Error)
	}
	sort.Strings(parsed.Data)
	if len(parsed.Data) > maxLabelValues {
		parsed.Data = parsed.Data[:maxLabelValues]
	}
	return parsed.Data, nil
}

// maxLabelValues 限制标签取值返回条数（避免把几百个序列刷到前端）。
const maxLabelValues = 50

// Snapshot 采集实例当前指标。
//
// 三种结果必须严格区分，否则接入排障无从下手：
//   - 至少一个指标取到值 → 正常快照；
//   - 全部指标为「空结果」 → 快照照常返回，但在 Note 里写清是选择器没匹配到；
//   - 全部指标为「请求失败」 → 返回 error，交给 fallbackClient 决定是否降级模拟器。
func (p *promClient) Snapshot(ctx context.Context, target Target) (*Snapshot, error) {
	profile := ProfileOf(target.MWType)
	selector := buildSelector(target, p.jobPrefix)
	snapshot := &Snapshot{
		InstanceID: target.InstanceID,
		MWType:     target.MWType,
		Collected:  time.Now().UTC(),
		Source:     "prometheus",
		Metrics:    make([]Metric, 0, len(profile.Metrics)),
		Selector:   selector,
		Total:      len(profile.Metrics),
	}
	var (
		firstErr  error
		emptyCnt  int
		failedCnt int
	)
	for _, spec := range profile.Metrics {
		expr := strings.ReplaceAll(spec.Expr, "{selector}", "{"+selector+"}")
		expr = strings.ReplaceAll(expr, ",}", "}")
		value, err := p.queryValue(ctx, expr)
		if err != nil {
			if errors.Is(err, errEmptyResult) {
				emptyCnt++
			} else {
				failedCnt++
			}
			if firstErr == nil {
				firstErr = err
			}
			p.log.Debug("指标查询失败", zap.String("metric", spec.Name), zap.String("expr", expr), zap.Error(err))
			snapshot.Metrics = append(snapshot.Metrics, Metric{
				Name: spec.Name, DisplayName: spec.DisplayName, Unit: spec.Unit,
				Category: spec.Category, Status: "unknown", Expr: expr,
				WarningThreshold: spec.WarningThreshold, CriticalThreshold: spec.CriticalThreshold,
			})
			continue
		}
		snapshot.Matched++
		snapshot.Metrics = append(snapshot.Metrics, Metric{
			Name: spec.Name, DisplayName: spec.DisplayName, Unit: spec.Unit, Category: spec.Category,
			Latest: value, Expr: expr, Status: evaluateStatus(spec, value),
			WarningThreshold: spec.WarningThreshold, CriticalThreshold: spec.CriticalThreshold,
		})
	}

	// 全部查询都失败（上游不可达/协议错误）→ 让 fallbackClient 降级为模拟器。
	if failedCnt == len(profile.Metrics) && len(profile.Metrics) > 0 {
		return nil, fmt.Errorf("Prometheus 查询全部失败（%d 项）：%w", failedCnt, firstErr)
	}

	// 探测 job 是否被 Prometheus 抓取：区分「job 未配置」与「标签对不上」。
	snapshot.JobUp = p.probeJobUp(ctx, target)
	snapshot.Note = buildSnapshotNote(selector, jobOf(target, p.jobPrefix), snapshot.JobUp, snapshot.Matched, emptyCnt, failedCnt)
	return snapshot, nil
}

// probeJobUp 查询 up{job="..."}，返回 nil 表示该 job 在 Prometheus 中不存在。
func (p *promClient) probeJobUp(ctx context.Context, target Target) *float64 {
	job := jobOf(target, p.jobPrefix)
	if job == "" {
		return nil
	}
	value, err := p.queryValue(ctx, fmt.Sprintf(`up{job="%s"}`, job))
	if err != nil {
		return nil
	}
	return &value
}

// buildSnapshotNote 依据采集结果生成人可读的诊断说明；一切正常时返回空串。
func buildSnapshotNote(selector, job string, jobUp *float64, matched, emptyCnt, failedCnt int) string {
	if matched > 0 {
		if emptyCnt > 0 || failedCnt > 0 {
			return fmt.Sprintf("选择器 {%s} 命中 %d 项，另有 %d 项无数据、%d 项查询失败（多为该实例未暴露对应指标，属正常现象）",
				selector, matched, emptyCnt, failedCnt)
		}
		return ""
	}
	switch {
	case jobUp == nil:
		return fmt.Sprintf("Prometheus 可达，但选择器 {%s} 未匹配到任何时序，且 up{job=%q} 也不存在：该 job 尚未在 Prometheus 中配置。请确认抓取配置已挂载并重建 Prometheus 容器。",
			selector, job)
	case *jobUp == 0:
		return fmt.Sprintf("Prometheus 已配置 job=%q，但该 target 抓取失败（up=0）：Exporter 未启动或连不上被管中间件。请查看 Prometheus /targets 页面的 lastError 与 Exporter 容器日志。", job)
	default:
		return fmt.Sprintf("Prometheus 已正常抓取 job=%q（up=1），但选择器 {%s} 匹配不到时序：请让「实例名称」与 Prometheus 标签 instance_name 完全一致，或改用「Prometheus instance」精确指定；两者只能用一个（填了 Prometheus instance 会忽略实例名）。",
			job, selector)
	}
}

// History 查询指标历史趋势。
func (p *promClient) History(ctx context.Context, target Target, metric string, r TimeRange) ([]Sample, error) {
	spec, ok := SpecOf(target.MWType, metric)
	if !ok {
		return nil, fmt.Errorf("未知指标: %s", metric)
	}
	selector := buildSelector(target, p.jobPrefix)
	expr := strings.ReplaceAll(spec.Expr, "{selector}", "{"+selector+"}")
	expr = strings.ReplaceAll(expr, ",}", "}")
	return p.queryRange(ctx, expr, r)
}

// Compare 多实例同指标对比。
//
// 单个实例查不到数据（空序列）不算失败，如实跳过；但**全部实例都查询失败**
// 说明上游不可用，返回 error 以便降级链路生效。
func (p *promClient) Compare(ctx context.Context, targets []Target, metric string) (map[string]float64, error) {
	out := make(map[string]float64, len(targets))
	var lastErr error
	failures := 0
	for _, target := range targets {
		samples, err := p.History(ctx, target, metric, TimeRange{
			Start: time.Now().Add(-5 * time.Minute),
			End:   time.Now(),
			Step:  time.Minute,
		})
		if err != nil {
			lastErr = err
			failures++
			continue
		}
		if len(samples) == 0 {
			continue
		}
		out[target.Name] = samples[len(samples)-1].Value
	}
	if len(targets) > 0 && failures == len(targets) {
		return nil, lastErr
	}
	return out, nil
}

// queryValue 执行瞬时查询。
func (p *promClient) queryValue(ctx context.Context, expr string) (float64, error) {
	key := "prom:instant:" + expr
	if p.store != nil {
		if raw, err := p.store.Get(ctx, key); err == nil {
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				return v, nil
			}
		}
	}
	body, err := p.do(ctx, "/api/v1/query", url.Values{"query": {expr}})
	if err != nil {
		return 0, err
	}
	var parsed promResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, fmt.Errorf("解析 prometheus 响应失败: %w", err)
	}
	if parsed.Status != "success" {
		return 0, fmt.Errorf("prometheus 返回 %s: %s", parsed.Status, parsed.Error)
	}
	value, err := parsed.firstValue()
	if err != nil {
		return 0, err
	}
	if p.store != nil && p.cacheTTL > 0 {
		_ = p.store.Set(ctx, key, strconv.FormatFloat(value, 'f', -1, 64), p.cacheTTL)
	}
	return value, nil
}

// queryRange 执行区间查询。
func (p *promClient) queryRange(ctx context.Context, expr string, r TimeRange) ([]Sample, error) {
	if r.Step <= 0 {
		r.Step = time.Minute
	}
	if r.End.IsZero() {
		r.End = time.Now()
	}
	if r.Start.IsZero() {
		r.Start = r.End.Add(-time.Hour)
	}
	params := url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(r.Start.Unix(), 10)},
		"end":   {strconv.FormatInt(r.End.Unix(), 10)},
		"step":  {strconv.FormatFloat(r.Step.Seconds(), 'f', -1, 64)},
	}
	body, err := p.do(ctx, "/api/v1/query_range", params)
	if err != nil {
		return nil, err
	}
	var parsed promResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析 prometheus 响应失败: %w", err)
	}
	if parsed.Status != "success" {
		return nil, fmt.Errorf("prometheus 返回 %s: %s", parsed.Status, parsed.Error)
	}
	return parsed.firstSeries()
}

// do 执行 HTTP 请求并返回响应体。
func (p *promClient) do(ctx context.Context, path string, params url.Values) ([]byte, error) {
	endpoint := p.baseURL + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("构造请求失败: %w", err)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 prometheus 失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 prometheus 响应失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("prometheus 状态码 %d", resp.StatusCode)
	}
	return body, nil
}

// promResponse 是 Prometheus 查询响应结构。
type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string           `json:"resultType"`
		Result     []promResultItem `json:"result"`
	} `json:"data"`
}

type promResultItem struct {
	Metric map[string]string `json:"metric"`
	Value  []any             `json:"value"`
	Values [][]any           `json:"values"`
}

// firstValue 提取首个样本值。
//
// 结果集为空时返回 errEmptyResult（而不是普通错误）：调用方据此区分
// 「选择器没匹配到时序」与「Prometheus 不可用」。
//
// 另外把 NaN / ±Inf 也视为"没有可用数值"：Prometheus 在「除以 0」「无样本可计算」
// 等场景会返回字符串 "NaN" / "+Inf"，Go 的 strconv.ParseFloat 会**成功**解析它们。
// 这类值一旦进入响应，encoding/json 会直接失败，而 gin 是先写状态码再 Marshal——
// 客户端收到「HTTP 200 + 空响应体」，前端解包得到 undefined，报出与真实原因无关的
// 错误（如 Cannot read properties of undefined (reading 'series')）。
func (r promResponse) firstValue() (float64, error) {
	if len(r.Data.Result) == 0 {
		return 0, errEmptyResult
	}
	item := r.Data.Result[0]
	if len(item.Value) < 2 {
		return 0, fmt.Errorf("查询结果缺少样本值")
	}
	value, err := toFloat(item.Value[1])
	if err != nil {
		return 0, err
	}
	if !isFinite(value) {
		return 0, errEmptyResult
	}
	return value, nil
}

// firstSeries 提取首个序列。
//
// 逐点丢弃 NaN / ±Inf：例如「命中率」= rate(hits)/(rate(hits)+rate(misses))*100，
// Redis 在该窗口内没有读写时两侧都是 0，Prometheus 会算出 NaN。
// 与其把 NaN 塞进响应（会破坏 JSON 编码），不如把它当作"该点无数据"跳过；
// 全部点都无数据时返回空序列，前端显示"暂无采样数据"。
func (r promResponse) firstSeries() ([]Sample, error) {
	if len(r.Data.Result) == 0 {
		return nil, nil
	}
	items := r.Data.Result[0].Values
	out := make([]Sample, 0, len(items))
	for _, pair := range items {
		if len(pair) < 2 {
			continue
		}
		ts, err := toFloat(pair[0])
		if err != nil {
			continue
		}
		val, err := toFloat(pair[1])
		if err != nil || !isFinite(val) {
			continue
		}
		out = append(out, Sample{Timestamp: time.Unix(int64(ts), 0).UTC(), Value: val})
	}
	return out, nil
}

// isFinite 判断样本值是否为有限数。
//
// 供 toFloat 的调用方在做"可编码性"检查时复用：JSON 无法表示 NaN/±Inf。
func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// toFloat 把 Prometheus 的字符串/浮点样本值转为 float64。
func toFloat(v any) (float64, error) {
	switch val := v.(type) {
	case float64:
		return val, nil
	case string:
		return strconv.ParseFloat(val, 64)
	case json.Number:
		return val.Float64()
	default:
		return 0, fmt.Errorf("无法解析样本值 %v", v)
	}
}

// evaluateStatus 依据 ThresholdMode 判定指标状态。
//
// 语义见 ThresholdMode 注释。两个关键设计点（均由真实缺陷驱动）：
//
//  1. 「越低越差」采用**含边界**比较（value <= 告警线 即异常）。
//     因为 0 是有意义的临界值（Elasticsearch red=0），严格小于会导致 x < 0 永远为假，
//     red 状态被判成 warning。
//  2. critical 必须先于 warning 判定，否则 warning 会先短路掉更严重的状态。
func evaluateStatus(spec MetricSpec, value float64) string {
	switch spec.Mode {
	case ThresholdHigherWorse:
		switch {
		case value >= spec.CriticalThreshold:
			return "critical"
		case value >= spec.WarningThreshold:
			return "warning"
		default:
			return "ok"
		}
	case ThresholdLowerWorse:
		switch {
		case value <= spec.CriticalThreshold:
			return "critical"
		case value <= spec.WarningThreshold:
			return "warning"
		default:
			return "ok"
		}
	case ThresholdBoolDown:
		if value <= spec.CriticalThreshold {
			return "critical"
		}
		return "ok"
	default:
		// ThresholdNone：纯观测型指标不做判定，避免用未配置的阈值误报。
		return "ok"
	}
}
