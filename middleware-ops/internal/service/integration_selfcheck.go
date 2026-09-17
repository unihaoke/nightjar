package service

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
)

// 本文件实现「集成自检」：一次点击，按**环节**给出结论，而不是让使用者去翻日志。
//
// 真实反馈：排查一个集成要在四处各看一段——平台能不能连 Exporter 端口、Prometheus 的 target
// 是不是 up、`redis_up` 是不是 1、Exporter 日志里写了什么——而且"Exporter 是否还在"、
// "状态是否正确"没有一处能直接回答。这里把链路拆成四段，每段给 状态 + 依据 + 下一步动作：
//
//	① platform  平台 → Exporter 端口可达（TCP 探测，远程时就是安全组/防火墙那一跳）
//	② exporter  Exporter 是否在位（本机 docker inspect；远程则说明并给出核对命令）
//	③ scrape    Prometheus 是否已抓取到它（按**本实例那条 target** 判 up / lastError）
//	④ metrics   业务指标是否真的有数据（组件自身的 up + 有多少指标命中）
//
// 结论里带上 next_action，与待处理横幅使用同一套判定，界面只需要渲染。

// SelfCheckStage 是端到端自检的一个环节。
type SelfCheckStage struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail"`
	Advice string `json:"advice"`
	// Action / ActionLabel 是本环节失败时**平台能替你做的下一步**（空=没有平台侧动作，
	// 例如安全组没放通只能人工处理）。由环节自己声明，不靠错误文本猜。
	Action      string `json:"action,omitempty"`
	ActionLabel string `json:"action_label,omitempty"`
}

// IntegrationSelfCheck 是一次集成自检的完整结论。
type IntegrationSelfCheck struct {
	InstanceID      int64            `json:"instance_id"`
	Name            string           `json:"name"`
	OK              bool             `json:"ok"`
	Summary         string           `json:"summary"`
	Stages          []SelfCheckStage `json:"stages"`
	NextAction      string           `json:"next_action"`
	NextActionLabel string           `json:"next_action_label"`
}

// 自检环节状态。
const (
	stageOK   = "ok"
	stageWarn = "warn"
	stageFail = "fail"
)

// SelfCheck 对单个集成做端到端自检（只读：不重装、不改配置、不需要凭据）。
func (s *IntegrationService) SelfCheck(ctx context.Context, id int64) (*IntegrationSelfCheck, error) {
	item, err := s.integrationInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	meta, ok := IntegrationMetaOf(*item)
	if !ok {
		return nil, fmt.Errorf("该实例不是通过集成中心创建的")
	}
	tpl, ok := integration.TemplateOf(meta.Template)
	if !ok {
		return nil, fmt.Errorf("组件模板 %q 不存在", meta.Template)
	}
	address, _ := integration.ParseAddress(meta.Address, tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)

	out := &IntegrationSelfCheck{InstanceID: item.ID, Name: item.Name, Stages: make([]SelfCheckStage, 0, 4)}
	remote := normalizeDeployTarget(meta.DeployTarget) == DeployTargetRemote
	target := s.scrapeTarget(meta, address)

	// ① 平台 → Exporter 端口
	probeStage := s.selfCheckPort(ctx, target, item.Name, remote)
	out.Stages = append(out.Stages, probeStage)

	// ② Exporter 是否在位
	out.Stages = append(out.Stages, s.selfCheckExporter(ctx, item, meta, tpl, remote, probeStage.Status == stageOK))

	// ③ Prometheus 抓取
	out.Stages = append(out.Stages, s.selfCheckScrape(ctx, item, meta, item.Name))

	// ④ 业务指标
	out.Stages = append(out.Stages, s.selfCheckMetrics(ctx, item))

	out.OK = true
	firstFail := SelfCheckStage{}
	for _, stage := range out.Stages {
		if stage.Status == stageFail {
			out.OK = false
			if firstFail.Key == "" {
				firstFail = stage
			}
		}
	}
	if out.OK {
		out.Summary = "全链路正常：平台可达 Exporter → Exporter 在位 → Prometheus 已抓取 → 业务指标有数据"
		return out, nil
	}
	out.Summary = fmt.Sprintf("在「%s」这一环断了：%s", firstFail.Title, firstFail.Advice)
	out.NextAction, out.NextActionLabel = firstFail.Action, firstFail.ActionLabel
	return out, nil
}

