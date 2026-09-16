package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// 本文件把 Prometheus 客户端的底层错误翻译成「可执行的结论」。
//
// 背景（真实故障）：平台查询失败时只把 Go 的原始错误抛给使用者，例如
//
//	Get "http://jd-prometheus:9090/api/v1/query?...": dial tcp:
//	lookup jd-prometheus on 127.0.0.11:53: server misbehaving
//
// 这句话里 127.0.0.11 是 **Docker 内置 DNS**，"server misbehaving" 实际含义是
// 「该容器所属的网络上没有这个名字」——也就是容器网络挂错了，而使用者很难看出来。
// 跨栈接入时这类错误占了绝大多数，因此统一在这里映射成中文结论 + 修复动作。

// DescribeError 把 Prometheus 查询错误翻译为「结论 + 修复方向」。
//
// baseURL 用于在文案里指出"平台正在查哪个地址"，便于和 .env 对照。
func DescribeError(err error, baseURL string) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	lower := strings.ToLower(raw)
	host := hostOfURL(baseURL)
	endpoint := baseURL
	if endpoint == "" {
		endpoint = "(未配置)"
	}

	switch {
	// Docker 内置 DNS 解析失败：容器不在目标网络上是典型原因。
	// 三种表现：server misbehaving（Docker DNS 的返回）、no such host（libc/Go 解析器）、
	// temporary failure in name resolution。
	case strings.Contains(lower, "server misbehaving"),
		strings.Contains(lower, "no such host"),
		strings.Contains(lower, "name resolution"),
		strings.Contains(lower, "lookup "):
		return fmt.Sprintf("平台容器内解析不了 %s（Docker 内置 DNS 报 server misbehaving）："+
			"说明平台没有连接到该名字所在的容器网络。"+
			"跨栈（jd）场景请确认平台是带 deploy/compose.jd-link.yml 启动的——mwops-backend 必须在 jd-nightjar 网络上；"+
			"可执行 scripts/setup-jd-link.sh 一键修复，或 scripts/doctor-jd-link.sh 体检。当前查询地址：%s",
			displayHost(host), endpoint)

	case strings.Contains(lower, "connection refused"):
		return fmt.Sprintf("目标端口拒绝连接：%s 上没有进程在监听（Prometheus 容器没起来、端口写错，或宿主端口与容器端口混淆）。当前查询地址：%s",
			displayHost(host), endpoint)

	case strings.Contains(lower, "i/o timeout"),
		strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "timeout"):
		return fmt.Sprintf("连接 %s 超时：网络可达性有问题（跨主机未放通端口、防火墙，或网络隔离导致不可路由）。当前查询地址：%s",
			displayHost(host), endpoint)

	case strings.Contains(lower, "certificate"), strings.Contains(lower, "x509"):
		return fmt.Sprintf("TLS 校验失败：%s 使用自签证书时需要在平台侧信任该证书或改用 http。当前查询地址：%s",
			displayHost(host), endpoint)

	case strings.Contains(lower, "状态码 401"), strings.Contains(lower, "状态码 403"):
		return fmt.Sprintf("Prometheus 拒绝访问（401/403）：该实例开启了鉴权，平台当前未带凭据。当前查询地址：%s", endpoint)

	default:
		return fmt.Sprintf("Prometheus 查询失败：%s（查询地址：%s）", raw, endpoint)
	}
}

// hostOfURL 从 base_url 里取出主机名（解析失败返回空串）。
func hostOfURL(baseURL string) string {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return ""
	}
	if !strings.Contains(trimmed, "://") {
		trimmed = "//" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// displayHost 空主机名时给出占位符。
func displayHost(host string) string {
	if strings.TrimSpace(host) == "" {
		return "目标主机"
	}
	return host
}

// HostOf 返回 base_url 的主机名（自检用于做 DNS/网络判定）。
func HostOf(baseURL string) string { return hostOfURL(baseURL) }

// LookupHostCtx 在给定 context 下解析主机名（自检用）。
//
// 单列一个函数是为了让"是不是容器网络问题"这件事可以被确定地回答：
// 解析失败 → 网络/别名问题；解析成功但连不上 → Prometheus 自身或端口问题。
// 已经是 IP 时不解析，直接视为可用。
func LookupHostCtx(ctx context.Context, host string) error {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return errors.New("主机名为空")
	}
	if net.ParseIP(trimmed) != nil {
		return nil
	}
	_, err := net.DefaultResolver.LookupHost(ctx, trimmed)
	return err
}
