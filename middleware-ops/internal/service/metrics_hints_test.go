package service

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"middleware-ops/internal/model"
)

// 本文件锁定「填错了该提示什么」的排序逻辑。
//
// 真实故障：纳管实例时把**容器名** redis-exporter 当成 **job 名** 填进 prom_job，
// 平台只回一句"未匹配到任何时序"，使用者无从下手。修正方式是让平台把 Prometheus 里
// 实际存在的 job 拉出来并按相似度排序，所以这个排序必须真的把正确答案排在最前面。

func TestSuggestLabelsRanksContainerNameToJobName(t *testing.T) {
	jobs := []string{
		"app-backend", "prometheus", "middleware-exporter-mysql", "middleware-exporter-redis",
	}
	// 用户把容器名 redis-exporter 填进了 job 字段。
	got := suggestLabels("redis-exporter", jobs, 3)
	if len(got) == 0 {
		t.Fatal("应给出候选 job，实际为空")
	}
	if got[0] != "middleware-exporter-redis" {
		t.Fatalf("最相近的 job 应是 middleware-exporter-redis，实际 %v", got)
	}
	// 只有两个 exporter job 得分非零，且 redis 的共同 token 更多必须排在 mysql 之前。
	if len(got) < 2 || got[1] != "middleware-exporter-mysql" {
		t.Fatalf("候选应为 redis 在前、mysql 在后，实际 %v", got)
	}
}

func TestSuggestLabelsRanksInstanceName(t *testing.T) {
	// 实例名用了容器名 app-redis，实际标签是 legacy-redis。
	got := suggestLabels("app-redis", []string{"legacy-redis", "legacy-mysql"}, 2)
	if len(got) == 0 || got[0] != "legacy-redis" {
		t.Fatalf("应优先推荐 legacy-redis，实际 %v", got)
	}
}

func TestSuggestLabelsEmptyTargetReturnsNothing(t *testing.T) {
	if got := suggestLabels("", []string{"a", "b"}, 3); got != nil {
		t.Fatalf("目标为空时不应给建议，实际 %v", got)
	}
}

func TestSuggestLabelsRespectsLimitAndExactMatch(t *testing.T) {
	jobs := []string{"redis-a", "redis-b", "redis-c", "redis-d"}
	got := suggestLabels("redis", jobs, 2)
	if len(got) != 2 {
		t.Fatalf("应受 limit 限制为 2 条，实际 %v", got)
	}
	// 完全相等时应排第一。
	exact := suggestLabels("redis-b", jobs, 3)
	if exact[0] != "redis-b" {
		t.Fatalf("完全匹配应排第一，实际 %v", exact)
	}
}

func TestSharedTokenCountAndPrefix(t *testing.T) {
	if got := sharedTokenCount("redis-exporter", "middleware-exporter-redis"); got != 2 {
		t.Fatalf("共同 token 应为 exporter/redis 两个，实际 %d", got)
	}
	if got := commonPrefixLen("middleware-exporter", "middleware-integration"); got != len("middleware-") {
		t.Fatalf("公共前缀应为 middleware-，实际 %d", got)
	}
}

func TestJobOfTargetMirrorsMonitorFallback(t *testing.T) {
	// 与 internal/monitor 的 buildSelector 约定一致：填了 prom_job 用它，
	// 否则按 <前缀>-<类型> 兜底。
	withJob := &model.MiddlewareInstance{MWType: "redis", PromJob: "custom-job"}
	if got := jobOfTarget(withJob); got != "custom-job" {
		t.Fatalf("显式 job 应原样返回，实际 %s", got)
	}
	withoutJob := &model.MiddlewareInstance{MWType: "redis"}
	if got := jobOfTarget(withoutJob); got != "middleware-exporter-redis" {
		t.Fatalf("未填 job 应按前缀兜底，实际 %s", got)
	}
}

// TestSuggestLabelsDeterministic 保证同分时输出稳定（前端展示不抖动）。
func TestSuggestLabelsDeterministic(t *testing.T) {
	jobs := []string{"redis-b", "redis-a"}
	first := suggestLabels("redis", jobs, 2)
	second := suggestLabels("redis", jobs, 2)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("同分排序应稳定：%v vs %v", first, second)
	}
}