// selfCheckPort 探测平台到 Exporter 端口的连通性（远程时这一跳就是安全组/防火墙）。
func (s *IntegrationService) selfCheckPort(ctx context.Context, target, name string, remote bool) SelfCheckStage {
	stage := SelfCheckStage{Key: "platform", Title: "平台 → Exporter 端口"}
	start := time.Now()
	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", target)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		stage.Status = stageFail
		stage.Detail = fmt.Sprintf("平台连不上 %s：%v", target, err)
		if remote {
			// 安全组/防火墙只能人工处理：这里不给平台侧动作按钮，避免"点了也没用"。
			stage.Advice = "远程部署的 Exporter 在目标机上：请在云安全组/防火墙对**平台出口 IP** 放通该端口，" +
				"并确认目标机上确实有进程在监听（`ss -ltnp | grep <端口>`）"
		} else {
			stage.Advice = "本机部署的 Exporter 没起来或没接入平台网络：点「重新应用」重建，随后看容器状态"
			stage.Action, stage.ActionLabel = string(NextActionReapply), nextActionLabel(NextActionReapply)
		}
		return stage
	}
	_ = conn.Close()
	stage.Status = stageOK
	stage.Detail = fmt.Sprintf("平台 %dms 内连通 %s", latency, target)
	return stage
}

// selfCheckExporter 回答"Exporter 是不是还被平台托管/在位"。
//
// 本机部署：直接问 docker（容器是否存在、是否 running）。
// 远程部署：平台的 docker 通道看不到目标机，**不能假装知道**——如实说明，并给出核对命令；
// 以"端口可达 + 抓取目标 up"作为替代证据。
func (s *IntegrationService) selfCheckExporter(
	ctx context.Context, item *model.MiddlewareInstance, meta IntegrationMeta,
	tpl integration.Template, remote, portReachable bool,
) SelfCheckStage {
	stage := SelfCheckStage{Key: "exporter", Title: "Exporter 是否在位"}
	if remote {
		command := fmt.Sprintf("docker ps --filter name=%s 与 ss -ltnp | grep %d", meta.Container, meta.ExporterHostPort)
		if portReachable {
			stage.Status = stageOK
			stage.Detail = fmt.Sprintf("远程部署：容器 %s 归目标机 %s 管理（平台没有那台机器的 docker 通道，不直接 inspect）；"+
				"端口 %d 已可达，说明它确实在跑", meta.Container, meta.TargetHost, meta.ExporterHostPort)
			stage.Advice = "需要确认真实容器时到目标机执行：" + command
			return stage
		}
		stage.Status = stageFail
		stage.Detail = fmt.Sprintf("远程部署：既探不到 %s:%d，也无法直接 inspect 目标机", meta.TargetHost, meta.ExporterHostPort)
		stage.Advice = "先按上一环放通端口；仍不通说明容器没起来，到目标机执行：" + command
		stage.Action, stage.ActionLabel = string(NextActionReapply), nextActionLabel(NextActionReapply)
		return stage
	}
	// 本机部署：容器状态就是权威答案。
	if s.docker == nil {
		stage.Status = stageWarn
		stage.Detail = "平台没有 docker 通道，无法确认容器状态（一键部署也未启用）"
		stage.Advice = "在 .env 打开 integration.docker_enabled 并挂载 docker.sock 后重建 backend"
		return stage
	}
	state, err := s.docker.Inspect(ctx, meta.Container)
	if err != nil || state == nil {
		stage.Status = stageFail
		stage.Detail = fmt.Sprintf("容器 %s 不存在或查询失败", meta.Container)
		stage.Advice = "点「重新应用」由平台重建容器（会重新拉镜像并接入目标网络）"
		stage.Action, stage.ActionLabel = string(NextActionReapply), nextActionLabel(NextActionReapply)
		return stage
	}
	if state.Status != "running" {
		stage.Status = stageFail
		stage.Detail = fmt.Sprintf("容器 %s 存在但状态是 %s", meta.Container, state.Status)
		stage.Advice = "点「重新应用」重建；反复退出时先看它的日志（口令/地址不对会让 Exporter 自己退出）"
		stage.Action, stage.ActionLabel = string(NextActionReapply), nextActionLabel(NextActionReapply)
		return stage
	}
	stage.Status = stageOK
	stage.Detail = fmt.Sprintf("容器 %s 正在运行（%s）", meta.Container, tpl.Component)
	return stage
}

