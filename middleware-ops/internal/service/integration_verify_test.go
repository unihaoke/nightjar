package service

import (
	"testing"

	"middleware-ops/internal/monitor"
)

// 锁定「周期自愈清谁的待处理」的匹配规则（INC-015）。
//
// 规则写错的后果不对称：匹配错了会把**别的集成**的待处理当成自己的清除掉，
// 于是真正的故障静默消失。因此宁可判不出（found=false），也不许张冠李戴。

func target(instanceName, health string) monitor.TargetStatus {
	labels := map[string]string{"job": "middleware-integration"}
	if instanceName != "" {
		labels["instance_name"] = instanceName
	}
	return monitor.TargetStatus{Health: health, Labels: labels, Instance: "10.0.0.1:9121"}
}

func TestPickTargetStatusPrefersInstanceName(t *testing.T) {
	statuses := []monitor.TargetStatus{
		target("other-app", "up"),
		target("jd-redis", "down"),
		target("third", "up"),
	}
	got, found := pickTargetStatus(statuses, "jd-redis")
	if !found {
		t.Fatal("应按 instance_name 精确匹配到 jd-redis")
	}
	if got.Health != "down" {
		t.Fatalf("应返回本实例那条（down），实际 %+v", got)
	}
}

func TestPickTargetStatusRefusesToGuess(t *testing.T) {
	// 多个目标但都没有 instance_name → 不猜（否则可能清错对象）。
	statuses := []monitor.TargetStatus{target("", "up"), target("", "up")}
	if _, found := pickTargetStatus(statuses, "jd-redis"); found {
		t.Fatal("多目标且无 instance_name 时不得猜测")
	}

	// 一个目标都没有 → 未找到。
	if _, found := pickTargetStatus(nil, "jd-redis"); found {
		t.Fatal("没有目标时应返回未找到")
	}

	// 名字对不上 → 未找到。
	statuses = []monitor.TargetStatus{target("other-app", "up")}
	if _, found := pickTargetStatus(statuses, "jd-redis"); found {
		t.Fatal("名字对不上时不得返回别的目标")
	}
}

func TestPickTargetStatusSingleTargetFallback(t *testing.T) {
	// job 下只有一个目标且它没有 instance_name（早期产物/人工配置）→ 认它。
	statuses := []monitor.TargetStatus{target("", "up")}
	if _, found := pickTargetStatus(statuses, "jd-redis"); !found {
		t.Fatal("唯一目标应作为兜底匹配")
	}
}

// 自愈的判定必须是"明确 up 才清除"。
func TestReverifyOnlyClearsOnExplicitUp(t *testing.T) {
	cases := []struct {
		name      string
		statuses  []monitor.TargetStatus
		wantClear bool
	}{
		{"目标 up → 清除", []monitor.TargetStatus{target("jd-redis", "up")}, true},
		{"目标 down → 保持", []monitor.TargetStatus{target("jd-redis", "down")}, false},
		{"目标 unknown → 保持", []monitor.TargetStatus{target("jd-redis", "unknown")}, false},
		{"目标缺失 → 保持", nil, false},
		{"只有别的集成 up → 保持", []monitor.TargetStatus{target("other", "up")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, found := pickTargetStatus(c.statuses, "jd-redis")
			clear := found && status.Health == "up"
			if clear != c.wantClear {
				t.Fatalf("want clear=%v, got %v（statuses=%+v）", c.wantClear, clear, c.statuses)
			}
		})
	}
}
