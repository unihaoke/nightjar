package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/repository"
)

// MetricsService 提供统一监控查询能力（4.2）。
//
// 指标全部来自 Prometheus（或内置模拟器），平台不落自有指标表。
type MetricsService struct {
	instances *repository.InstanceRepository
	monitor   monitor.Client
	log       *zap.Logger
}

// NewMetricsService 构造监控服务。
func NewMetricsService(instances *repository.InstanceRepository, mon monitor.Client, log *zap.Logger) *MetricsService {
	return &MetricsService{instances: instances, monitor: mon, log: log}
}

// Snapshot 查询实例当前指标。
func (s *MetricsService) Snapshot(ctx context.Context, instanceID int64, scope Scope) (*monitor.Snapshot, error) {
	item, err := s.instance(ctx, instanceID, scope)
	if err != nil {
		return nil, err
	}
	snapshot, err := s.monitor.Snapshot(ctx, ToTarget(*item))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream, err)
	}
	return snapshot, nil
}

// History 查询指标历史趋势。
func (s *MetricsService) History(ctx context.Context, instanceID int64, metric string, r monitor.TimeRange, scope Scope) ([]monitor.Sample, error) {
	item, err := s.instance(ctx, instanceID, scope)
	if err != nil {
		return nil, err
	}
	if metric == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "必须指定指标名")
	}
	if r.End.IsZero() {
		r.End = time.Now().UTC()
	}
	if r.Start.IsZero() {
		r.Start = r.End.Add(-6 * time.Hour)
	}
	if r.Step <= 0 {
		r.Step = 5 * time.Minute
	}
	samples, err := s.monitor.History(ctx, ToTarget(*item), metric, r)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream, err)
	}
	return samples, nil
}

// CompareItem 是多实例对比结果项。
type CompareItem struct {
	InstanceID   int64   `json:"instance_id"`
	InstanceName string  `json:"instance_name"`
	MWType       string  `json:"mw_type"`
	Environment  string  `json:"environment"`
	GroupName    string  `json:"group_name"`
	Value        float64 `json:"value"`
	Unit         string  `json:"unit"`
}

// Compare 执行多实例对比。
func (s *MetricsService) Compare(ctx context.Context, ids []int64, metric string, scope Scope) ([]CompareItem, error) {
	if metric == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "必须指定对比指标")
	}
	items, err := s.instances.All(ctx, repository.InstanceFilter{
		EnvScope: scope.EnvScope, GroupScope: scope.GroupScope,
	})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	idSet := make(map[int64]bool, len(ids))
	for _, id := range ids {
		idSet[id] = true
	}
	selected := make([]model.MiddlewareInstance, 0, len(items))
	for _, item := range items {
		if len(idSet) > 0 && !idSet[item.ID] {
			continue
		}
		selected = append(selected, item)
	}
	if len(selected) == 0 {
		return nil, apperr.New(apperr.CodeNotFound, "没有可对比的实例")
	}
	out := make([]CompareItem, 0, len(selected))
	for _, item := range selected {
		history, histErr := s.monitor.History(ctx, ToTarget(item), metric, monitor.TimeRange{
			Start: time.Now().Add(-30 * time.Minute), End: time.Now(), Step: time.Minute,
		})
		value := 0.0
		if histErr == nil && len(history) > 0 {
			value = history[len(history)-1].Value
		}
		unit := ""
		if profile := monitor.ProfileOf(item.MWType); profile.MWType != "" {
			for _, spec := range profile.Metrics {
				if spec.Name == metric {
					unit = spec.Unit
					break
				}
			}
		}
		out = append(out, CompareItem{
			InstanceID: item.ID, InstanceName: item.Name, MWType: item.MWType,
			Environment: item.Environment, GroupName: item.GroupName, Value: value, Unit: unit,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value > out[j].Value })
	return out, nil
}

