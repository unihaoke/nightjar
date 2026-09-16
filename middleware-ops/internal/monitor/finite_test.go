package monitor

import (
	"encoding/json"
	"math"
	"testing"
)

// 本文件锁定「Prometheus 的非有限值」这一真实故障的防线。
//
// 背景：命中率指标 = rate(redis_keyspace_hits_total)/(rate(hits)+rate(misses))*100。
// Redis 在窗口内没有读写时两侧都是 0，Prometheus 返回字符串 "NaN"；
// 参考 Redis 内存上限未设置时 redis_memory_max_bytes=0，比值则是 "+Inf"。
// Go 的 strconv.ParseFloat 会**成功**解析这些字符串，于是 NaN/±Inf 会一路流进响应，
// 而 encoding/json 无法编码它们：gin 先写 200 再 Marshal，客户端拿到空 body，
// 前端最终报 "Cannot read properties of undefined (reading 'series')"。
// 因此在数据源头就挡掉，并保证解析结果要么是有限数、要么是明确的"无数据"。

func TestFirstValueRejectsNonFinite(t *testing.T) {
	cases := []string{"NaN", "+Inf", "-Inf", "Inf"}
	for _, raw := range cases {
		resp := promResponse{}
		resp.Data.Result = []promResultItem{{Value: []any{1700000000.0, raw}}}
		value, err := resp.firstValue()
		if err == nil {
			t.Fatalf("样本值 %q 应被判定为无可用数值，实际返回 %v", raw, value)
		}
		if err != errEmptyResult {
			t.Fatalf("样本值 %q 应返回 errEmptyResult（语义：无数据），实际 %v", raw, err)
		}
	}
}

func TestFirstValueAcceptsFinite(t *testing.T) {
	resp := promResponse{}
	resp.Data.Result = []promResultItem{{Value: []any{1700000000.0, "42.5"}}}
	value, err := resp.firstValue()
	if err != nil {
		t.Fatalf("正常数值不应报错：%v", err)
	}
	if value != 42.5 {
		t.Fatalf("数值解析错误：%v", value)
	}
}

func TestFirstSeriesSkipsNonFinitePoints(t *testing.T) {
	resp := promResponse{}
	resp.Data.Result = []promResultItem{{
		Values: [][]any{
			{1700000000.0, "1.5"},
			{1700000060.0, "NaN"},  // 该点无数据（如 0/0）
			{1700000120.0, "+Inf"}, // 该点无数据（如除以 0）
			{1700000180.0, "-Inf"}, // 该点无数据
			{1700000240.0, "2.5"},
		},
	}}
	samples, err := resp.firstSeries()
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("应只保留 2 个有限点，实际 %d：%+v", len(samples), samples)
	}
	if samples[0].Value != 1.5 || samples[1].Value != 2.5 {
		t.Fatalf("保留的点不正确：%+v", samples)
	}
}

func TestFirstSeriesAllNonFiniteYieldsEmpty(t *testing.T) {
	resp := promResponse{}
	resp.Data.Result = []promResultItem{{
		Values: [][]any{{1700000000.0, "NaN"}, {1700000060.0, "NaN"}},
	}}
	samples, err := resp.firstSeries()
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if len(samples) != 0 {
		t.Fatalf("全部为 NaN 时应返回空序列，实际 %d", len(samples))
	}
	// 空序列必须能被 JSON 编码为 []（而不是 null 或失败）
	if payload, err := json.Marshal(samples); err != nil || string(payload) != "[]" {
		t.Fatalf("空序列应编码为 []，实际 %s（err=%v）", payload, err)
	}
}

func TestIsFinite(t *testing.T) {
	if !isFinite(0) || !isFinite(-1.5) || !isFinite(math.MaxFloat64) {
		t.Fatal("有限数应判定为 true")
	}
	if isFinite(math.Inf(1)) || isFinite(math.Inf(-1)) || isFinite(math.NaN()) {
		t.Fatal("NaN/±Inf 应判定为 false")
	}
}
