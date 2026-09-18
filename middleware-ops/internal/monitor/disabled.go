package monitor

import (
	"context"
	"fmt"
	"time"
)

// disabledClient 是「未接入 Prometheus」时的占位客户端。
//
// 设计意图（产品约定：只使用真实数据）：base_url 为空时监控链路必须明确表达「无数据源」，
// 而不是用模拟曲线粉饰一个什么都不存在的监控面板。
//
// 它实现 Client 接口：快照返回一个带说明的空快照（页面展示「无数据」而非报错），
// 历史/对比直接返回错误（这些接口无人值守调用，报错比静默空值更安全）。
type disabledClient struct{}

// NewDisabledClient 构造已禁用的监控客户端。
func NewDisabledClient() Client { return &disabledClient{} }

// Kind 返回实现类型。
func (d *disabledClient) Kind() string { return "disabled" }

// Healthy 已禁用，恒为 false。
func (d *disabledClient) Healthy(context.Context) bool { return false }

// Snapshot 返回一个带说明的空快照，明确告知无数据源。
func (d *disabledClient) Snapshot(_ context.Context, target Target) (*Snapshot, error) {
	return &Snapshot{
		InstanceID: target.InstanceID,
		MWType:     target.MWType,
		Collected:  time.Now().UTC(),
		Source:     "disabled",
		Degraded:   true,
		Note: "未配置 prometheus.base_url：当前无监控数据源（平台不使用任何模拟数据）。" +
			"请设置 MWOPS_PROMETHEUS_BASE_URL=http://prometheus:9090 并启动平台自带 Prometheus。",
		Metrics: make([]Metric, 0),
	}, nil
}

// History 无数据源，直接报错。
func (d *disabledClient) History(context.Context, Target, string, TimeRange) ([]Sample, error) {
	return nil, fmt.Errorf("监控数据源未配置：未设置 prometheus.base_url（平台不使用模拟数据）")
}

// Compare 无数据源，直接报错。
func (d *disabledClient) Compare(context.Context, []Target, string) (map[string]float64, error) {
	return nil, fmt.Errorf("监控数据源未配置：未设置 prometheus.base_url（平台不使用模拟数据）")
}
