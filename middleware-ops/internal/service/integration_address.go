package service

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
)

// 本文件处理「集成地址的两种视角」。
//
// 同一个「地址」字段，对不同角色含义不同：
//   - **平台视角**：平台自己（后端容器）要能连到它——用于平台侧 TCP 健康探测；
//   - **目标机视角**：Exporter 进程要能连到它——远程部署时 Exporter 跑在目标机上。
//
// 两者在被管实例与 Exporter 同机时最容易错开：用户顺手填了平台能连的公网 IP，
// 而 Exporter 需要的是回环地址。真实故障：
//
//	redis_exporter 日志：Couldn't connect to redis instance (redis://203.195.191.75:6379)
//	redis_up 0（而该机上的 127.0.0.1:6379 明明是通的）
//
// 打自己的公网 IP 要走 hairpin NAT 并穿过安全组，云上常被拦；回环则永远可用。

// isLoopbackHost 判断是否回环地址（含 localhost）。
//
// 除常见写法外，用 net.ParseIP().IsLoopback() 兜底：127.0.0.0/8 整段与 IPv6 的 ::1
// 都是回环，"只有 127.0.0.1 才算"会漏掉 127.0.0.2 这类同样打不到对端的写法。
func isLoopbackHost(host string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(host))
	switch trimmed {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	}
	if ip := net.ParseIP(strings.Trim(trimmed, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// exporterSideAddress 返回渲染 Exporter 配置时应使用的地址（**目标机视角**）。
//
// 规则：地址主机与目标机是同一台机器时改用回环地址（回环永远可用，且不经公网/安全组）；
// 其余情况原样返回——被管实例在别的机器上时，用户填的地址就是 Exporter 该用的地址。
//
// 第二个返回值是给使用者看的说明（空串=未改写）。
func exporterSideAddress(addr integration.Address, targetHost string) (integration.Address, string) {
	if isLoopbackHost(addr.Host) || !sameMachine(addr.Host, targetHost) {
		return addr, ""
	}
	original := addr.Host
	addr.Host = "127.0.0.1"
	return addr, fmt.Sprintf("Exporter 侧地址自动改用 127.0.0.1（它与目标机同机；"+
		"原地址 %s 需要经公网 hairpin 与安全组，Exporter 打它常常连不上）", original)
}

// platformSideProbeSkipReason 判断平台侧 TCP 探测对该实例是否**没有意义**。
//
// 远程集成的地址是目标机视角：若填的是回环地址，平台的探测会打到平台自己的 localhost，
// 必然失败——那是个假警报。这类实例的健康应以 Exporter 抓取的指标为准
// （redis_up / mysql_up / pg_up 等，平台的告警规则与「接入自检」都基于它）。
func platformSideProbeSkipReason(item model.MiddlewareInstance) (string, bool) {
	meta, ok := IntegrationMetaOf(item)
	if !ok || normalizeDeployTarget(meta.DeployTarget) != DeployTargetRemote {
		return "", false
	}
	if !isLoopbackHost(item.Host) {
		return "", false
	}
	endpoint := net.JoinHostPort(item.Host, strconv.Itoa(item.Port))
	return fmt.Sprintf("远程集成的地址 %s 是**目标机本地回环**：平台侧探测没有意义（会打到平台自己），已跳过；"+
		"该实例健康以 Exporter 指标为准（如 redis_up / mysql_up / pg_up）", endpoint), true
}
