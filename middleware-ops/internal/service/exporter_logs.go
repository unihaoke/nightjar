package service

import (
	"regexp"
	"strings"
)

// 本文件把「Exporter 容器日志」翻译成可执行的结论。
//
// 为什么需要：Exporter 起来了、Prometheus 也 up=1，但指标是 0 —— 真正的原因
// （名字解析不了、端口拒绝、口令不对、网络不通）只写在 Exporter 容器的日志里。
// 让使用者自己去 docker logs 里翻是反人性的，平台既然创建了这个容器，
// 就应该顺手把日志读回来给结论。

// lookupPattern 提取 "lookup <host> on 127.0.0.11:53: no such host" 里的主机名。
var lookupPattern = regexp.MustCompile(`lookup ([^\s:]+)`)

// addrPattern 提取 "dial tcp 10.0.0.1:6379" 这类地址。
var addrPattern = regexp.MustCompile(`(?:dial tcp|connect to|connection to) ([0-9a-zA-Z._-]+:\d+)`)

// DescribeExporterLog 扫描 Exporter 日志，命中已知失败模式时返回「原因 + 动作」；
// 无异常（或日志里没有可识别的问题）返回空串。
//
// 纯函数：不碰网络、不读时钟，便于单测锁定措辞。
func DescribeExporterLog(logs string) string {
	if strings.TrimSpace(logs) == "" {
		return ""
	}
	lower := strings.ToLower(logs)

	switch {
	case strings.Contains(lower, "no such host"):
		host := ""
		if match := lookupPattern.FindStringSubmatch(logs); len(match) > 1 {
			host = match[1]
		}
		target := "目标地址"
		if host != "" {
			target = "「" + host + "」"
		}
		return "Exporter 解析不了" + target + "（no such host）：这个名字在 docker 里不存在或没接到同一张网。" +
			"正确的地址是 `docker ps` 里 NAMES 列的名字（如 app-redis:6379）；" +
			"改完地址后点「重新应用」——平台会重新发现并接入目标网络。"

	case strings.Contains(lower, "noauth") || strings.Contains(lower, "wrongpass") ||
		strings.Contains(lower, "invalid username-password"):
		return "Exporter 连上了目标但**认证失败**：口令不一致。" +
			"核对集成里的口令与被管实例的实际口令（如 被管项目的 REDIS_PASSWORD），保存后点「重新应用」。"

	case strings.Contains(lower, "access denied for user"):
		return "Exporter 连上了目标但数据库拒绝了该账号（Access denied）。" +
			"在「监控账号」点「重试建号」由平台幂等建号授权，或核对账号名与口令。"

	case strings.Contains(lower, "connection refused"):
		addr := ""
		if match := addrPattern.FindStringSubmatch(logs); len(match) > 1 {
			addr = match[1]
		}
		suffix := ""
		if addr != "" {
			suffix = "（" + addr + "）"
		}
		return "Exporter 连不上目标端口" + suffix + "：端口写错，或目标没在监听。" +
			"确认地址形如 app-redis:6379，并在被管项目里确认该服务已启动。"

	case strings.Contains(lower, "i/o timeout"), strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "connection timed out"):
		return "Exporter 连接目标超时：**网络不通**。" +
			"点「重新应用」让平台重新接入目标容器所在网络；若目标在另一台主机，请确认端口放通。"

	case strings.Contains(lower, "certificate"), strings.Contains(lower, "x509"):
		return "Exporter 遇到 TLS 证书问题：自签证书需要在 Exporter 侧信任，或改用非 TLS 端口。"

	case strings.Contains(lower, "permission denied"):
		return "Exporter 报权限不足：核对只读账号的授权（MySQL 需要 PROCESS / REPLICATION CLIENT / SELECT）。"
	}
	return ""
}