// fakeLabelReporter 是只实现标签查询的桩，用于验证自检提示的分支。
type fakeLabelReporter struct {
	values map[string][]string
	fail   map[string]bool
}

func (f fakeLabelReporter) LabelValues(_ context.Context, label string, _ ...string) ([]string, error) {
	if f.fail[label] {
		return nil, errStub
	}
	return f.values[label], nil
}

var errStub = errors.New("stub failure")

func upValue(v float64) *float64 { return &v }

// TestLabelHintsListsActualInstanceName 锁定：标签值不同 → 直接把实际值列出来。
func TestLabelHintsListsActualInstanceName(t *testing.T) {
	reporter := fakeLabelReporter{values: map[string][]string{
		"job":           {"middleware-exporter-redis", "prometheus"},
		"instance_name": {"legacy-redis"},
		"instance":      {"redis-exporter:9121"},
	}}
	item := &model.MiddlewareInstance{Name: "app-redis", MWType: "redis", PromJob: "middleware-exporter-redis"}
	result := &DiagnoseResult{Matched: 0, Total: 8, JobUp: upValue(1)}

	hints := strings.Join(labelHints(context.Background(), reporter, item, result), "\n")
	if !strings.Contains(hints, "instance_name 实际是：legacy-redis") {
		t.Fatalf("应列出实际的 instance_name 取值，实际：\n%s", hints)
	}
	if !strings.Contains(hints, "redis-exporter:9121") {
		t.Fatalf("应同时给出 instance 标签作为备选，实际：\n%s", hints)
	}
}

// TestLabelHintsDetectMissingInstanceName 锁定：没有 instance_name 标签时，
// 要明确指向「抓取配置缺 relabel」，并给出可直接填的 instance 值。
func TestLabelHintsDetectMissingInstanceName(t *testing.T) {
	reporter := fakeLabelReporter{values: map[string][]string{
		"job":      {"middleware-exporter-redis"},
		"instance": {"redis-exporter:9121"},
	}}
	item := &model.MiddlewareInstance{Name: "legacy-redis", MWType: "redis", PromJob: "middleware-exporter-redis"}
	result := &DiagnoseResult{Matched: 0, Total: 8, JobUp: upValue(1)}

	hints := strings.Join(labelHints(context.Background(), reporter, item, result), "\n")
	if !strings.Contains(hints, "没有 instance_name 标签") {
		t.Fatalf("应判定为缺少 relabel，实际：\n%s", hints)
	}
	if !strings.Contains(hints, "Prometheus instance") || !strings.Contains(hints, "redis-exporter:9121") {
		t.Fatalf("应给出可立即生效的 Prometheus instance 取值，实际：\n%s", hints)
	}
}

// TestLabelHintsSilentWhenJobNotScraped 锁定：job 未抓取（up=0 或不存在）时
// 不输出标签候选，避免与「job 未配置」的提示互相干扰。
func TestLabelHintsSilentWhenJobNotScraped(t *testing.T) {
	reporter := fakeLabelReporter{values: map[string][]string{"job": {"middleware-exporter-redis"}}}
	item := &model.MiddlewareInstance{Name: "legacy-redis", MWType: "redis", PromJob: "middleware-exporter-redis"}
	result := &DiagnoseResult{Matched: 0, JobUp: upValue(0)}
	hints := strings.Join(labelHints(context.Background(), reporter, item, result), "\n")
	if strings.Contains(hints, "instance_name") {
		t.Fatalf("up=0 时不应给标签候选提示，实际：\n%s", hints)
	}
}

// TestLabelHintsDegradesWhenLabelQueryFails 锁定：标签查询失败不影响自检本身。
func TestLabelHintsDegradesWhenLabelQueryFails(t *testing.T) {
	reporter := fakeLabelReporter{values: map[string][]string{}, fail: map[string]bool{
		"job": true, "instance_name": true, "instance": true,
	}}
	item := &model.MiddlewareInstance{Name: "legacy-redis", MWType: "redis", PromJob: "middleware-exporter-redis"}
	result := &DiagnoseResult{Matched: 0, JobUp: upValue(1)}
	hints := labelHints(context.Background(), reporter, item, result)
	if len(hints) == 0 {
		t.Fatal("即使标签查询失败也应给出兜底提示（说明取不到标签取值）")
	}
}
