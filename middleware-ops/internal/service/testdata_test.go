package service

import (
	"github.com/unihaoke/nightjar/middleware-ops/internal/config"
	"github.com/unihaoke/nightjar/middleware-ops/internal/model"
	"github.com/unihaoke/nightjar/middleware-ops/internal/monitor"
	"github.com/unihaoke/nightjar/middleware-ops/internal/repository"
)

// monitorMetricStub 构造监控指标测试数据。
func monitorMetricStub(name, display, unit string) monitor.Metric {
	return monitor.Metric{Name: name, DisplayName: display, Unit: unit}
}

// knowledgePublishedFilter 构造「仅已发布」检索条件（与向量候选集语义一致）。
func knowledgePublishedFilter() repository.KnowledgeFilter {
	return repository.KnowledgeFilter{Status: model.KnowledgeStatusPublished, OnlyPub: true}
}

// securityConfigStub 构造脱敏配置测试数据。
func securityConfigStub() config.SecurityConfig {
	return config.SecurityConfig{CodeSnippetMaxLines: 200}
}
