package monitor

import (
	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/pkg/cache"
)

// New 依据配置创建监控客户端。
//
// **只使用真实数据**（产品约定）：未配置 prometheus.base_url 时，返回「无数据源」客户端，
// 页面明确显示"无数据"，而不是用确定性曲线粉饰一个什么都不存在的监控面板。
// 历史上这里还有一条"查询失败回退内置模拟器"的通路，已彻底移除——
// 假数据会让"实例其实没接入""Exporter 连不上被管实例"这类问题被一条看起来很正常的曲线掩盖，
// 而且使用者无法分辨屏幕上哪个数字是真的。
func New(cfg *config.Config, store cache.Store, log *zap.Logger) Client {
	if cfg.Prometheus.BaseURL == "" {
		log.Warn("未配置 prometheus.base_url：监控数据源已禁用（平台不使用任何模拟数据）",
			zap.String("hint", "设置 MWOPS_PROMETHEUS_BASE_URL=http://prometheus:9090 并启动平台自带 Prometheus"))
		return NewDisabledClient()
	}
	return NewPrometheusClient(cfg, store, log)
}

// 说明：这里曾经有一个 fallbackClient（Prometheus 查询失败时回退模拟器）。
// 已删除，理由是它把"后端不可达"伪装成"指标正常"，并且顺手掩盖了选择器写错导致的空结果。
// 现在查询失败就直接返回错误，由界面显示"无数据 + 原因"。
