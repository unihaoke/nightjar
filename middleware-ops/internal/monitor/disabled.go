package monitor

import (
	"context"
	"fmt"
	"time"
)

// disabledClient 是「模拟数据已关闭且未接入 Prometheus」时的占位客户端。
//
// 设计意图：过去 base_url 为空会静默回退到内置模拟器，导致「实例其实没接入」
// 被假数据掩盖。关闭 mock_enabled 后，监控链路必须明确表达「无数据源」，
// 而不是用确定性曲线粉饰一个什么都不存在的监控面板。
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
		Note: "模拟数据已关闭且未配置 prometheus.base_url：当前无监控数据源。" +
			"如需离线演示可设 MWOPS_PROMETHEUS_MOCK_ENABLED=true；生产环境请接入平台自带 Prometheus。",
		Metrics: make([]Metric, 0),
	}, nil
}

// History 无数据源，直接报错。
func (d *disabledClient) History(context.Context, Target, string, TimeRange) ([]Sample, error) {
	return nil, fmt.Errorf("监控数据源未配置：模拟数据已关闭且未设置 prometheus.base_url")
}

// Compare 无数据源，直接报错。
func (d *disabledClient) Compare(context.Context, []Target, string) (map[string]float64, error) {
	return nil, fmt.Errorf("监控数据源未配置：模拟数据已关闭且未设置 prometheus.base_url")
}
