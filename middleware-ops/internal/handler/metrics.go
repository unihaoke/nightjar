package handler

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/response"
)

// alertFilter 构造告警检索条件（供实例详情与大列表复用）。
func alertFilter(instanceID int64, level, status, mwType string) repository.AlertFilter {
	return repository.AlertFilter{InstanceID: instanceID, Level: level, Status: status, MWType: mwType}
}

// GetMetrics 查询实例当前指标。
func (h *Handler) GetMetrics(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	snapshot, err := h.deps.Metrics.Snapshot(c.Request.Context(), id, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, snapshot)
}

// DiagnoseMetrics 执行实例接入自检（回答「为什么这个实例没有指标」）。
func (h *Handler) DiagnoseMetrics(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	result, err := h.deps.Metrics.Diagnose(c.Request.Context(), id, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, result)
}

// GetMetricsHistory 查询指标历史趋势。
func (h *Handler) GetMetricsHistory(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	rangeParam := monitor.TimeRange{
		Start: derefTime(queryTime(c, "start")),
		End:   derefTime(queryTime(c, "end")),
		Step:  time.Duration(queryInt64(c, "step_seconds")) * time.Second,
	}
	if rangeParam.Start.IsZero() {
		hours := queryInt64(c, "hours")
		if hours <= 0 {
			hours = 6
		}
		rangeParam.Start = time.Now().UTC().Add(-time.Duration(hours) * time.Hour)
	}
	samples, err := h.deps.Metrics.History(c.Request.Context(), id, c.Query("metric"), rangeParam, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	// 空序列（Prometheus 正常响应但无时序）是合法结果：不要返回 502，
	// 用 note 解释"为什么没数据"，避免前端一片空白且被误读成故障。
	note := ""
	if len(samples) == 0 {
		note = "该指标在当前时间范围内无采样数据：可能 Exporter 未上报此指标、或标签选择器（instance_name/job）与 Prometheus 实际标签不匹配。可运行「接入自检」核对。"
	}
	response.OK(c, gin.H{
		"metric": c.Query("metric"),
		"series": samples,
		"source": h.deps.Metrics.MonitorKind(),
		"note":   note,
	})
}

// CompareMetrics 多实例对比。
func (h *Handler) CompareMetrics(c *gin.Context) {
	metric := c.Query("metric")
	if metric == "" {
		response.Fail(c, apperr.New(apperr.CodeInvalidParam, "必须指定 metric 参数"))
		return
	}
	ids := parseInt64List(c.QueryArray("instance_ids"))
	items, err := h.deps.Metrics.Compare(c.Request.Context(), ids, metric, h.scope(c))
	if err != nil {
		response.Fail(c, err)
		return
	}
	response.OK(c, gin.H{"metric": metric, "items": items, "source": h.deps.Metrics.MonitorKind()})
}

// MetricsCatalog 返回指标目录。
func (h *Handler) MetricsCatalog(c *gin.Context) {
	response.OK(c, h.deps.Metrics.Catalog(c.Query("mw_type")))
}

// derefTime 解引用时间指针，nil 返回零值。
func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// parseInt64List 解析整数数组参数（支持重复参数与逗号分隔）。
func parseInt64List(values []string) []int64 {
	out := make([]int64, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if v := atoi64Default(part, 0); v > 0 {
				out = append(out, v)
			}
		}
	}
	return out
}
