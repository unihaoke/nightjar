package monitor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
)

// 本文件锁定「接入排障」的语义边界，防止回归成"看不见的假数据"。
//
// 背景（真实缺陷）：早期实现把两类完全不同的情况混为一谈——
//  1. Prometheus 不可达（请求失败）：应当降级为模拟器保证页面可用；
//  2. Prometheus 正常响应但选择器没匹配到时序（接入配置错）：必须如实暴露。
//
// 混同的后果是：选择器写错时历史趋势仍会用模拟器补齐，前端画出貌似正常的曲线，
// 用户以为"接上了"，实际一条真实指标都没有——这正是「新增实例后没有监控输出」
// 最难排查的成因。因此用桩服务把两种情形固化成测试。

// stubMode 决定桩服务的行为。
type stubMode string

const (
	// stubEmpty 模拟「Prometheus 正常，但选择器什么都不匹配」（含 up{} 也不存在）。
	stubEmpty stubMode = "empty"
	// stubValue 模拟「指标正常返回」。
	stubValue stubMode = "value"
	// stubDown 模拟「Prometheus 不可达」。
	stubDown stubMode = "down"
)

// stubHandler 返回一个可控的 Prometheus HTTP 桩。
func stubHandler(mode stubMode) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if mode == stubDown {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if mode == stubEmpty {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
			return
		}
		query := r.URL.Query().Get("query")
		if strings.HasPrefix(query, "up{") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"1"]}]}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/query_range") {
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"12.5"],[1700000060,"13.5"]]}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"42"]}]}}`))
	}
}

// newStubClient 构造指向桩服务的监控客户端（含降级链）。
func newStubClient(t *testing.T, mode stubMode) (Client, func()) {
	t.Helper()
	server := httptest.NewServer(stubHandler(mode))
	cfg := &config.Config{}
	cfg.Prometheus.BaseURL = server.URL
	cfg.Prometheus.Timeout = 2 * time.Second
	cfg.Prometheus.ExporterJobPrefix = "middleware-exporter"
	return New(cfg, nil, zap.NewNop()), server.Close
}

// sampleRedisTarget 是测试用的实例（对应 跨项目场景）。
func sampleRedisTarget() Target {
	return Target{InstanceID: 1, Name: "legacy-redis", MWType: "redis"}
}

// TestSnapshotExposesSelectorWhenNothingMatches 锁定：
// 选择器无匹配时必须回传选择器、job 状态与原因，而不是静默返回一堆 unknown/0。
func TestSnapshotExposesSelectorWhenNothingMatches(t *testing.T) {
	client, closeFn := newStubClient(t, stubEmpty)
	defer closeFn()

	snapshot, err := client.Snapshot(context.Background(), sampleRedisTarget())
	if err != nil {
		t.Fatalf("空结果不应返回错误（否则会掩盖真实原因）：%v", err)
	}
	if snapshot.Source != "prometheus" {
		t.Fatalf("选择器无匹配属于接入错误，不得降级为模拟器，实际 source=%s", snapshot.Source)
	}
	if snapshot.Matched != 0 {
		t.Fatalf("命中数应为 0，实际 %d", snapshot.Matched)
	}
	if snapshot.Total == 0 {
		t.Fatal("必须回传画像指标总数，否则前端无法判断「一条都没命中」")
	}
	wantSelector := `job="middleware-exporter-redis",instance_name="legacy-redis"`
	if snapshot.Selector != wantSelector {
		t.Fatalf("选择器不符：want %s, got %s", wantSelector, snapshot.Selector)
	}
	if snapshot.JobUp != nil {
		t.Fatalf("Prometheus 中不存在该 job 时应回传 job_up=nil，实际 %v", *snapshot.JobUp)
	}
	if !strings.Contains(snapshot.Note, "未匹配到任何时序") {
		t.Fatalf("必须给出人可读的原因，实际 note=%q", snapshot.Note)
	}
}

// TestHistoryDoesNotFabricateDataOnEmptyResult 锁定：
// 空结果绝不能用模拟数据补齐（否则趋势图会骗人）。
func TestHistoryDoesNotFabricateDataOnEmptyResult(t *testing.T) {
	client, closeFn := newStubClient(t, stubEmpty)
	defer closeFn()

	samples, err := client.History(context.Background(), sampleRedisTarget(), "memory_usage_percent", TimeRange{})
	if err != nil {
		t.Fatalf("空结果不应报错：%v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("选择器无匹配时不得用模拟数据补齐，实际返回 %d 个采样点", len(samples))
	}
}

// TestSnapshotDegradesToSimulatorWhenPrometheusDown 锁定：
// 上游真的挂了才降级为模拟器，且降级后仍要回传真实选择器供排障。
func TestSnapshotDegradesToSimulatorWhenPrometheusDown(t *testing.T) {
	client, closeFn := newStubClient(t, stubDown)
	defer closeFn()

	snapshot, err := client.Snapshot(context.Background(), sampleRedisTarget())
	if err != nil {
		t.Fatalf("降级链应兜住上游故障：%v", err)
	}
	if snapshot.Source != "simulator" || !snapshot.Degraded {
		t.Fatalf("Prometheus 不可达时应降级为模拟器，实际 source=%s degraded=%v", snapshot.Source, snapshot.Degraded)
	}
	if !strings.Contains(snapshot.Note, "已回退") {
		t.Fatalf("降级必须显式提示，实际 note=%q", snapshot.Note)
	}
	if snapshot.Selector != `job="middleware-exporter-redis",instance_name="legacy-redis"` {
		t.Fatalf("降级后仍应回传真实选择器，实际 %q", snapshot.Selector)
	}
}

// TestSnapshotMatchesWhenJobAndLabelsAgree 锁定：
// 选择器命中时不得产生噪声提示，job_up 应为 1。
func TestSnapshotMatchesWhenJobAndLabelsAgree(t *testing.T) {
	client, closeFn := newStubClient(t, stubValue)
	defer closeFn()

	snapshot, err := client.Snapshot(context.Background(), sampleRedisTarget())
	if err != nil {
		t.Fatalf("正常路径不应报错：%v", err)
	}
	if snapshot.Matched != snapshot.Total || snapshot.Total == 0 {
		t.Fatalf("应全部命中，实际 %d/%d", snapshot.Matched, snapshot.Total)
	}
	if snapshot.JobUp == nil || *snapshot.JobUp != 1 {
		t.Fatalf("job 已被抓取时应回传 job_up=1，实际 %v", snapshot.JobUp)
	}
	if snapshot.Note != "" {
		t.Fatalf("一切正常时不应产生提示，实际 note=%q", snapshot.Note)
	}

	// 历史查询走真实数据源，不应被模拟器接管。
	samples, err := client.History(context.Background(), sampleRedisTarget(), "memory_usage_percent", TimeRange{})
	if err != nil {
		t.Fatalf("历史查询不应报错：%v", err)
	}
	if len(samples) != 2 || samples[0].Value != 12.5 {
		t.Fatalf("应返回 Prometheus 的真实序列，实际 %+v", samples)
	}
}

// TestDiagnoseSelectorIgnoresInstanceNameWhenInstanceFilled 锁定 buildSelector 的二选一语义：
// 填了 prom_instance 就用 instance 标签，实例名不再参与匹配（接入说明必须与此一致）。
func TestDiagnoseSelectorIgnoresInstanceNameWhenInstanceFilled(t *testing.T) {
	selector := SelectorFor(Target{Name: "legacy-redis", MWType: "redis", Instance: "10.0.0.11:6379"}, "middleware-exporter")
	if selector != `job="middleware-exporter-redis",instance="10.0.0.11:6379"` {
		t.Fatalf("填了 prom_instance 时应改用 instance 标签，实际 %s", selector)
	}
}
