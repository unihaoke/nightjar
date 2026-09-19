package service

import (
	"strings"
	"testing"

	"middleware-ops/internal/model"
)

// 本文件锁定「日志告警屏蔽项」的判定语义与入参校验。
//
// 为什么值得测：屏蔽项的效果是**让告警消失**。判错的两个方向都很糟——
// 该屏蔽的没屏蔽（噪音继续淹没人），不该屏蔽的被屏蔽（故障悄悄消失，且没有任何记录）。
// 后者尤其危险：被屏蔽的日志连事件都不落库，页面上查不到它存在过。

// TestMatchLogAlertExclusion 校验子串/正则/服务/启用/顺序五个维度。
func TestMatchLogAlertExclusion(t *testing.T) {
	items := []model.LogAlertExclusion{
		{Base: model.Base{ID: 1}, Pattern: "Request method 'GET' is not supported", Enabled: true},
		{Base: model.Base{ID: 2}, Pattern: "/connection (timeout|refused)/", Enabled: true},
		{Base: model.Base{ID: 3}, Pattern: "Broken pipe", Enabled: true, ServiceName: "order-api"},
		{Base: model.Base{ID: 4}, Pattern: "noisy but disabled", Enabled: false},
	}
	cases := []struct {
		name    string
		service string
		message string
		wantID  int64
		wantHit bool
	}{
		{"普通文本按子串匹配", "any", "Resolved [org.springframework...]: Request method 'GET' is not supported", 1, true},
		{"/re/ 形式按正则匹配", "any", "dial tcp: connection refused by peer", 2, true},
		{"服务名匹配才生效", "order-api", "write: Broken pipe", 3, true},
		{"服务名不匹配则不生效", "pay-api", "write: Broken pipe", 0, false},
		{"停用项永不命中", "any", "noisy but disabled", 0, false},
		{"多条命中取第一条", "order-api", "Broken pipe + Request method 'GET' is not supported", 1, true},
		{"没有命中就不屏蔽", "order-api", "NullPointerException at OrderService", 0, false},
	}
	for _, c := range cases {
		got, hit := MatchLogAlertExclusion(items, c.service, c.message)
		if hit != c.wantHit {
			t.Fatalf("%s: 命中=%v，期望 %v", c.name, hit, c.wantHit)
		}
		if hit && got.ID != c.wantID {
			t.Fatalf("%s: 命中屏蔽项 #%d，期望 #%d", c.name, got.ID, c.wantID)
		}
	}
}

// TestBuildLogAlertExclusionValidation 校验屏蔽项的入参边界。
func TestBuildLogAlertExclusionValidation(t *testing.T) {
	svc := &LogAlertService{}
	enabled := false

	// 空内容必须被挡住：空 pattern 会让 signatureMatches 对任意消息返回 true，
	// 等于"屏蔽全部日志"——告警体系整体静默，这是最严重的一种误配置。
	if _, err := svc.buildExclusion(model.LogAlertExclusion{}, LogAlertExclusionInput{Pattern: "  "}); err == nil ||
		!strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空屏蔽内容应报错：%v", err)
	}

	// 非法正则必须在保存时挡住：放到运行期才发现，表现是"配了屏蔽却还在告警"。
	if _, err := svc.buildExclusion(model.LogAlertExclusion{},
		LogAlertExclusionInput{Pattern: "/(unclosed/"}); err == nil {
		t.Fatal("非法正则应报错")
	}

	if _, err := svc.buildExclusion(model.LogAlertExclusion{},
		LogAlertExclusionInput{Pattern: strings.Repeat("a", exclusionMaxPattern+1)}); err == nil {
		t.Fatal("超长屏蔽内容应报错")
	}

	// 合法入参：正则形式与普通文本都要能保存，且启用状态按入参走。
	item, err := svc.buildExclusion(model.LogAlertExclusion{Enabled: true},
		LogAlertExclusionInput{Name: " 框架噪音 ", Pattern: " /GET is not supported/ ", Enabled: &enabled})
	if err != nil {
		t.Fatalf("合法入参不应报错：%v", err)
	}
	if item.Pattern != "/GET is not supported/" || item.Name != "框架噪音" {
		t.Fatalf("入参未按预期规整：pattern=%q name=%q", item.Pattern, item.Name)
	}
	if item.Enabled {
		t.Fatal("显式关闭的屏蔽项应保持关闭")
	}

	// 更新时未传 enabled 应保持原值（只改备注不会把屏蔽项悄悄停用）。
	item, err = svc.buildExclusion(model.LogAlertExclusion{Enabled: true},
		LogAlertExclusionInput{Pattern: "Broken pipe"})
	if err != nil {
		t.Fatalf("合法入参不应报错：%v", err)
	}
	if !item.Enabled {
		t.Fatal("未传 enabled 时应保持原值")
	}
}
