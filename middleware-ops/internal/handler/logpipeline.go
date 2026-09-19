package handler

import (
	"github.com/gin-gonic/gin"

	"middleware-ops/internal/response"
)

// LogPipelineStatus 返回日志集成（Kafka）接收链路状态。
//
// 页面用它渲染「Kafka 采集链路」卡片：启用状态、brokers、对外地址、topic、消费组、
// 已消费/丢弃/失败条数、最近一条消息时间与原因说明。
// 没有数据时页面显示「无数据」，这里也刻意不把空值补成 0（见 INC-016）。
func (h *Handler) LogPipelineStatus(c *gin.Context) {
	response.OK(c, h.deps.LogPipeline.Status())
}

// ProbeLogPipeline 探测平台侧 Kafka 可达性与日志主题是否存在。
//
// 与文件里的"测试连接"按钮一一对应。探的是**平台侧**地址；
// 目标机能否连上 Kafka 由「日志集成 → 自检」在目标机上探测（两者失败原因不同）。
func (h *Handler) ProbeLogPipeline(c *gin.Context) {
	response.OK(c, h.deps.LogPipeline.Probe(c.Request.Context()))
}
