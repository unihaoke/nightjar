package integration

import (
	"strings"
	"testing"
)

// 锁定 redis_exporter 的地址写法（INC-012）。
//
// 真实故障：平台生成 `REDIS_ADDR=redis://127.0.0.1:6379`，Exporter 却报
//
//	redis_exporter_last_scrape_error{err="dial redis: unknown network redis"} 1
//	redis_up 0
//
// 即把 URL 的 scheme 当成了网络类型。官方 README 明确 `redis.example.com:6379`
// 这种不带 scheme 的 tcp 地址同样合法，因此改用无歧义写法。

func TestRedisAddrHasNoScheme(t *testing.T) {
	tpl, ok := TemplateOf(TypeRedis)
	if !ok {
		t.Fatal("Redis 模板应存在")
	}
	address, err := ParseAddress("127.0.0.1:6379", tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	env := tpl.RenderEnv(Instance{
		Name: "jd-redis", MWType: TypeRedis, Address: address,
		Password: "p", Environment: "dev",
	})
	if got := env["REDIS_ADDR"]; got != "127.0.0.1:6379" {
		t.Fatalf("REDIS_ADDR 应为不带 scheme 的 host:port，实际 %q", got)
	}

	// 显式带 db 路径时保留 URL 形态，避免丢掉语义。
	withPath, err := ParseAddress("redis://127.0.0.1:6379/2", tpl.DefaultPort, "redis", "")
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	pathEnv := tpl.RenderEnv(Instance{Name: "r", MWType: TypeRedis, Address: withPath})
	if got := pathEnv["REDIS_ADDR"]; !strings.Contains(got, "/2") {
		t.Fatalf("带 db 路径时应保留路径，实际 %q", got)
	}
}

// TestRedisExporterArgsDoNotDuplicateAddr 锁定：地址只有一个来源（env），不额外传 flag。
//
// 同时传 `--redis.addr`（官方说明 flag 优先于 env）会造成"改了一处不生效"的排查陷阱。
func TestRedisExporterArgsDoNotDuplicateAddr(t *testing.T) {
	tpl, _ := TemplateOf(TypeRedis)
	address, _ := ParseAddress("127.0.0.1:6379", tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	instance := Instance{Name: "jd-redis", MWType: TypeRedis, Address: address, Environment: "dev"}
	for _, arg := range tpl.RenderArgs(instance) {
		if strings.Contains(arg, "redis.addr") {
			t.Fatalf("地址不应再由 flag 重复指定（flag 会覆盖 env）：%q", arg)
		}
	}
	art, err := RenderRemoteInstall(tpl, instance, remoteTestOptions())
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if strings.Contains(art.Playbook, "--redis.addr") {
		t.Fatalf("产物里不应出现 --redis.addr：\n%s", art.Playbook)
	}
}