// selfCheckScrape 判 Prometheus 是否已经抓取到本实例（按本实例那条 target，而不是 job 级）。
func (s *IntegrationService) selfCheckScrape(
	ctx context.Context, item *model.MiddlewareInstance, meta IntegrationMeta, name string,
) SelfCheckStage {
	stage := SelfCheckStage{Key: "scrape", Title: "Prometheus 是否已抓取到它"}
	reporter, ok := s.monitor.(monitor.TargetReporter)
	if !ok || s.monitor == nil {
		stage.Status = stageWarn
		stage.Detail = "当前监控数据源不支持查询抓取目标（如内置模拟器），无法判定"
		return stage
	}
	job := meta.Job
	if job == "" {
		job = s.jobName()
	}
	statuses, err := reporter.Targets(ctx, job)
	if err != nil {
		stage.Status = stageWarn
		stage.Detail = "读不到 Prometheus 的目标状态：" + err.Error()
		stage.Advice = "确认平台能访问 Prometheus（prometheus.base_url）；这项不影响集成本身"
		return stage
	}
	status, found := pickTargetStatus(statuses, name)
	if !found {
		stage.Status = stageFail
		stage.Detail = fmt.Sprintf("Prometheus 的 job=%s 下没有本实例的抓取目标（http_sd 默认 30s 刷新）", job)
		stage.Advice = "点「重新应用」重写服务发现；若刚集成完，等 30 秒再自检一次"
		stage.Action, stage.ActionLabel = string(NextActionReapply), nextActionLabel(NextActionReapply)
		return stage
	}
	if status.Health != "up" {
		stage.Status = stageFail
		stage.Detail = fmt.Sprintf("抓取目标 %s 处于 %s：%s", status.ScrapeURL, status.Health,
			monitor.DescribeTargetError(status.LastError))
		stage.Advice = "先修上一环（端口/安全组）；地址写错或容器没起来都会表现为这里 down"
		stage.Action, stage.ActionLabel = string(NextActionReapply), nextActionLabel(NextActionReapply)
		return stage
	}
	stage.Status = stageOK
	stage.Detail = fmt.Sprintf("抓取正常（%s，最近一次 %s）", status.ScrapeURL, status.LastScrape)
	return stage
}

// selfCheckMetrics 回答"状态是否正确"：组件自身的 up 与业务指标是否真的有数据。
func (s *IntegrationService) selfCheckMetrics(ctx context.Context, item *model.MiddlewareInstance) SelfCheckStage {
	stage := SelfCheckStage{Key: "metrics", Title: "业务指标是否真的在流"}
	if s.monitor == nil {
		stage.Status = stageWarn
		stage.Detail = "未配置监控数据源"
		return stage
	}
	snapshot, err := s.monitor.Snapshot(ctx, ToTarget(*item))
	if err != nil {
		stage.Status = stageWarn
		stage.Detail = "指标快照采集失败：" + err.Error()
		return stage
	}
	if snapshot.Degraded || snapshot.Source != "prometheus" {
		stage.Status = stageWarn
		stage.Detail = "当前展示的是内置模拟数据（Prometheus 不可达），无法判定真实指标"
		stage.Advice = "确认平台能访问 Prometheus"
		return stage
	}
	// 组件自身的 up（redis_up / mysql_up / pg_up / 抓取目标的 up）直接查一次：
	// 它才是"Exporter 能不能连上被管实例"的权威答案，而这类指标不在 profile 里。
	upName, upValue, hasUp := s.componentUp(ctx, item, snapshot)
	detail := fmt.Sprintf("命中 %d/%d 项指标", snapshot.Matched, snapshot.Total)
	if hasUp {
		detail += fmt.Sprintf("，%s=%s", upName, formatUpValue(upValue))
	}
	stage.Detail = detail
	switch {
	case hasUp && upValue != nil && *upValue == 0:
		stage.Status = stageFail
		stage.Advice = "Exporter 起来了但连不上被管实例（" + upName + "=0）：核对地址与账号口令；" +
			"Redis 填了公网 IP 而同机时应改 127.0.0.1；口令不一致、或目标没设口令却传了口令，都会导致 0"
		// 原因可能是地址也可能是口令，而「测试连接 / 重试建号」这条通道既能验证凭据又能重建 Exporter，
		// 是这里最省事的入口（比重新安装轻，比只看日志明确）。
		stage.Action, stage.ActionLabel = string(NextActionRetryAccount), "去测试连接 / 重试建号"
	case snapshot.Matched == 0:
		stage.Status = stageFail
		stage.Detail += "；选择器没有命中任何时序"
		stage.Advice = "Exporter 进程在跑但没有业务指标：多为连不上被管实例（见上一环与 Exporter 日志）"
		stage.Action, stage.ActionLabel = string(NextActionReapply), nextActionLabel(NextActionReapply)
	default:
		stage.Status = stageOK
		stage.Advice = "业务指标已有数据，无需处理"
	}
	return stage
}

