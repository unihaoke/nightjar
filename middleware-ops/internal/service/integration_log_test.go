package service

import (
	"strings"
	"testing"

	"middleware-ops/internal/config"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
)

// 本文件锁定「日志集成」服务层与集成模板之间的**参数键契约**。
//
// 为什么值得单测：服务层是按字符串键去读表单参数的（`options["MWOPS_LOG_PATHS"]`），
// 模板里声明的键名也是字符串。两边任何一处改名都不会有编译错误——
// 后果是"表单填了但服务读不到"：日志路径为空 → 报"路径不能为空"，
// 或者更糟——安装方式/版本悄悄退回默认值，用户以为改生效了其实没有。
// 所以这里显式断言"服务层用到的每个键，模板里都必须声明"。

// TestLogOptionKeysAreDeclaredInTemplate 校验键名一一对应。
func TestLogOptionKeysAreDeclaredInTemplate(t *testing.T) {
	tpl, ok := integration.TemplateOf(integration.TypeLog)
	if !ok {
		t.Fatal("集成模板里必须有 log 类型（日志集成），否则前端无法新建日志集成")
	}
	if tpl.CategoryOf() != integration.CategoryLog {
		t.Fatalf("log 模板的 category 应为 %s，实际 %s", integration.CategoryLog, tpl.CategoryOf())
	}
	declared := map[string]bool{}
	for _, opt := range tpl.Options {
		declared[opt.Key] = true
	}
	used := []string{
		optLogPaths, optLogService, optLogEnvironment, optLogLevel,
		optLogMultiline, optLogPattern, optLogInstallMode, optLogBeatVersion, optLogOverwrite,
	}
	for _, key := range used {
		if !declared[key] {
			t.Fatalf("服务层读取的参数 %s 没有在 log 模板的 Options 里声明——"+
				"表单不会渲染这个字段，服务层永远读不到值", key)
		}
	}
}

// TestLogInputOfBuildsFromOptionsAndPlatformKafka 校验渲染输入的组装。
//
// 两条硬约定：
//  1. Kafka 地址与 topic **只来自平台配置**，表单里填什么都不影响（避免把日志推到没人消费的 topic）；
//  2. 安装方式留空时默认 auto（复用已装 → docker → 包安装），因为"已存在就不重复部署"是硬要求。
func TestLogInputOfBuildsFromOptionsAndPlatformKafka(t *testing.T) {
	cfg := &config.Config{}
	// Enabled 必须显式打开：Kafka.Active() = enabled && 有 broker，
	// 生产里由 defaults.go 默认置 true，测试里得自己给（否则会被判成"未启用日志总线"）。
	cfg.Kafka.Enabled = true
	cfg.Kafka.Brokers = []string{"kafka:29092"}
	cfg.Kafka.LogTopic = "mwops-logs"
	cfg.Kafka.ExternalHost = "10.0.0.5"
	cfg.Kafka.ExternalPort = 9092
	cfg.Kafka.FilebeatVersion = "8.16.0"
	svc := &IntegrationService{cfg: cfg}

	tpl, _ := integration.TemplateOf(integration.TypeLog)
	instance := integration.Instance{
		Name: "order-log", MWType: tpl.Type, Address: integration.Address{Host: "10.0.0.9"},
		Environment: model.EnvProd,
		Options: map[string]string{
			optLogPaths:     "/var/log/app/*.log\n/data/logs/**/*.log",
			optLogService:   "order-api",
			optLogLevel:     "WARN",
			optLogMultiline: "false",
		},
	}
	item := &model.MiddlewareInstance{Name: "order-log", MWType: tpl.Type, Environment: model.EnvProd}
	input, err := svc.logInputOf(item, tpl, instance, IntegrationMeta{
		Options: instance.Options, TargetHost: "10.0.0.9",
	})
	if err != nil {
		t.Fatalf("组装渲染输入失败：%v", err)
	}
	if input.Host != "10.0.0.9" || input.Name != "order-log" {
		t.Fatalf("目标机/名称不对：%+v", input)
	}
	if input.Service != "order-api" || input.Environment != model.EnvProd || input.Level != "WARN" {
		t.Fatalf("服务/环境/级别应从表单取值并带上环境兜底：%+v", input)
	}
	if len(input.Paths) != 2 {
		t.Fatalf("多行路径应拆成 2 条，实际 %v", input.Paths)
	}
	if input.Multiline {
		t.Fatal("表单显式填 false 时应关闭多行合并")
	}
	// 默认 package（而不是 auto）：auto 会在有 Docker 的目标机上走容器模式，
	// 而容器模式的活动部件最多（容器用户/数据目录属主/镜像约定的配置路径/
	// Docker 创建绑定源目录），真实环境里连着暴露了四轮问题（INC-024 / INC-026）。
	if input.InstallMode != "package" {
		t.Fatalf("安装方式留空应默认 package（活动部件最少的那条路径），实际 %q", input.InstallMode)
	}
	if input.FilebeatVersion != "8.16.0" {
		t.Fatalf("版本应回落到平台配置，实际 %q", input.FilebeatVersion)
	}
	if len(input.KafkaHosts) != 1 || input.KafkaHosts[0] != "10.0.0.5:9092" {
		t.Fatalf("Kafka 地址必须来自平台配置的对外地址，实际 %v", input.KafkaHosts)
	}
	if input.Topic != "mwops-logs" {
		t.Fatalf("topic 必须来自平台配置，实际 %q", input.Topic)
	}
}

