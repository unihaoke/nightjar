package service

import (
	"context"
	"sort"
	"strings"

	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
)

// 本文件负责把「查不到指标」翻译成「你该填什么」。
//
// 背景（真实故障）：纳管实例时最容易把**容器名**当成 **Prometheus job 名**填进
// prom_job（例如把 jd-redis-exporter 填进去），而容器名、job 名、instance_name
// 是三个完全不同的东西：
//
//	容器名          jd-redis-exporter          docker ps 里看到的
//	job 名          middleware-exporter-redis prometheus.yml 的 job_name
//	instance_name   jd-redis                  relabel 写入的标签值，= 平台实例名
//
// 报错只说"未匹配到任何时序"，使用者无从下手。这里把 Prometheus 里**实际存在**的
// job 名与实例标签值拉出来，做相似度排序后直接给建议。

// labelSuggestLimit 限制每类建议条数（避免刷屏）。
const labelSuggestLimit = 6

// augmentLabelHints 在自检结果里补充「Prometheus 实际有什么」。
//
// 只在确实查不到数据时触发，且所有查询失败都静默降级——自检本身不能因为
// 附加信息查不到而失败。
func (s *MetricsService) augmentLabelHints(ctx context.Context, item *model.MiddlewareInstance, result *DiagnoseResult) []string {
	reporter, ok := s.monitor.(monitor.LabelReporter)
	if !ok || result == nil {
		return nil
	}
	return labelHints(ctx, reporter, item, result)
}

// labelHints 是自检附加提示的纯逻辑（只依赖 LabelReporter，便于单测）。
func labelHints(ctx context.Context, reporter monitor.LabelReporter, item *model.MiddlewareInstance, result *DiagnoseResult) []string {
	hints := make([]string, 0, 4)

	// 关键前提：prom_job 是空的还是错的，处理方式不同。
	if strings.TrimSpace(item.PromJob) == "" {
		hints = append(hints, "「Prometheus job」留空时平台按 <exporter_job_prefix>-<中间件类型> 兜底，当前会匹配 "+defaultJobHint(item))
	}

	// ① 列出 Prometheus 里实际存在的 job，并在用户填错时给出最接近的候选。
	if jobs, err := reporter.LabelValues(ctx, "job"); err == nil && len(jobs) > 0 {
		if item.PromJob != "" && !containsString(jobs, item.PromJob) {
			hints = append(hints, "提示：「Prometheus job」填的不是 job 名——容器名（docker ps 里看到的）、"+
				"job 名（prometheus.yml 的 job_name）、实例标签（instance_name）是三个不同的东西。")
		}
		candidates := suggestLabels(item.PromJob, jobs, labelSuggestLimit)
		if len(candidates) == 0 && item.PromJob == "" {
			candidates = suggestLabels(item.MWType, jobs, labelSuggestLimit)
		}
		if len(candidates) > 0 {
			hints = append(hints, "Prometheus 中可用的 job（按相似度排序）："+strings.Join(candidates, "、"))
		} else {
			hints = append(hints, "Prometheus 中现有 job："+strings.Join(jobs, "、"))
		}
	}

	// ② job 抓取正常但标签对不上：把该 job 下真实的标签值列出来。
	//
	// 这里要区分两种完全不同的故障（报错文案原本无法分辨）：
	//   a) 时序里没有 instance_name 标签 → 抓取配置缺 relabel_configs；
	//   b) 有 instance_name 但取值不同 → 两边改名对齐即可。
	if result.Matched == 0 && result.JobUp != nil && *result.JobUp == 1 {
		job := jobOfTarget(item)
		matcher := `up{job="` + job + `"}`
		names, nameErr := reporter.LabelValues(ctx, "instance_name", matcher)
		instances, instErr := reporter.LabelValues(ctx, "instance", matcher)
		switch {
		case nameErr == nil && len(names) > 0:
			hints = append(hints, "该 job 的实例标签 instance_name 实际是："+strings.Join(names, "、")+
				"；把「实例名称」改成其中之一（或把 Prometheus 的 relabel 值改成当前实例名）。")
			if instErr == nil && len(instances) > 0 {
				hints = append(hints, "若不方便改 Prometheus，也可把「Prometheus instance」填成："+strings.Join(instances, "、"))
			}
		case instErr == nil && len(instances) > 0:
			hints = append(hints, "该 job 的时序**没有 instance_name 标签**（抓取配置缺少 relabel_configs）："+
				"可先把「Prometheus instance」填成 "+strings.Join(instances, "、")+" 立即生效；"+
				"正解是把该实例改由「集成中心」纳管（平台写入的服务发现自带 instance_name）后重建该容器。")
		default:
			hints = append(hints, "该 job 的 target 虽为 up，但取不到标签取值：确认 Prometheus 版本 ≥ 2.24（label values 的 match[] 支持）与网络连通。")
		}
	}

	// ③ job 存在但 target 抓取失败（up=0）：把 Prometheus 记录的 lastError 取回来并翻译。
	//
	// 这一步最省时间：lastError 里通常直接写着 "Access denied for user ..." 或
	// "invalid DSN" ——不用再去翻 /targets 页面猜。
	if result.JobUp != nil && *result.JobUp == 0 {
		hints = append(hints, targetFailureHints(ctx, reporter, jobOfTarget(item))...)
	}
	return hints
}