// componentUp 取"组件自身是否可用"的指标值。
//
// 两条来源，优先级从高到低：
//  1. 按类型直接查 Exporter 暴露的 up（redis_up / mysql_up / pg_up / 抓取目标的 up）——
//     这是权威答案；
//  2. profile 里 Mode=ThresholdBoolDown 的指标（node 的 host_up、kafka 的 broker_up），
//     用于没有 `*_up` 约定的组件。
func (s *IntegrationService) componentUp(
	ctx context.Context, item *model.MiddlewareInstance, snapshot *monitor.Snapshot,
) (string, *float64, bool) {
	selector := ""
	if reporter, ok := s.monitor.(monitor.SelectorReporter); ok {
		selector = reporter.Selector(ToTarget(*item))
	}
	if expr, name := componentUpExpr(item.MWType, selector); expr != "" {
		if reporter, ok := s.monitor.(monitor.ValueReporter); ok {
			if value, err := reporter.QueryValue(ctx, expr); err == nil && value != nil {
				return name, value, true
			}
		}
	}
	return findUpMetric(snapshot)
}

// componentUpMetrics 是各类组件"自身可用性"的指标名。
//
// node_exporter 没有 node_up —— 抓取目标的 `up` 就代表 Exporter 在位。
// 表里没有的类型（或在 Prometheus 里查不到的）会退回 profile 的 bool_down 指标。
var componentUpMetrics = map[string]string{
	integration.TypeRedis: "redis_up",
	integration.TypeMySQL: "mysql_up",
	integration.TypePG:    "pg_up",
	integration.TypeNode:  "up",
}

// componentUpExpr 拼出 up 指标的 PromQL（查不到对应约定时返回空串）。
func componentUpExpr(mwType, selector string) (string, string) {
	name := componentUpMetrics[mwType]
	if name == "" {
		return "", ""
	}
	if strings.TrimSpace(selector) == "" {
		return name, name
	}
	return name + "{" + selector + "}", name
}

// findUpMetric 在快照里找出"组件自身是否可用"的那个指标（如 redis_up / mysql_up / host_up）。
//
// 判定依据是 profile 里的 Mode == ThresholdBoolDown（0 表示异常），而不是猜指标名，
// 这样新增组件模板时不需要改这里。
func findUpMetric(snapshot *monitor.Snapshot) (string, *float64, bool) {
	if snapshot == nil {
		return "", nil, false
	}
	profile := monitor.ProfileOf(snapshot.MWType)
	upNames := make(map[string]bool, len(profile.Metrics))
	for _, spec := range profile.Metrics {
		if spec.Mode == monitor.ThresholdBoolDown {
			upNames[spec.Name] = true
		}
	}
	for _, metric := range snapshot.Metrics {
		if !upNames[metric.Name] || metric.Status == "unknown" {
			continue
		}
		value := metric.Latest
		return metric.Name, &value, true
	}
	return "", nil, false
}

// formatUpValue 把 up 指标的值渲染成人可读的 1/0（保留异常值原样）。
func formatUpValue(value *float64) string {
	if value == nil {
		return "无数据"
	}
	switch *value {
	case 1:
		return "1（可用）"
	case 0:
		return "0（不可用）"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

// selfCheckSummaryForLog 把各环节状态压成一行，供日志记录（避免把长文本塞进日志字段）。
func selfCheckSummaryForLog(check *IntegrationSelfCheck) string {
	if check == nil {
		return ""
	}
	parts := make([]string, 0, len(check.Stages))
	for _, stage := range check.Stages {
		parts = append(parts, stage.Key+"="+stage.Status)
	}
	return strings.Join(parts, " ")
}
