package monitor

import (
	"strings"
	"testing"
)

// 本文件锁定「接入自检」那句结论的措辞。
//
// 为什么值得单独测：这句话是使用者排查时最先看到的东西，措辞错会直接把人带偏。
// 真实案例：job 里有 MySQL（正常）和 Redis（Exporter 挂了）两条 target，
// `up{job=...}` 返回 1，旧文案写成"已正常抓取（up=1）"，于是使用者去改实例名，
// 而真正的问题是 Redis 的 Exporter 根本没起来。

func TestSnapshotNoteJobUpIsNotInstanceUp(t *testing.T) {
	jobUp := 1.0
	note := buildSnapshotNote(`job="middleware-integration",instance_name="jd-redis"`,
		"middleware-integration", &jobUp, 0, 0, 0)

	if !strings.Contains(note, "job 级 up=1") {
		t.Fatalf("必须点明这是 job 级判定，实际：%s", note)
	}
	if strings.Contains(note, "已正常抓取") {
		t.Fatalf("不能写成「已正常抓取」——同 job 其他实例正常也会 up=1，实际：%s", note)
	}
	if !strings.Contains(note, "本实例") {
		t.Fatalf("应把注意力引回本实例那条 target，实际：%s", note)
	}
	// 仍然要保留"标签对齐"这条正确路径（确实是标签问题时用得上）。
	if !strings.Contains(note, "instance_name") {
		t.Fatalf("标签问题时仍需给出对齐指引，实际：%s", note)
	}
}

func TestSnapshotNoteCoversJobMissingAndTargetDown(t *testing.T) {
	if note := buildSnapshotNote("s", "j", nil, 0, 0, 0); !strings.Contains(note, "尚未在 Prometheus 中配置") {
		t.Fatalf("job 不存在时应直接指出，实际：%s", note)
	}
	zero := 0.0
	if note := buildSnapshotNote("s", "j", &zero, 0, 0, 0); !strings.Contains(note, "up=0") {
		t.Fatalf("target 挂掉时应指出 up=0，实际：%s", note)
	}
	// 有命中且没有缺失项 → 不打扰使用者（这是既有契约）。
	if note := buildSnapshotNote("s", "j", &zero, 3, 0, 0); note != "" {
		t.Fatalf("指标齐备时不应输出结论，实际：%s", note)
	}
	// 有命中但有缺失项 → 只做信息性说明，不误导排查方向。
	if note := buildSnapshotNote("s", "j", &zero, 3, 1, 1); !strings.Contains(note, "命中 3 项") {
		t.Fatalf("有缺失项时应说明命中与缺失数量，实际：%s", note)
	}
}
