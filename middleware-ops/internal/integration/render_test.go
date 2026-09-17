package integration

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件锁定「集成中心」的渲染契约。
//
// 渲染产物是使用者直接复制粘贴到 Prometheus / compose 里的东西，错一个字符就是
// 「集成保存成功但永远没有指标」，因此逐条固化：
//   - 集成名称规范（与云厂商控制台一致，同时要能当标签值与容器名）；
//   - 地址解析（host:port / 裸 host / 带 scheme 与 path 的 URL）；
//   - Exporter 的 env 与命令行开关（redis 用环境变量、mysql 用 --collect.*）；
//   - file_sd 的 instance_name（平台定位实例的唯一依据）；
//   - 生成的配置里绝不出现明文口令。

func TestValidateNameFollowsConsoleConvention(t *testing.T) {
	valid := []string{"legacy-redis", "redis", "a1", "mysql.prod-01", "redis-dev-01"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Fatalf("名称 %q 应合法，实际被拒：%v", name, err)
		}
	}
	invalid := []string{"", "Prod-Redis", "prod_redis", "-redis", "redis-", "redis..prod", "redis prod"}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Fatalf("名称 %q 应被拒绝", name)
		}
	}
}

func TestParseAddressForms(t *testing.T) {
	cases := []struct {
		raw     string
		defPort int
		host    string
		port    int
		scheme  string
		path    string
	}{
		{"10.0.0.11:6379", 6379, "10.0.0.11", 6379, "", ""},
		{"redis.internal", 6379, "redis.internal", 6379, "", ""},
		{"redis://legacy-redis:6380", 6379, "legacy-redis", 6380, "redis", ""},
		{"http://10.0.0.15:9200/", 9200, "10.0.0.15", 9200, "http", ""},
		{"10.0.0.16:80", 80, "10.0.0.16", 80, "", ""},
	}
	for _, tc := range cases {
		address, err := ParseAddress(tc.raw, tc.defPort, "", "/stub_status")
		if err != nil {
			t.Fatalf("地址 %q 解析失败：%v", tc.raw, err)
		}
		if address.Host != tc.host || address.Port != tc.port {
			t.Fatalf("地址 %q 解析为 %s:%d，期望 %s:%d", tc.raw, address.Host, address.Port, tc.host, tc.port)
		}
		if tc.scheme != "" && address.Scheme != tc.scheme {
			t.Fatalf("地址 %q 的 scheme 应为 %s，实际 %s", tc.raw, tc.scheme, address.Scheme)
		}
	}
	// 未指定 path 时回落模板默认值（nginx 的 stub_status）。
	address, err := ParseAddress("http://10.0.0.16", 80, "http", "/stub_status")
	if err != nil {
		t.Fatalf("nginx 地址解析失败：%v", err)
	}
	if address.Path != "/stub_status" {
		t.Fatalf("nginx 未指定 path 时应回落 /stub_status，实际 %q", address.Path)
	}
	if err := func() error { _, err := ParseAddress("", 6379, "", ""); return err }(); err == nil {
		t.Fatal("空地址应被拒绝")
	}
}