// Catalog 返回指定中间件类型的指标目录（供前端筛选与规则配置）。
func (s *MetricsService) Catalog(mwType string) map[string]any {
	profile := monitor.ProfileOf(mwType)
	metrics := make([]map[string]any, 0, len(profile.Metrics))
	for _, spec := range profile.Metrics {
		metrics = append(metrics, map[string]any{
			"name": spec.Name, "display_name": spec.DisplayName, "unit": spec.Unit,
			"category": spec.Category, "warning_threshold": spec.WarningThreshold,
			"critical_threshold": spec.CriticalThreshold,
			// threshold_mode 决定前端如何解读阈值方向：
			//   higher_worse 越高越差 / lower_worse 越低越差 / bool_down 正常-异常 / 空 表示纯观测
			"threshold_mode": string(spec.Mode),
		})
	}
	return map[string]any{
		"mw_type": mwType,
		"metrics": metrics,
		"types":   monitor.SupportedTypes(),
	}
}

// DiagnoseMetric 是自检中单个指标的探测结果。
type DiagnoseMetric struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Expr        string `json:"expr"`
	Matched     bool   `json:"matched"`
	Status      string `json:"status"`
}

// DiagnoseResult 是实例「接入自检」结果。
//
// 存在的意义：实例纳管后"看不到监控"有 6 种以上互不相同的原因，而平台早期
// 统一表现为「页面空白/全是 0」，用户无法定位。自检把平台真正执行的 PromQL、
// 选择器、job 抓取状态与逐条建议一次性摊开。
type DiagnoseResult struct {
	InstanceID   int64            `json:"instance_id"`
	InstanceName string           `json:"instance_name"`
	MWType       string           `json:"mw_type"`
	Host         string           `json:"host"`
	Port         int              `json:"port"`
	PromJob      string           `json:"prom_job"`
	PromInstance string           `json:"prom_instance"`
	MonitorKind  string           `json:"monitor_kind"`
	Healthy      bool             `json:"prometheus_healthy"`
	Selector     string           `json:"selector"`
	JobUp        *float64         `json:"job_up"`
	Matched      int              `json:"matched"`
	Total        int              `json:"total"`
	Source       string           `json:"source"`
	Degraded     bool             `json:"degraded"`
	Note         string           `json:"note"`
	Metrics      []DiagnoseMetric `json:"metrics"`
	Hints        []string         `json:"hints"`
	// LogChecklist 说明日志链路（与中间件实例无关）的必备条件。
	LogChecklist []string `json:"log_checklist"`
}

// Diagnose 执行实例接入自检：把选择器、Prometheus 抓取状态与逐条排查建议返回给前端。
func (s *MetricsService) Diagnose(ctx context.Context, instanceID int64, scope Scope) (*DiagnoseResult, error) {
	item, err := s.instance(ctx, instanceID, scope)
	if err != nil {
		return nil, err
	}
	target := ToTarget(*item)
	result := &DiagnoseResult{
		InstanceID: item.ID, InstanceName: item.Name, MWType: item.MWType,
		Host: item.Host, Port: item.Port,
		PromJob: item.PromJob, PromInstance: item.PromInstance,
		MonitorKind: s.MonitorKind(),
		Metrics:     make([]DiagnoseMetric, 0),
		Hints:       make([]string, 0, 6),
		LogChecklist: []string{
			"日志告警与中间件实例是两条独立链路：日志按「服务器 + 服务名」归集，纳管 MySQL/Redis 实例不会产生任何日志事件。",
			"日志采集需在「集成中心 → 日志接入」为该服务创建采集容器；只保存中间件集成是不会产生日志事件的。",
			"采集容器读取目标容器已挂载的日志目录，路径由平台从 docker 配置中读取（环境变量 LOG_PATH 或像日志的挂载），读不到就拒绝配置而不会猜路径。",
			"采集容器与平台之间用服务令牌鉴权（平台 .env 的 HOOK_TOKEN），令牌不一致时 /api/hooks/logs 返回 401，日志会静默丢失。",
			"日志事件按错误指纹聚合，在「日志告警 → 事件」中查看；服务名与服务器名取自创建集成时填写的值。",
		},
	}

	if reporter, ok := s.monitor.(monitor.SelectorReporter); ok {
		result.Selector = reporter.Selector(target)
	}
	if s.monitor != nil {
		result.Healthy = s.monitor.Healthy(ctx)
	}

	snapshot, snapErr := s.monitor.Snapshot(ctx, target)
	if snapErr != nil {
		result.Hints = append(result.Hints, "指标快照采集失败："+snapErr.Error())
	} else {
		result.Source = snapshot.Source
		result.Degraded = snapshot.Degraded
		result.Note = snapshot.Note
		result.Matched = snapshot.Matched
		result.Total = snapshot.Total
		result.JobUp = snapshot.JobUp
		if result.Selector == "" {
			result.Selector = snapshot.Selector
		}
		for _, metric := range snapshot.Metrics {
			result.Metrics = append(result.Metrics, DiagnoseMetric{
				Name: metric.Name, DisplayName: metric.DisplayName, Expr: metric.Expr,
				Matched: metric.Status != "unknown", Status: metric.Status,
			})
		}
	}

	result.Hints = append(result.Hints, diagnoseHints(item, result)...)
	// 把「Prometheus 里实际有什么」补进来：报错只说"未匹配到任何时序"时，
	// 使用者最需要的是下面这份可选项（job 名 / instance_name 取值）。
	result.Hints = append(result.Hints, s.augmentLabelHints(ctx, item, result)...)
	// 容器网络/DNS 判定：这一步能确定地区分"网络挂错"与"Prometheus 自身问题"。
	result.Hints = append(result.Hints, s.augmentEndpointHints(ctx, result)...)
	return result, nil
}

