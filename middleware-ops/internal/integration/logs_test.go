package integration

import (
	"strings"
	"testing"
)

// 本文件锁定「日志位置必须从 docker 配置反查、查不到就拒绝」这条硬约束。
//
// 为什么必须拒绝而不是猜：猜错的后果是采集容器起来了、但永远没有日志，
// 使用者只会看到"日志页空的"，比直接报错难排查得多。

func TestDiscoverLogSourcePrefersLogPathEnv(t *testing.T) {
	env := []string{"TZ=Asia/Shanghai", "LOG_PATH=/app/data/logs", "AI_PROVIDER=mock"}
	mounts := []MountInfo{
		{Type: "volume", Name: "app_backend-logs", Destination: "/app/data/logs"},
		{Type: "volume", Name: "app_mysql-data", Destination: "/var/lib/mysql"},
	}
	source, ok := DiscoverLogSource("app-backend", env, mounts, []string{"app_data"})
	if !ok {
		t.Fatal("环境变量指明了日志目录且被卷覆盖，应判定为可采集")
	}
	if source.MountKind != "volume" || source.MountSource != "app_backend-logs" {
		t.Fatalf("应挂载命名卷 app_backend-logs，实际 %s/%s", source.MountKind, source.MountSource)
	}
	if source.MountSpec != "app_backend-logs:/logs:ro" {
		t.Fatalf("挂载串不符：%s", source.MountSpec)
	}
	if source.Glob != "/logs/*.log" {
		t.Fatalf("采集通配不符：%s", source.Glob)
	}
	if len(source.Evidence) == 0 || !strings.Contains(source.Evidence[0], "LOG_PATH") {
		t.Fatalf("应给出判断依据（环境变量名），实际 %v", source.Evidence)
	}
}

func TestDiscoverLogSourceMatchesParentMount(t *testing.T) {
	// 日志写在挂载点的子目录里（挂 /app/data、日志在 /app/data/logs）
	env := []string{"LOG_PATH=/app/data/logs"}
	mounts := []MountInfo{{Type: "volume", Name: "app_backend-data", Destination: "/app/data"}}
	source, ok := DiscoverLogSource("app-backend", env, mounts, nil)
	if !ok {
		t.Fatal("父目录被挂载时也应能采集（整卷挂进去即可）")
	}
	if source.MountSource != "app_backend-data" {
		t.Fatalf("应挂载父卷，实际 %s", source.MountSource)
	}
	if source.Dir != "/app/data/logs" {
		t.Fatalf("目录应保留原值，实际 %s", source.Dir)
	}
}

func TestDiscoverLogSourceFallsBackToLogNamedMount(t *testing.T) {
	// 没有 LOG_PATH，但卷名/路径里有 log
	mounts := []MountInfo{
		{Type: "bind", Source: "/srv/app/logs", Destination: "/var/log/app"},
		{Type: "volume", Name: "app_redis-data", Destination: "/data"},
	}
	source, ok := DiscoverLogSource("app-backend", nil, mounts, nil)
	if !ok {
		t.Fatal("挂载路径含 log 时应判定为可采集")
	}
	if source.MountKind != "bind" || source.MountSource != "/srv/app/logs" {
		t.Fatalf("应识别为宿主目录挂载，实际 %s/%s", source.MountKind, source.MountSource)
	}
}

func TestDiscoverLogSourceRefusesWhenNothingFound(t *testing.T) {
	// 关键用例：既没有日志环境变量，也没有像日志的挂载 —— 必须拒绝。
	mounts := []MountInfo{
		{Type: "volume", Name: "app_mysql-data", Destination: "/var/lib/mysql"},
		{Type: "volume", Name: "app_redis-data", Destination: "/data"},
	}
	if _, ok := DiscoverLogSource("app-mysql", []string{"MYSQL_ROOT_PASSWORD=x"}, mounts, nil); ok {
		t.Fatal("读不到日志位置时必须拒绝配置，而不是猜一个路径")
	}
}

func TestDiscoverLogSourceRefusesWhenEnvDirNotMounted(t *testing.T) {
	// 环境变量指向的目录没有被任何卷覆盖：采集容器挂不到它 → 也必须拒绝。
	env := []string{"LOG_PATH=/app/data/logs"}
	mounts := []MountInfo{{Type: "volume", Name: "app_mysql-data", Destination: "/var/lib/mysql"}}
	source, ok := DiscoverLogSource("app-backend", env, mounts, nil)
	if ok {
		t.Fatalf("日志目录未被挂载时不应判定为可采集，实际 %+v", source)
	}
	if len(source.Evidence) == 0 || !strings.Contains(source.Evidence[0], "未被任何卷") {
		t.Fatalf("应说明拒绝原因，实际 %v", source.Evidence)
	}
}

func TestDiscoverLogSourcePicksMostLogLikeCandidate(t *testing.T) {
	mounts := []MountInfo{
		{Type: "volume", Name: "app_data", Destination: "/app/data"},         // 含 log? 否
		{Type: "volume", Name: "app_logs", Destination: "/app/logs"},         // 更像日志
		{Type: "bind", Source: "/srv/misc/my-catalog", Destination: "/cata"}, // "catalog" 含 log 但是误报
	}
	source, ok := DiscoverLogSource("app", nil, mounts, nil)
	if !ok {
		t.Fatal("存在日志候选时应可采集")
	}
	if source.MountSource != "app_logs" {
		t.Fatalf("应优先选最像日志目录的挂载，实际 %s（%s）", source.MountSource, source.Dir)
	}
}
