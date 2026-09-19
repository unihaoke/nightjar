package service

import (
	"testing"

	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
)

// 锁定「周期自愈清谁的待处理」的匹配规则（INC-015 / INC-025）。
//
// 规则写错的后果不对称：匹配错了会把**别的集成**的待处理当成自己的清除掉，
// 于是真正的故障静默消失。因此宁可判不出（found=false），也不许张冠李戴。

// target 构造一条抓取目标；mwType 为空表示目标没有 mw_type 标签（早期产物/人工配置）。
func target(instanceName, health, mwType string) monitor.TargetStatus {
	labels := map[string]string{"job": "middleware-integration"}
	if instanceName != "" {
		labels["instance_name"] = instanceName
	}
	if mwType != "" {
		labels["mw_type"] = mwType
	}
	return monitor.TargetStatus{Health: health, Labels: labels, Instance: "10.0.0.1:9121"}
}

func TestPickTargetStatusPrefersInstanceName(t *testing.T) {
	statuses := []monitor.TargetStatus{
		target("other-app", "up", integration.TypeRedis),
		target("jd-redis", "down", integration.TypeRedis),
		target("third", "up", integration.TypeRedis),
	}
	got, found := pickTargetStatus(statuses, "jd-redis", integration.TypeRedis)
	if !found {
		t.Fatal("应按 instance_name 精确匹配到 jd-redis")
	}
	if got.Health != "down" {
		t.Fatalf("应返回本实例那条（down），实际 %+v", got)
	}
}

func TestPickTargetStatusRefusesToGuess(t *testing.T) {
	// 多个目标但都没有 instance_name → 不猜（否则可能清错对象）。
	statuses := []monitor.TargetStatus{target("", "up", ""), target("", "up", "")}
	if _, found := pickTargetStatus(statuses, "jd-redis", integration.TypeRedis); found {
		t.Fatal("多目标且无 instance_name 时不得猜测")
	}

	// 一个目标都没有 → 未找到。
	if _, found := pickTargetStatus(nil, "jd-redis", integration.TypeRedis); found {
		t.Fatal("没有目标时应返回未找到")
	}

	// 名字对不上 → 未找到。
	statuses = []monitor.TargetStatus{target("other-app", "up", integration.TypeRedis)}
	if _, found := pickTargetStatus(statuses, "jd-redis", integration.TypeRedis); found {
		t.Fatal("名字对不上时不得返回别的目标")
	}
}

func TestPickTargetStatusSingleTargetFallback(t *testing.T) {
	// job 下只有一个目标且它没有 instance_name（早期产物/人工配置）→ 认它。
	statuses := []monitor.TargetStatus{target("", "up", "")}
	if _, found := pickTargetStatus(statuses, "jd-redis", integration.TypeRedis); !found {
		t.Fatal("唯一目标应作为兜底匹配")
	}
	// 目标没写 mw_type 时也不因类型判定而拒绝（否则正常集成会被判成"没有目标"）。
	statuses = []monitor.TargetStatus{target("jd-redis", "up", "")}
	if _, found := pickTargetStatus(statuses, "jd-redis", integration.TypeRedis); !found {
		t.Fatal("目标缺 mw_type 标签时应按名字匹配（兼容早期产物）")
	}
}

// TestPickTargetStatusRefusesCrossTypeMatch 是 INC-025 的回归守卫。
//
// 真实故障：日志集成（它根本没有抓取目标）在周期核验里认领到了一个
// **同名中间件**的抓取目标，于是界面上出现
// 「周期核验通过：抓取目标 jd-redis 已 up，已自动清除待处理」——
// 使用者看到的是"我的日志集成怎么在核验 redis"。
// 因此匹配必须同时看 mw_type：类型不同就不认领，宁可不判定。
func TestPickTargetStatusRefusesCrossTypeMatch(t *testing.T) {
	statuses := []monitor.TargetStatus{
		target("jd-redis", "up", integration.TypeRedis),
	}
	if _, found := pickTargetStatus(statuses, "jd-redis", model.MWTypeLog); found {
		t.Fatal("日志集成绝不能认领 redis 的抓取目标（跨类型同名）——这正是 INC-025")
	}
	if _, found := pickTargetStatus(statuses, "jd-redis", integration.TypeMySQL); found {
		t.Fatal("类型不同时不得认领别人的目标")
	}
	// 同类型同名才是它自己。
	if _, found := pickTargetStatus(statuses, "jd-redis", integration.TypeRedis); !found {
		t.Fatal("同类型同名时应正常匹配")
	}
}

// TestVerifyViaPrometheusSkipsLogIntegration 锁定"日志集成不参与抓取核验"。
func TestVerifyViaPrometheusSkipsLogIntegration(t *testing.T) {
	if verifyViaPrometheus(model.MWTypeLog) {
		t.Fatal("日志集成没有抓取目标，不得走 Prometheus 核验通道（INC-025）")
	}
	for _, mwType := range []string{integration.TypeRedis, integration.TypeMySQL, integration.TypeNode} {
		if !verifyViaPrometheus(mwType) {
			t.Fatalf("%s 有抓取目标，必须继续走核验通道", mwType)
		}
	}
}

// 自愈的判定必须是"明确 up 才清除"。
func TestReverifyOnlyClearsOnExplicitUp(t *testing.T) {
	cases := []struct {
		name      string
		statuses  []monitor.TargetStatus
		wantClear bool
	}{
		{"目标 up → 清除", []monitor.TargetStatus{target("jd-redis", "up", integration.TypeRedis)}, true},
		{"目标 down → 保持", []monitor.TargetStatus{target("jd-redis", "down", integration.TypeRedis)}, false},
		{"目标 unknown → 保持", []monitor.TargetStatus{target("jd-redis", "unknown", integration.TypeRedis)}, false},
		{"目标缺失 → 保持", nil, false},
		{"只有别的集成 up → 保持", []monitor.TargetStatus{target("other", "up", integration.TypeRedis)}, false},
		// 同名但类型不同 → 不认领，因此不清除（INC-025 的现场）。
		{"同名但类型不同 → 保持", []monitor.TargetStatus{target("jd-redis", "up", integration.TypeMySQL)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, found := pickTargetStatus(c.statuses, "jd-redis", integration.TypeRedis)
			clear := found && status.Health == "up"
			if clear != c.wantClear {
				t.Fatalf("want clear=%v, got %v（statuses=%+v）", c.wantClear, clear, c.statuses)
			}
		})
	}
}
