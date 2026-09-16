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
			"说明平台当前不在该名字所在的容器网络上。"+
			"集成中心创建 Exporter 时会**自动**把容器接进目标容器所在网络，"+
			"因此先确认：① 该名字与 `docker ps` 里的容器名（或 compose 服务名）一致；"+
			"② 平台已挂载 docker.sock 且 integration.docker_enabled=true；"+
			"③ 目标容器在运行。当前查询地址：%s",
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

// TargetStatus 是 Prometheus 中某个抓取目标的当前状态。
type TargetStatus struct {
	Job       string `json:"job"`
	Instance  string `json:"instance"`
	Health    string `json:"health"`
	LastError string `json:"last_error"`
	// LastScrape / ScrapeURL 用于定位"抓的是哪个地址、多久没成功过"。
	LastScrape string            `json:"last_scrape"`
	ScrapeURL  string            `json:"scrape_url"`
	Labels     map[string]string `json:"labels,omitempty"`
}

// DescribeTargetError 把 Prometheus 的 lastError 翻译成「原因 + 动作」。
//
// up=0 时最有用的信息就是这行 lastError（例如
// "Error opening connection to database: Access denied for user 'exporter'@..."），
// 但很多人不知道去 Prometheus 的 /targets 页面看，于是只看到一句"抓取失败"。
// 这里把它取回来并给出对应的处理动作。
func DescribeTargetError(lastError string) string {
	text := strings.TrimSpace(lastError)
	if text == "" {
		return "Prometheus 未记录具体错误：通常是 Exporter 容器没起来（端口无人监听），请查看容器状态与日志。"
	}
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "access denied"):
		return "原因：监控账号或口令不一致，或该账号缺少 PROCESS / REPLICATION CLIENT 权限。" +
			"正解：在「集成中心」重新保存该集成并勾选「由平台创建只读监控账号」（填一次管理凭据），" +
			"平台会幂等建号授权；也可按 docs/INTEGRATION.md 的模板 SQL 手工创建。"

	case strings.Contains(lower, "invalid dsn"), strings.Contains(lower, "error parsing"),
		strings.Contains(lower, "malformed"), strings.Contains(lower, "unescaped"):
		return "原因：Exporter 的 DSN 解析失败，通常是口令含 @ ( ) / : ? 等特殊字符，而旧配置在做 DSN 拼接。" +
			"重建该 Exporter 容器即可：本平台用官方参数方式传凭据（--mysqld.username 与 MYSQLD_EXPORTER_PASSWORD），不做 DSN 转义。"

	case strings.Contains(lower, "connection refused"), strings.Contains(lower, "can't connect"),
		strings.Contains(lower, "no route to host"):
		return "原因：Exporter 连不上被管实例（对被管中间件而言是 connection refused）。" +
			"确认 MySQL/Redis 容器已 healthy，且 Exporter 与其在同一容器网络。"

	case strings.Contains(lower, "no such host"), strings.Contains(lower, "server misbehaving"),
		strings.Contains(lower, "name resolution"):
		return "原因：容器内解析不了被管实例的主机名——网络/别名问题。" +
			"确认 Exporter 与目标在同一 docker 网络（jd 场景：jd_jd-data）。"

	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"):
		return "原因：抓取超时。实例负载过高、网络隔离，或 scrape_timeout 小于实际采集耗时（可调大 scrape_timeout）。"

	case strings.Contains(lower, "unknown database"), strings.Contains(lower, "access denied for user"):
		return "原因：库名或账号权限不对。核对 DSN 里的库名，并确认账号具备所需的只读权限。"

	case strings.Contains(lower, "x509"), strings.Contains(lower, "certificate"):
		return "原因：TLS 证书校验失败（自签证书场景）。在 Exporter 侧信任该 CA 或改用非 TLS 连接。"

	default:
		return "原因见上面的 lastError 原文；对照 docs/GUIDE-JD-ONBOARD.md 的「up=0 排查」表逐条排除。"
	}
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
