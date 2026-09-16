package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/pkg/cache"
)

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

// Snapshot 采集实例当前指标。
func (p *promClient) Snapshot(ctx context.Context, target Target) (*Snapshot, error) {
	profile := ProfileOf(target.MWType)
	snapshot := &Snapshot{
		InstanceID: target.InstanceID,
		MWType:     target.MWType,
		Collected:  time.Now().UTC(),
		Source:     "prometheus",
		Metrics:    make([]Metric, 0, len(profile.Metrics)),
	}
	selector := buildSelector(target, p.jobPrefix)
	var firstErr error
	for _, spec := range profile.Metrics {
		expr := strings.ReplaceAll(spec.Expr, "{selector}", "{"+selector+"}")
		expr = strings.ReplaceAll(expr, ",}", "}")
		value, err := p.queryValue(ctx, expr)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			p.log.Debug("指标查询失败", zap.String("metric", spec.Name), zap.Error(err))
			snapshot.Metrics = append(snapshot.Metrics, Metric{
				Name: spec.Name, DisplayName: spec.DisplayName, Unit: spec.Unit,
				Category: spec.Category, Status: "unknown", Expr: expr,
				WarningThreshold: spec.WarningThreshold, CriticalThreshold: spec.CriticalThreshold,
			})
			continue
		}
		snapshot.Metrics = append(snapshot.Metrics, Metric{
			Name: spec.Name, DisplayName: spec.DisplayName, Unit: spec.Unit, Category: spec.Category,
			Latest: value, Expr: expr, Status: evaluateStatus(spec, value),
			WarningThreshold: spec.WarningThreshold, CriticalThreshold: spec.CriticalThreshold,
		})
	}
	if firstErr != nil && len(snapshot.Metrics) == 0 {
		return nil, firstErr
	}
	return snapshot, nil
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
func (p *promClient) Compare(ctx context.Context, targets []Target, metric string) (map[string]float64, error) {
	out := make(map[string]float64, len(targets))
	for _, target := range targets {
		samples, err := p.History(ctx, target, metric, TimeRange{
			Start: time.Now().Add(-5 * time.Minute),
			End:   time.Now(),
			Step:  time.Minute,
		})
		if err != nil || len(samples) == 0 {
			continue
		}
		out[target.Name] = samples[len(samples)-1].Value
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
func (r promResponse) firstValue() (float64, error) {
	if len(r.Data.Result) == 0 {
		return 0, fmt.Errorf("查询结果为空")
	}
	item := r.Data.Result[0]
	if len(item.Value) < 2 {
		return 0, fmt.Errorf("查询结果缺少样本值")
	}
	return toFloat(item.Value[1])
}

// firstSeries 提取首个序列。
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
		if err != nil {
			continue
		}
		out = append(out, Sample{Timestamp: time.Unix(int64(ts), 0).UTC(), Value: val})
	}
	return out, nil
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