// TestRenderRedisIntegration 锁定 Redis 集成产物的关键契约。
func TestRenderRedisIntegration(t *testing.T) {
	tpl, ok := TemplateOf(TypeRedis)
	if !ok {
		t.Fatal("redis 模板缺失")
	}
	address, err := ParseAddress("redis://legacy-redis:6379", tpl.DefaultPort, "", "")
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	instance := Instance{
		Name: "legacy-redis", MWType: TypeRedis, Address: address,
		Username: "monitor", Password: "p@ss w0rd", // 故意带 @ 与空格，验证脱敏与转义
		Labels:      map[string]string{"team": "interview"},
		Options:     map[string]string{"REDIS_EXPORTER_EXCLUDE_SLOWLOG_METRICS": "true"},
		Environment: "dev", GroupName: "interview",
	}
	artifacts, err := Render(tpl, instance, "middleware-integration", "/etc/prometheus/sd/integrations.json", "mwops,target_default")
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	// 选择器必须与平台的 buildSelector 约定一致（job + instance_name）。
	if artifacts.Selector != `job="middleware-integration",instance_name="legacy-redis"` {
		t.Fatalf("选择器不符：%s", artifacts.Selector)
	}

	// file_sd：instance_name 是平台定位实例的唯一依据。
	var entries []FileSDEntry
	if err := json.Unmarshal([]byte(artifacts.FileSD), &entries); err != nil {
		t.Fatalf("file_sd 不是合法 JSON：%v\n%s", err, artifacts.FileSD)
	}
	if len(entries) != 1 || entries[0].Labels["instance_name"] != "legacy-redis" {
		t.Fatalf("file_sd 缺少 instance_name：%s", artifacts.FileSD)
	}
	if entries[0].Targets[0] != "legacy-redis:6379" {
		t.Fatalf("file_sd 目标不符：%v", entries[0].Targets)
	}
	if entries[0].Labels["team"] != "interview" || entries[0].Labels["mw_type"] != "redis" {
		t.Fatalf("file_sd 自定义标签/类型标签缺失：%v", entries[0].Labels)
	}

	// 生成物里不得出现明文口令（会被复制到工单、聊天与仓库）。
	if strings.Contains(artifacts.Compose, "p@ss w0rd") || strings.Contains(artifacts.DeployCmd, "p@ss w0rd") {
		t.Fatal("生成的配置中出现了明文口令")
	}
	if !strings.Contains(artifacts.Compose, "${MONITOR_PASSWORD}") {
		t.Fatal("生成的口令应替换为变量占位")
	}

	// Redis 的参数走环境变量，且 REDIS_ADDR 使用 redis:// 前缀。
	if !strings.Contains(artifacts.Compose, "REDIS_ADDR: redis://legacy-redis:6379") {
		t.Fatalf("compose 片段缺少 REDIS_ADDR：\n%s", artifacts.Compose)
	}
	if !strings.Contains(artifacts.Compose, "REDIS_EXPORTER_EXCLUDE_SLOWLOG_METRICS: true") {
		t.Fatalf("compose 片段缺少集群架构的排除开关：\n%s", artifacts.Compose)
	}

	// 多网络：监控面 + 目标所在网络（后者由平台自动发现）都要列出。
	if strings.Count(artifacts.Compose, "      - ") < 2 || !strings.Contains(artifacts.Compose, "- target_default") {
		t.Fatalf("compose 片段应列出两个网络：\n%s", artifacts.Compose)
	}
	if !strings.Contains(artifacts.DeployCmd, "--network mwops") || !strings.Contains(artifacts.DeployCmd, "--network target_default") {
		t.Fatalf("docker run 命令应包含两个网络：%s", artifacts.DeployCmd)
	}

	// 显式 job 片段（file_sd 的替代方案）也要带 instance_name。
	if !strings.Contains(artifacts.ScrapeJob, "instance_name: legacy-redis") {
		t.Fatalf("显式 job 片段缺少 instance_name：\n%s", artifacts.ScrapeJob)
	}
}

// TestRenderMySQLIntegrationUsesCollectFlags 锁定 MySQL 的参数落地形态：
// 文档里的「Exporter 配置」在本平台是命令行开关，而不是环境变量。
func TestRenderMySQLIntegrationUsesCollectFlags(t *testing.T) {
	tpl, _ := TemplateOf(TypeMySQL)
	address, err := ParseAddress("10.0.0.12:3306", tpl.DefaultPort, "", "")
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	instance := Instance{
		Name: "legacy-mysql", MWType: TypeMySQL, Address: address,
		Username: "exporter", Password: "secret",
		Options: map[string]string{
			"collect.global_status":          "true",
			"collect.info_schema.tables":     "false",
			"collect.auto_increment.columns": "true",
		},
		Environment: "dev",
	}
	artifacts, err := Render(tpl, instance, "middleware-integration", "/etc/prometheus/sd/integrations.json", "mwops")
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if !strings.Contains(artifacts.Compose, "--collect.global_status") {
		t.Fatalf("应渲染 --collect.global_status：\n%s", artifacts.Compose)
	}
	// 上游默认开启的采集项被显式关闭时，必须输出 --no-xxx，
	// 否则 mysqld_exporter 仍按默认值继续采集（"关了没关掉"）。
	if !strings.Contains(artifacts.Compose, "--no-collect.info_schema.tables") {
		t.Fatalf("显式关闭默认开启的采集项时应输出 --no- 前缀：\n%s", artifacts.Compose)
	}
	// 凭据必须走官方 flag + 环境变量，**绝不能拼 DATA_SOURCE_NAME**：
	// 拼串方式会让口令里的 @ ( ) / : ? 破坏 DSN 解析（表现为 Exporter 启动失败、up=0，
	// 且 Prometheus 侧只看到一句泛泛的抓取失败）。
	if strings.Contains(artifacts.Compose, "DATA_SOURCE_NAME") {
		t.Fatalf("MySQL 集成不应再使用 DATA_SOURCE_NAME（口令特殊字符会破坏 DSN）：\n%s", artifacts.Compose)
	}
	if !strings.Contains(artifacts.Compose, "--mysqld.address=10.0.0.12:3306") {
		t.Fatalf("应渲染 --mysqld.address：\n%s", artifacts.Compose)
	}
	if !strings.Contains(artifacts.Compose, "--mysqld.username=exporter") {
		t.Fatalf("应渲染 --mysqld.username：\n%s", artifacts.Compose)
	}
	if !strings.Contains(artifacts.Compose, "MYSQLD_EXPORTER_PASSWORD") {
		t.Fatalf("口令应通过 MYSQLD_EXPORTER_PASSWORD 注入：\n%s", artifacts.Compose)
	}
	if strings.Contains(artifacts.Compose, "secret") {
		t.Fatalf("生成的配置中出现了明文口令：\n%s", artifacts.Compose)
	}
}