// TestLogInputOfRejectsMissingPieces 校验缺件时的报错是"可操作"的。
func TestLogInputOfRejectsMissingPieces(t *testing.T) {
	tpl, _ := integration.TemplateOf(integration.TypeLog)
	instance := integration.Instance{
		Name: "order-log", MWType: tpl.Type, Address: integration.Address{Host: "10.0.0.9"},
	}
	item := &model.MiddlewareInstance{Name: "order-log", MWType: tpl.Type}

	// 没有日志路径：必须直接拒绝，而不是渲染出一个什么都不采的 Filebeat 配置。
	cfg := &config.Config{}
	cfg.Kafka.Enabled = true
	cfg.Kafka.Brokers = []string{"kafka:29092"}
	svc := &IntegrationService{cfg: cfg}
	if _, err := svc.logInputOf(item, tpl, instance, IntegrationMeta{}); err == nil {
		t.Fatal("缺少日志路径时必须报错")
	} else if !strings.Contains(err.Error(), "日志路径") {
		t.Fatalf("报错要指明缺什么，实际 %v", err)
	}

	// 平台没启用 Kafka：日志集成整体不可用，错误里要指向配置项。
	instance.Options = map[string]string{optLogPaths: "/var/log/app/*.log"}
	if _, err := (&IntegrationService{cfg: &config.Config{}}).logInputOf(item, tpl, instance, IntegrationMeta{}); err == nil {
		t.Fatal("平台未配置 Kafka 时必须报错")
	} else if !strings.Contains(err.Error(), "KAFKA_BROKERS") {
		t.Fatalf("报错要指明改哪个配置，实际 %v", err)
	}

	// INC-028：远程目标机 + 平台默认的回环 Kafka 地址 = 必然失败，必须在配置阶段就拒绝。
	// 放过去的话现象是"部署成功、日志页空白"，使用者要在两个系统之间来回猜。
	loopback := &config.Config{}
	loopback.Kafka.Enabled = true
	loopback.Kafka.Brokers = []string{"kafka:29092"}
	loopback.Kafka.ExternalHost = "127.0.0.1"
	loopback.Kafka.ExternalPort = 9092
	remote := integration.Instance{
		Name: "jd-logs", MWType: tpl.Type, Address: integration.Address{Host: "203.195.191.75"},
		Options: map[string]string{optLogPaths: "/var/log/app/*.log"},
	}
	remoteItem := &model.MiddlewareInstance{Name: "jd-logs", MWType: tpl.Type}
	if _, err := (&IntegrationService{cfg: loopback}).logInputOf(remoteItem, tpl, remote, IntegrationMeta{}); err == nil {
		t.Fatal("远程目标机 + 回环 Kafka 地址必须被拒绝（否则 Filebeat 会去连它自己）")
	} else if !strings.Contains(err.Error(), "KAFKA_ADVERTISED_HOST") {
		t.Fatalf("拒绝时必须给出改哪个配置，实际 %v", err)
	}
	// 同一份回环地址对**本机**目标是合法的（EXTERNAL 端口已发布到宿主），不能误拦。
	local := remote
	local.Address = integration.Address{Host: "127.0.0.1"}
	if _, err := (&IntegrationService{cfg: loopback}).logInputOf(remoteItem, tpl, local, IntegrationMeta{}); err != nil {
		t.Fatalf("本机目标用回环地址是合法的，不该被拦：%v", err)
	}
}

