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
		optLogMultiline, optLogPattern, optLogInstallMode, optLogBeatVersion,
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
	if input.InstallMode != "auto" {
		t.Fatalf("安装方式留空应默认 auto（已存在则不重复部署），实际 %q", input.InstallMode)
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
}
