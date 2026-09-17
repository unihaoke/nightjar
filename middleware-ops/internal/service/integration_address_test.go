package service

import (
	"strings"
	"testing"

	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
)

// 本文件锁定「集成地址的两种视角」（真实反馈：Exporter 打了自己的公网 IP）。

func TestExporterSideAddressRewritesSameMachine(t *testing.T) {
	addr := integration.Address{Host: "203.195.191.75", Port: 6379}

	// 远程部署 + 地址就是目标机 → 改用回环（Exporter 与 Redis 同机）。
	got, note := exporterSideAddress(addr, "203.195.191.75")
	if got.Host != "127.0.0.1" || got.Port != 6379 {
		t.Fatalf("同机应改用 127.0.0.1，实际 %s:%d", got.Host, got.Port)
	}
	if !strings.Contains(note, "127.0.0.1") || !strings.Contains(note, "203.195.191.75") {
		t.Fatalf("改写必须给使用者一句说明：%s", note)
	}

	// 已经是回环 → 不动，也不需要说明。
	loop := integration.Address{Host: "127.0.0.1", Port: 6379}
	if got, note := exporterSideAddress(loop, "203.195.191.75"); got.Host != "127.0.0.1" || note != "" {
		t.Fatalf("回环地址不应改写也不应提示，实际 %s / %s", got.Host, note)
	}

	// 被管实例在**别的机器**上 → 原样使用（那才是 Exporter 该连的地址）。
	other := integration.Address{Host: "10.0.0.5", Port: 3306}
	got, note = exporterSideAddress(other, "203.195.191.75")
	if got.Host != "10.0.0.5" || note != "" {
		t.Fatalf("异机地址不应改写，实际 %s / %s", got.Host, note)
	}

	// 目标机未知（还没填）→ 不动。
	if got, _ := exporterSideAddress(addr, ""); got.Host != "203.195.191.75" {
		t.Fatalf("目标机未知时不应改写，实际 %s", got.Host)
	}
}

func TestPlatformSideProbeSkipReason(t *testing.T) {
	remoteLoop := model.MiddlewareInstance{
		Name: "jd-redis", Host: "127.0.0.1", Port: 6379,
		Config: model.JSONMap{"integration": map[string]any{
			"template": integration.TypeRedis, "deploy_target": "remote",
		}},
	}
	reason, skip := platformSideProbeSkipReason(remoteLoop)
	if !skip {
		t.Fatal("远程 + 回环地址应跳过平台侧探测（否则打到平台自己）")
	}
	if !strings.Contains(reason, "127.0.0.1:6379") || !strings.Contains(reason, "已跳过") {
		t.Fatalf("应说明跳过的原因与替代口径：%s", reason)
	}

	// 远程但地址是真实主机 → 平台侧探测有意义，照常执行。
	remoteHost := model.MiddlewareInstance{
		Name: "jd-redis", Host: "203.195.191.75", Port: 6379,
		Config: model.JSONMap{"integration": map[string]any{
			"template": integration.TypeRedis, "deploy_target": "remote",
		}},
	}
	if _, skip := platformSideProbeSkipReason(remoteHost); skip {
		t.Fatal("远程 + 真实 IP 不应跳过（仍可探测平台到目标的连通性）")
	}

	// 本机部署：地址是容器名/平台视角，照常探测。
	local := model.MiddlewareInstance{
		Name: "app-redis", Host: "127.0.0.1", Port: 6379,
		Config: model.JSONMap{"integration": map[string]any{
			"template": integration.TypeRedis, "deploy_target": "local",
		}},
	}
	if _, skip := platformSideProbeSkipReason(local); skip {
		t.Fatal("本机部署不应跳过（地址本就是平台视角）")
	}

	// 非集成实例（手工纳管）：没有 integration 元信息 → 照常探测。
	plain := model.MiddlewareInstance{Name: "legacy", Host: "127.0.0.1", Port: 6379}
	if _, skip := platformSideProbeSkipReason(plain); skip {
		t.Fatal("非集成实例不应跳过")
	}
}