// TestRenderRejectsReservedLabelsAndUnknownOptions 锁定白名单：
// 不允许用自定义标签覆盖平台标签，也不允许透传模板未声明的 Exporter 参数。
func TestRenderRejectsReservedLabelsAndUnknownOptions(t *testing.T) {
	tpl, _ := TemplateOf(TypeRedis)
	address, err := ParseAddress("10.0.0.11:6379", 6379, "", "")
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	base := Instance{Name: "legacy-redis", MWType: TypeRedis, Address: address, Username: "monitor"}

	reserved := base
	reserved.Labels = map[string]string{"instance_name": "hijack"}
	if _, err := Render(tpl, reserved, "", "", ""); err == nil {
		t.Fatal("自定义标签覆盖 instance_name 应被拒绝")
	}

	unknown := base
	unknown.Options = map[string]string{"REDIS_EXPORTER_EVIL_FLAG": "1"}
	if _, err := Render(tpl, unknown, "", "", ""); err == nil {
		t.Fatal("模板未声明的 Exporter 参数应被拒绝")
	}

	// 无认证实例（账号与口令都空）必须能渲染通过：
	// 内网 Redis 未开 requirepass 是合法场景，旧逻辑在这里拒绝保存，
	// 使用者既填不出账号也填不出口令，被彻底卡住（真实反馈）。
	anonymous := base
	anonymous.Username = ""
	if _, err := Render(tpl, anonymous, "", "", ""); err != nil {
		t.Fatalf("无认证 Redis 应允许渲染（账号与口令都可空），实际：%v", err)
	}

	// 但"平台代建只读账号"的组件仍必须给出账号名，否则建号 SQL 无从下手。
	mysqlTpl, _ := TemplateOf(TypeMySQL)
	mysqlAddress, _ := ParseAddress("10.0.0.12:3306", 3306, "", "")
	if _, err := Render(mysqlTpl, Instance{
		Name: "mysql-01", MWType: TypeMySQL, Address: mysqlAddress, Environment: "dev",
	}, "", "", ""); err == nil {
		t.Fatal("MySQL 未填监控账号名时应被拒绝")
	}
}

// TestRenderFileSDIsStableAndMultiInstance 锁定 file_sd 的多目标与稳定输出。
func TestRenderFileSDIsStableAndMultiInstance(t *testing.T) {
	first := FileSDEntry{Targets: []string{"a:6379"}, Labels: map[string]string{"instance_name": "a", "mw_type": "redis"}}
	second := FileSDEntry{Targets: []string{"b:6379"}, Labels: map[string]string{"instance_name": "b", "mw_type": "redis"}}
	docA, err := RenderFileSD([]FileSDEntry{first, second})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	docB, err := RenderFileSD([]FileSDEntry{second, first})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if docA != docB {
		t.Fatalf("file_sd 输出应与输入顺序无关（顺序变化会让 Prometheus 侧产生无意义 diff）:\n%s\n---\n%s", docA, docB)
	}
	var entries []FileSDEntry
	if err := json.Unmarshal([]byte(docA), &entries); err != nil {
		t.Fatalf("file_sd 不是合法 JSON：%v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("应输出 2 条目标，实际 %d", len(entries))
	}
	empty, err := RenderFileSD(nil)
	if err != nil {
		t.Fatalf("空集合渲染失败：%v", err)
	}
	if strings.TrimSpace(empty) != "[]" {
		t.Fatalf("空集合应渲染为 []，实际 %q", empty)
	}
}

// TestContainerNameIsDockerSafe 锁定容器名（集成名称里的点不能留在容器名里）。
func TestContainerNameIsDockerSafe(t *testing.T) {
	if got := ContainerName("mysql.prod-01"); got != "mwops-exporter-mysql-prod-01" {
		t.Fatalf("容器名不符：%s", got)
	}
}
