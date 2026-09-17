package service

import (
	"testing"

	"middleware-ops/internal/integration"
)

// 本文件锁定「Prometheus 抓取目标」的选择规则。
//
// 三种部署形态对目标的要求完全不同，写错就必然 up=0：
//   - 本机容器（普通中间件）：容器名:端口，Prometheus 与 Exporter 同网络；
//   - 本机宿主监控（node_exporter）：Exporter 共享宿主网络，容器名无意义，
//     必须用"主机地址:端口"；地址填回环时要换成 host.docker.internal
//     （Prometheus 在容器里，127.0.0.1 指向它自己）；
//   - 远程服务器：目标机 IP:端口。

func TestScrapeTargetForLocalContainer(t *testing.T) {
	svc := &IntegrationService{}
	got := svc.scrapeTarget(IntegrationMeta{
		Template: integration.TypeRedis, Container: "mwops-exporter-order-redis", ExporterPort: 9121,
		DeployTarget: DeployTargetLocal,
	}, integration.Address{Host: "app-redis", Port: 6379})
	if got != "mwops-exporter-order-redis:9121" {
		t.Fatalf("本机容器应抓容器名:端口，实际 %s", got)
	}
}

func TestScrapeTargetForHostMode(t *testing.T) {
	svc := &IntegrationService{}
	// 回环地址 → host.docker.internal（Prometheus 在容器内，127.0.0.1 是它自己）
	if got := svc.scrapeTarget(IntegrationMeta{
		Template: integration.TypeNode, ExporterPort: 9100, DeployTarget: DeployTargetLocal,
	}, integration.Address{Host: "127.0.0.1", Port: 9100}); got != "host.docker.internal:9100" {
		t.Fatalf("回环地址应改写为 host.docker.internal:9100，实际 %s", got)
	}
	if got := svc.scrapeTarget(IntegrationMeta{
		Template: integration.TypeNode, ExporterPort: 9100, DeployTarget: DeployTargetLocal,
	}, integration.Address{Host: "localhost", Port: 9100}); got != "host.docker.internal:9100" {
		t.Fatalf("localhost 应改写为 host.docker.internal:9100，实际 %s", got)
	}
	// 真实主机 IP 保持原样
	if got := svc.scrapeTarget(IntegrationMeta{
		Template: integration.TypeNode, ExporterPort: 9100, DeployTarget: DeployTargetLocal,
	}, integration.Address{Host: "10.0.0.31", Port: 9100}); got != "10.0.0.31:9100" {
		t.Fatalf("真实主机 IP 应原样使用，实际 %s", got)
	}
}

func TestScrapeTargetForRemote(t *testing.T) {
	svc := &IntegrationService{}
	got := svc.scrapeTarget(IntegrationMeta{
		Template: integration.TypeMySQL, DeployTarget: DeployTargetRemote,
		TargetHost: "10.0.0.41", ExporterHostPort: 9104,
	}, integration.Address{Host: "10.0.0.41", Port: 3306})
	if got != "10.0.0.41:9104" {
		t.Fatalf("远程应抓目标机 IP:Exporter 端口，实际 %s", got)
	}
	// 端口留空时回落到模板默认端口
	if got := svc.scrapeTarget(IntegrationMeta{
		Template: integration.TypeMySQL, DeployTarget: DeployTargetRemote,
		TargetHost: "10.0.0.41", ExporterPort: 9104,
	}, integration.Address{Host: "10.0.0.41", Port: 3306}); got != "10.0.0.41:9104" {
		t.Fatalf("远程端口应回落到模板默认值，实际 %s", got)
	}
	// IPv6 需要方括号
	if got := svc.scrapeTarget(IntegrationMeta{
		Template: integration.TypeMySQL, DeployTarget: DeployTargetRemote,
		TargetHost: "fd00::41", ExporterHostPort: 9104,
	}, integration.Address{Host: "fd00::41", Port: 3306}); got != "[fd00::41]:9104" {
		t.Fatalf("IPv6 应加方括号，实际 %s", got)
	}
}

func TestNormalizeDeployTargetAndInstallMode(t *testing.T) {
	if normalizeDeployTarget("") != DeployTargetLocal {
		t.Fatal("空值应视为 local（向后兼容）")
	}
	if normalizeDeployTarget("remote") != DeployTargetRemote {
		t.Fatal("remote 应被识别")
	}
	if normalizeDeployTarget("something-else") != DeployTargetLocal {
		t.Fatal("未知值应回落 local，避免误在远程执行")
	}
	if normalizeInstallModeForTest("systemd") != integration.InstallModeDockerSystemd {
		t.Fatal("历史值 systemd 应归一化为 docker-systemd")
	}
	if normalizeInstallModeForTest("binary") != integration.InstallModeBinary {
		t.Fatal("binary 应被识别")
	}
	if normalizeInstallModeForTest("") != integration.InstallModeDocker {
		t.Fatal("空值应回落 docker")
	}
}

// normalizeInstallModeForTest 便于在 service 包里断言 integration 包的归一化规则。
func normalizeInstallModeForTest(value string) string {
	return integration.NormalizeInstallMode(value)
}