// augmentEndpointHints 在自检里做一次容器内 DNS 实测。
//
// 为什么值得单独做：跨栈接入时最常见的故障是平台没接到 jd 的容器网络，
// 表现为 Docker 内置 DNS（127.0.0.11）报 server misbehaving。与其让使用者
// 去猜，不如在平台容器里直接解析一次 prometheus.base_url 的主机名——
// 解析失败即可确定结论为"容器网络问题"，解析成功则把方向指向 Prometheus 本身。
func (s *MetricsService) augmentEndpointHints(ctx context.Context, result *DiagnoseResult) []string {
	reporter, ok := s.monitor.(monitor.EndpointReporter)
	if !ok {
		return nil
	}
	endpoint := strings.TrimSpace(reporter.Endpoint())
	if endpoint == "" {
		return []string{"当前未配置 prometheus.base_url：监控页面展示的是内置模拟数据，不是真实指标。"}
	}
	host := monitor.HostOf(endpoint)
	if host == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := monitor.LookupHostCtx(probeCtx, host); err != nil {
		return []string{fmt.Sprintf(
			"平台容器内解析不了 %s（%v）：这是**容器网络**问题，不是 Prometheus 的问题。"+
				"集成中心创建 Exporter 时会自动把容器接进目标网络；若仍解析不了，"+
				"请核对名字是否与 `docker ps` 的容器名一致，并确认平台已挂载 docker.sock。"+
				"可执行 scripts/doctor-jd-link.sh 体检。当前查询地址：%s",
			host, err, endpoint)}
	}
	// 解析成功：把方向指向 Prometheus 自身/端口，避免用户继续在网络层打转。
	if result != nil && !result.Healthy {
		return []string{fmt.Sprintf(
			"主机名 %s 能解析，但 Prometheus 健康检查不通：方向在 Prometheus 自身或其端口。"+
				"确认容器在运行、端口与 prometheus.base_url 一致（同机部署注意平台两侧的 PROMETHEUS_PORT 不要用反）。当前查询地址：%s",
			host, endpoint)}
	}
	return nil
}

