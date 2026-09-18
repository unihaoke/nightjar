package service

import (
	"testing"

	"middleware-ops/internal/integration"
	"middleware-ops/internal/monitor"
)

// 本文件锁定「推荐告警规则只能引用真实存在的指标」。
//
// 为什么值得测：模板里的 Alerts 会在使用者勾选「自动创建推荐告警规则」时变成真实规则，
// 而这些规则按 **profile 里的指标名** 取数（AlertService 用 SpecOf(mw_type, metric) 解析）。
// 一旦模板引用了一个画像里没有的指标名，规则会静默失效——页面不报错，只是永不触发。
// 补齐指标（INC-017）时正是靠这条守卫确认新告警有据可依。

func TestTemplateAlertMetricsExistInProfiles(t *testing.T) {
	checked := 0
	for _, tpl := range integration.Templates() {
		profile := monitor.ProfileOf(tpl.Type)
		if len(profile.Metrics) == 0 {
			t.Fatalf("组件 %s 没有指标画像，推荐告警必然失效", tpl.Type)
		}
		names := make(map[string]bool, len(profile.Metrics))
		for _, spec := range profile.Metrics {
			names[spec.Name] = true
		}
		for _, alert := range tpl.Alerts {
			checked++
			if alert.MetricName == "" {
				t.Fatalf("%s 的推荐告警 %q 没有指定指标", tpl.Type, alert.Name)
			}
			if !names[alert.MetricName] {
				t.Fatalf("%s 的推荐告警 %q 引用了画像里不存在的指标 %q（该规则永远不会触发）",
					tpl.Type, alert.Name, alert.MetricName)
			}
		}
	}
	if checked == 0 {
		t.Fatal("没有检查到任何推荐告警，测试失效")
	}
}

// 统一监控页的指标下拉 = 画像顺序，**第一条是切换实例后的默认指标**。
// 因此每个组件的画像必须非空且第一条可用（名字与 PromQL 都不能为空）。
func TestProfileFirstMetricIsUsable(t *testing.T) {
	for _, tpl := range integration.Templates() {
		profile := monitor.ProfileOf(tpl.Type)
		if len(profile.Metrics) == 0 {
			t.Fatalf("%s 画像为空，统一监控页会没有指标可选", tpl.Type)
		}
		first := profile.Metrics[0]
		if first.Name == "" || first.DisplayName == "" {
			t.Fatalf("%s 的第一个指标缺少名称/展示名：%+v", tpl.Type, first)
		}
		if first.Expr == "" {
			t.Fatalf("%s 的第一个指标 %s 没有 PromQL", tpl.Type, first.Name)
		}
	}
}

// mysql / redis 的可用性指标必须在画像里且排在第一位：切到该组件后默认就能看到"它是不是活的"。
func TestAvailabilityMetricComesFirst(t *testing.T) {
	cases := map[string]string{
		integration.TypeRedis: "redis_up",
		integration.TypeMySQL: "mysql_up",
	}
	for mwType, want := range cases {
		profile := monitor.ProfileOf(mwType)
		if len(profile.Metrics) == 0 {
			t.Fatalf("%s 画像为空", mwType)
		}
		if got := profile.Metrics[0].Name; got != want {
			t.Fatalf("%s 的第一个指标应为可用性 %s，实际 %s", mwType, want, got)
		}
		if _, ok := monitor.SpecOf(mwType, want); !ok {
			t.Fatalf("%s 应能按名字取到 %s", mwType, want)
		}
	}
}