// TestLogInputOfOverwriteOption 锁定「覆盖 Filebeat」开关的解析与文案。
//
// 开关的语义（用户要求："如果选择则可以覆盖 Filebeat 重新获取 docker，否则如果没有才进行拉取"）：
//   - 表单没填 / 填 false → **不动**目标机上已有的 Filebeat（默认必须是"不动"，因为这是破坏性操作）；
//   - 填 true → Overwrite=true，产物走重新拉取 + 强制重装那条路。
//
// 同时锁定文案：预览步骤与部署备注都必须能读出"这次到底会不会覆盖安装"。
// 文案错位的代价是真实的——之前 deployAttemptLabel 就出现过"日志集成显示成创建只读账号"的问题。
func TestLogInputOfOverwriteOption(t *testing.T) {
	tpl, _ := integration.TemplateOf(integration.TypeLog)
	cfg := &config.Config{}
	cfg.Kafka.Enabled = true
	cfg.Kafka.Brokers = []string{"kafka:29092"}
	cfg.Kafka.LogTopic = "mwops-logs"
	cfg.Kafka.ExternalHost = "10.0.0.5"
	cfg.Kafka.ExternalPort = 9092
	svc := &IntegrationService{cfg: cfg}
	item := &model.MiddlewareInstance{Name: "order-log", MWType: tpl.Type, Environment: model.EnvProd}

	cases := []struct {
		raw  string
		want bool
	}{
		{"", false},      // 未填：默认关闭
		{"false", false}, // 显式关闭
		{"0", false},     // 兼容 0/1 写法
		{"no", false},    // 兼容 no/yes
		{"true", true},   // 打开
		{"1", true},      //
		{" yes ", true},  // 带空白的取值必须照样识别（表单/接口都可能带空白）
		{"TRUE", true},   // 大小写不敏感
	}
	for _, tc := range cases {
		instance := integration.Instance{
			Name: "order-log", MWType: tpl.Type, Address: integration.Address{Host: "10.0.0.9"},
			Environment: model.EnvProd,
			Options: map[string]string{
				optLogPaths:       "/var/log/app/*.log",
				optLogOverwrite:   tc.raw,
				optLogInstallMode: "package",
			},
		}
		input, err := svc.logInputOf(item, tpl, instance, IntegrationMeta{Options: instance.Options, TargetHost: "10.0.0.9"})
		if err != nil {
			t.Fatalf("覆盖=%q：组装渲染输入失败：%v", tc.raw, err)
		}
		if input.Overwrite != tc.want {
			t.Fatalf("覆盖=%q：Overwrite 应为 %v，实际 %v", tc.raw, tc.want, input.Overwrite)
		}
		// 文案必须与开关一致：勾选时说"覆盖安装"，未勾选时说"已存在则不重装"。
		label := filebeatInstallPlanLabel(input)
		if tc.want {
			if !strings.Contains(label, "覆盖安装") {
				t.Fatalf("覆盖=%q：文案应说明本次会覆盖安装，实际 %q", tc.raw, label)
			}
		} else if !strings.Contains(label, "不重新拉取") {
			t.Fatalf("覆盖=%q：文案应说明已存在则不重新拉取/不重装，实际 %q", tc.raw, label)
		}
	}
}

// TestKafkaAddressUsableForTarget 锁定"必然失败的组合要在执行前拒绝"（INC-028）。
//
// 真实故障：平台的 Kafka 对外地址是默认值 127.0.0.1，而日志集成的目标是远程服务器；
// 平台把 127.0.0.1:9092 渲染进了那台机器的 filebeat.yml —— 它只会连自己，
// 日志一条都到不了平台，而现象是"部署成功、日志页空白"。
func TestKafkaAddressUsableForTarget(t *testing.T) {
	brokers := []string{"kafka:29092"}
	cases := []struct {
		name       string
		external   string
		port       int
		target     string
		wantErr    bool
		wantSubstr string
	}{
		{"远程 + 回环地址 → 拒绝", "127.0.0.1", 9092, "203.195.191.75", true, "回环地址"},
		{"远程 + localhost → 拒绝", "localhost", 9092, "203.195.191.75", true, "回环地址"},
		{"远程 + ::1 → 拒绝", "::1", 9092, "10.0.0.9", true, "回环地址"},
		{"远程 + 127.0.0.2（同段回环）→ 拒绝", "127.0.0.2", 9092, "10.0.0.9", true, "回环地址"},
		{"远程 + 容器内服务名 → 拒绝", "kafka", 29092, "203.195.191.75", true, "容器网络内"},
		{"远程 + 平台公网地址 → 允许", "203.195.191.75", 9092, "10.0.0.9", false, ""},
		{"远程 + 平台域名 → 允许", "nightjar.example.com", 9092, "10.0.0.9", false, ""},
		// 本机目标：回环地址是**对的**（EXTERNAL 端口已发布到宿主），不能误拦。
		{"本机目标 + 回环地址 → 允许", "127.0.0.1", 9092, "127.0.0.1", false, ""},
		{"本机目标 + localhost → 允许", "localhost", 9092, "localhost", false, ""},
		{"未配置对外地址 → 拒绝", "", 9092, "10.0.0.9", true, "未配置"},
	}
	for _, c := range cases {
		err := kafkaAddressUsableForTarget(c.external, c.port, c.target, brokers)
		if c.wantErr && err == nil {
			t.Fatalf("%s：必须拒绝（%s → %s）", c.name, c.external, c.target)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("%s：不该拒绝，实际 %v", c.name, err)
		}
		if c.wantErr && !strings.Contains(err.Error(), c.wantSubstr) {
			t.Fatalf("%s：错误里应说明「%s」，实际 %v", c.name, c.wantSubstr, err)
		}
		// 错误必须给出可照抄的修法：只告诉他"不行"而不说改哪里，等于把问题丢回给使用者。
		if c.wantErr && !strings.Contains(err.Error(), "KAFKA_ADVERTISED_HOST") {
			t.Fatalf("%s：错误里应给出改哪个配置项，实际 %v", c.name, err)
		}
	}
}