// diagnoseHints 依据自检事实生成可执行的排查建议（按可能性从高到低）。
func diagnoseHints(item *model.MiddlewareInstance, result *DiagnoseResult) []string {
	hints := make([]string, 0, 6)
	if result.MonitorKind == "simulator" {
		hints = append(hints, "当前数据源是内置模拟器（prometheus.base_url 为空）：页面上的数值不是真实指标。请设置 MWOPS_PROMETHEUS_BASE_URL=http://prometheus:9090（平台自带 Prometheus），并用 docker compose 启动平台监控服务。")
	}
	if !result.Healthy {
		hints = append(hints, "Prometheus 健康检查未通过：确认 mwops-prometheus 容器在运行、地址可达、网络别名正确（平台侧 getent hosts prometheus）。")
	}
	if result.Total == 0 {
		hints = append(hints, "该中间件类型没有指标画像（rabbitmq 仅纳管），因此不会产出任何指标。")
	}
	switch {
	case result.Matched > 0:
		// 正常，无需提示。
	case result.JobUp == nil && result.MonitorKind == "prometheus":
		hints = append(hints, "Prometheus 中没有这个 job：抓取配置未生效。核对 job 名与 prom_job 字段，并确认抓取配置文件已挂载、Prometheus 已重建（docker compose up -d 后需重启该容器）。")
	case result.JobUp != nil && *result.JobUp == 0:
		hints = append(hints, "target 抓取失败（up=0）：Exporter 未运行，或连不上被管中间件。先看 Prometheus /targets 的 lastError，再看 Exporter 容器日志与认证口令。")
	case result.Matched == 0:
		hints = append(hints, "抓取正常但标签对不上：把平台「实例名称」改成与 Prometheus 标签 instance_name 完全一致的值，或填写「Prometheus instance」精确指定。")
	}
	if item.PromInstance != "" {
		hints = append(hints, "已填写「Prometheus instance」：实例名称不再参与匹配（两者二选一）。jd 这类自建 Exporter 只上报 instance_name，此时应清空该字段。")
	}
	if result.Selector != "" {
		hints = append(hints, "可以直接在 Prometheus 里验证："+result.Selector)
	}
	return hints
}

// instance 查询实例并校验数据权限。
func (s *MetricsService) instance(ctx context.Context, id int64, scope Scope) (*model.MiddlewareInstance, error) {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "实例不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if !inScope(item, scope) {
		return nil, apperr.New(apperr.CodeScopeDenied, "该实例不在你的数据权限范围内")
	}
	return item, nil
}

// MonitorKind 返回监控数据来源类型。
func (s *MetricsService) MonitorKind() string {
	if s.monitor == nil {
		return "none"
	}
	return s.monitor.Kind()
}

// PrometheusJobs 返回 Prometheus 中已存在的 job 名，供纳管表单直接选择。
//
// 目的：把「job 名靠猜」变成「从列表里选」。纳管时最容易踩的坑就是把容器名
// （jd-redis-exporter）当成 job 名（middleware-exporter-redis）填进去。
// 查询失败或超时不阻塞表单，返回空列表即可（前端退化为手工输入）。
func (s *MetricsService) PrometheusJobs(ctx context.Context) []string {
	reporter, ok := s.monitor.(monitor.LabelReporter)
	if !ok {
		return nil
	}
	// 表单是同步请求，这里给一个比 prometheus.timeout 更短的预算。
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	jobs, err := reporter.LabelValues(probeCtx, "job")
	if err != nil {
		s.log.Debug("查询 Prometheus job 列表失败（表单将退化为手工输入）", zap.Error(err))
		return nil
	}
	return jobs
}

// PrometheusInstanceNames 返回 Prometheus 里已存在的 instance_name 取值。
//
// 这就是「实例名称」应该填的东西——**平台集成写入的服务发现标签**，
// 取值等于「集成中心」里的集成名称（如 jd-redis / jd-mysql）。
// 界面把它做成下拉候选，使用者不必再去 Prometheus 页面翻标签。
func (s *MetricsService) PrometheusInstanceNames(ctx context.Context) []string {
	reporter, ok := s.monitor.(monitor.LabelReporter)
	if !ok {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	names, err := reporter.LabelValues(probeCtx, "instance_name")
	if err != nil {
		s.log.Debug("查询 Prometheus instance_name 列表失败（表单将退化为手工输入）", zap.Error(err))
		return nil
	}
	return names
}
