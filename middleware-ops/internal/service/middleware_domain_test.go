package service

import (
	"testing"

	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
)

// 本文件锁定「日志集成不属于中间件纳管与监控域」这条边界。
//
// 真实反馈：日志集成建好之后，它同时出现在「中间件纳管」列表和「统一监控」的实例下拉里
// （两处查的是同一个 /api/middlewares），大盘还把 it 算成一种实例类型。
// 根因是这一域的几个入口各自查 middleware_instances，谁都没排除 mw_type=log。
//
// 因此把判定收口成 MiddlewareDomainTypes()，并用这两条测试钉住：
//  1. 该域必须包含全部中间件类型与主机监控（有指标画像的才能在这个域里）；
//  2. 没有指标画像的类型（当前就是 log）绝不允许出现在该域里——
//     这条规则是**通用**的，将来新增"没有指标的一类集成"时不需要再回来改过滤。

// TestMiddlewareDomainTypesExcludeLogIntegration 校验白名单的内容。
func TestMiddlewareDomainTypesExcludeLogIntegration(t *testing.T) {
	set := map[string]bool{}
	for _, mwType := range MiddlewareDomainTypes() {
		set[mwType] = true
	}
	if len(set) == 0 {
		t.Fatal("纳管域白名单不能为空（否则中间件列表会一条都查不出来）")
	}

	// 必须在域内：手工纳管的中间件类型（含二期 rabbitmq）+ 有画像的主机监控。
	mustInclude := []string{
		model.MWTypeRedis, model.MWTypeKafka, model.MWTypeMySQL, model.MWTypePG,
		model.MWTypeES, model.MWTypeNginx, model.MWTypeRMQ, model.MWTypeNode,
	}
	for _, mwType := range mustInclude {
		if !set[mwType] {
			t.Fatalf("纳管域应包含 %s（它有指标画像，属于监控对象）；实际：%v", mwType, MiddlewareDomainTypes())
		}
	}

	// 必须在域外：日志集成——它没有指标画像，也没有实例端口。
	if set[model.MWTypeLog] {
		t.Fatalf("日志集成（%s）绝不能出现在中间件纳管域里（否则它会混进纳管列表、统一监控下拉、"+
			"告警规则的实例选择与大盘统计，还会被健康探测标成离线）", model.MWTypeLog)
	}
}

// TestMiddlewareDomainOnlyContainsProfiledTypes 校验通用规则：
// 凡是**没有指标画像**的集成类型，都不属于纳管域。
func TestMiddlewareDomainOnlyContainsProfiledTypes(t *testing.T) {
	set := map[string]bool{}
	for _, mwType := range MiddlewareDomainTypes() {
		set[mwType] = true
	}
	checked := 0
	for _, tpl := range integration.Templates() {
		if len(monitor.ProfileOf(tpl.Type).Metrics) > 0 {
			continue
		}
		checked++
		if set[tpl.Type] {
			t.Fatalf("集成类型 %s 没有指标画像，却出现在纳管域白名单里（%v）："+
				"它没有指标可选、探测必然失败，混进监控域只会制造噪音",
				tpl.Type, MiddlewareDomainTypes())
		}
	}
	if checked == 0 {
		t.Fatal("当前应当存在至少一个无指标画像的集成类型（日志集成），测试前置条件失效")
	}
}

// TestLogTypeConstantsAgree 防止两处类型常量漂移。
//
// model.MWTypeLog 用于数据库域过滤，integration.TypeLog 用于模板注册；
// 两者一旦不一致，域过滤就会失效（过滤 log 而实际写入的是另一个字符串）。
func TestLogTypeConstantsAgree(t *testing.T) {
	tpl, ok := integration.TemplateOf(model.MWTypeLog)
	if !ok {
		t.Fatalf("集成模板注册表里找不到类型 %q（model.MWTypeLog 与 integration.TypeLog 漂移了）", model.MWTypeLog)
	}
	if tpl.Type != model.MWTypeLog {
		t.Fatalf("模板类型 %q 与 model.MWTypeLog=%q 不一致", tpl.Type, model.MWTypeLog)
	}
	if tpl.CategoryOf() != integration.CategoryLog {
		t.Fatalf("日志集成的 category 应为 %s，实际 %s", integration.CategoryLog, tpl.CategoryOf())
	}
}
