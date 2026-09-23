package service

import (
	"testing"

	"middleware-ops/internal/config"
)

// 本文件钉住「合规设置（出网白名单）」的平台托管行为。
//
// 为什么值得测：白名单是"能否把日志送去外部 AI"的唯一闸门，默认空 = 全禁。
// 它如果只能靠改 .env 再重启来维护，"放行一个新服务"就变成一次部署动作，
// 而放行/收回恰恰是接入过程中最频繁的操作。

// TestNormalizeWhitelist 去空白、去重、丢空项。
func TestNormalizeWhitelist(t *testing.T) {
	got := normalizeWhitelist([]string{"  order-service ", "order-service", "", "  ", "pay", "*"})
	want := []string{"order-service", "pay", "*"}
	if len(got) != len(want) {
		t.Fatalf("规范化结果异常：%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项期望 %q，实际 %q（完整：%v）", i, want[i], got[i], got)
		}
	}
	if got := normalizeWhitelist(nil); len(got) != 0 {
		t.Fatalf("nil 应得到空切片而不是 nil 之外的东西：%v", got)
	}
}

// TestApplySecurityPayloadWritesBackToConfig 保存后必须立刻改到内存配置上。
//
// 白名单不重建任何客户端，靠"判定时实时读 cfg"生效（见 CodeAnalysisService.outboundGate），
// 所以这一步写不回去，界面上就会显示"已保存"而实际仍按旧名单拦截。
func TestApplySecurityPayloadWritesBackToConfig(t *testing.T) {
	cfg := &config.Config{}
	cfg.Security.OutboundWhitelist = []string{"old-service"}
	svc := &SettingService{cfg: cfg}

	svc.applySecurityPayload(securitySettingsPayload{
		OutboundWhitelist: []string{" order-service ", "order-service", "", "pay"},
	})
	if len(cfg.Security.OutboundWhitelist) != 2 {
		t.Fatalf("白名单未写回内存配置：%v", cfg.Security.OutboundWhitelist)
	}
	if cfg.Security.OutboundWhitelist[0] != "order-service" || cfg.Security.OutboundWhitelist[1] != "pay" {
		t.Fatalf("写回内容未规范化：%v", cfg.Security.OutboundWhitelist)
	}
}

// TestSecurityPayloadFromConfigCopies 基线取自内存配置时必须拷贝，不能共享底层数组。
func TestSecurityPayloadFromConfigCopies(t *testing.T) {
	cfg := &config.Config{}
	cfg.Security.OutboundWhitelist = []string{"order-service"}
	p := securityPayloadFromConfig(cfg)
	p.OutboundWhitelist[0] = "tampered"
	if cfg.Security.OutboundWhitelist[0] != "order-service" {
		t.Fatal("基线切片与内存配置共享了底层数组（改 payload 会污染 cfg）")
	}
}