// targetFailureHints 读取某个 job 的抓取失败原因并翻译成结论。
func targetFailureHints(ctx context.Context, reporter monitor.LabelReporter, job string) []string {
	targetReporter, ok := reporter.(monitor.TargetReporter)
	if !ok {
		return nil
	}
	targets, err := targetReporter.Targets(ctx, job)
	if err != nil {
		return []string{"未能读取 Prometheus 的 target 状态（" + err.Error() + "）：请直接查看 Prometheus 的 /targets 页面。"}
	}
	hints := make([]string, 0, 3)
	for _, target := range targets {
		if target.Health == "up" {
			continue
		}
		detail := "Prometheus 抓取 " + target.Job + " 的 target " + target.Instance + " 失败（up=0）"
		if target.LastError != "" {
			detail += "，lastError：" + target.LastError
		}
		if target.ScrapeURL != "" {
			detail += "（scrape_url=" + target.ScrapeURL + "）"
		}
		hints = append(hints, detail)
		hints = append(hints, monitor.DescribeTargetError(target.LastError))
		break
	}
	if len(hints) == 0 {
		hints = append(hints, "Prometheus 记为 up=0，但当前读不到失败详情：多半是 Exporter 容器没起来（端口无人监听）。")
	}
	return hints
}

// defaultJobHint 说明留空 prom_job 时的兜底匹配串。
func defaultJobHint(item *model.MiddlewareInstance) string {
	return "job=\"" + jobOfTarget(item) + "\""
}

// jobOfTarget 复现 monitor 侧的 job 兜底规则（仅用于文案展示）。
//
// 与 internal/monitor 的 jobOf 保持一致：填了 prom_job 用它，否则用
// <前缀>-<类型>（平台默认前缀是 middleware-exporter）。
func jobOfTarget(item *model.MiddlewareInstance) string {
	if strings.TrimSpace(item.PromJob) != "" {
		return item.PromJob
	}
	return "middleware-exporter-" + item.MWType
}

// containsString 判断切片是否包含某值。
func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

// suggestLabels 从候选里挑出与目标最接近的若干项。
//
// 打分规则（够用即可，不做编辑距离）：
//   - 互相包含（目标名是候选的一部分或反之）权重最高——用户往往只差一个前后缀；
//   - 「-」切分后的共同 token 数（exporter / redis / mysql 这类词最容易看错）；
//   - 共同前缀长度作为兜底。
func suggestLabels(target string, candidates []string, limit int) []string {
	trimmed := strings.ToLower(strings.TrimSpace(target))
	if trimmed == "" || len(candidates) == 0 {
		return nil
	}
	type scored struct {
		value string
		score int
	}
	ranked := make([]scored, 0, len(candidates))
	for _, candidate := range candidates {
		lower := strings.ToLower(candidate)
		score := commonPrefixLen(trimmed, lower)
		if strings.Contains(lower, trimmed) || strings.Contains(trimmed, lower) {
			score += 100
		}
		score += 3 * sharedTokenCount(trimmed, lower)
		if score > 0 {
			ranked = append(ranked, scored{value: candidate, score: score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].value < ranked[j].value
	})
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	out := make([]string, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.value)
	}
	return out
}

// commonPrefixLen 返回两个字符串的公共前缀长度。
func commonPrefixLen(a, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	count := 0
	for count < limit && a[count] == b[count] {
		count++
	}
	return count
}

// sharedTokenCount 统计以 '-' 切分后的共同 token 数。
func sharedTokenCount(a, b string) int {
	left := make(map[string]bool)
	for _, token := range strings.Split(a, "-") {
		if token != "" {
			left[token] = true
		}
	}
	count := 0
	seen := make(map[string]bool)
	for _, token := range strings.Split(b, "-") {
		if token == "" || seen[token] {
			continue
		}
		seen[token] = true
		if left[token] {
			count++
		}
	}
	return count
}
