package response

import (
	"math"
	"strings"
	"testing"
)

// 本文件锁定「响应体必须可编码」这一条防线。
//
// 真实故障：命中率指标在 Redis 无读写时算出 NaN，NaN 一路进到响应体；
// gin 的 c.JSON 先写 200 状态码再 Marshal，Marshal 失败后客户端收到
// 「HTTP 200 + 空响应体」，前端解包得到 undefined，报出的却是
// "Cannot read properties of undefined (reading 'series')"——与真实原因无关。
// 现在改成先 Encode，失败即返回标准错误响应。

func TestEncodeSucceedsForNormalPayload(t *testing.T) {
	payload, err := Encode(map[string]any{"series": []int{1, 2}, "metric": "qps"})
	if err != nil {
		t.Fatalf("正常数据不应编码失败：%v", err)
	}
	text := string(payload)
	if !strings.Contains(text, `"code":0`) || !strings.Contains(text, `"series":[1,2]`) {
		t.Fatalf("编码结果不符：%s", text)
	}
}

func TestEncodeFailsForNaN(t *testing.T) {
	cases := []float64{math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, value := range cases {
		if _, err := Encode(map[string]any{"value": value}); err == nil {
			t.Fatalf("值 %v 应编码失败（JSON 无法表示 NaN/Inf）", value)
		}
	}
}

func TestEncodeReportsNestedNaN(t *testing.T) {
	// 深层嵌套里的 NaN 同样要拦住：样本序列就是这样被污染的。
	payload := map[string]any{
		"series": []map[string]any{
			{"timestamp": "2026-01-01T00:00:00Z", "value": 1.0},
			{"timestamp": "2026-01-01T00:05:00Z", "value": math.NaN()},
		},
	}
	if _, err := Encode(payload); err == nil {
		t.Fatal("嵌套的 NaN 应导致编码失败")
	}
}
