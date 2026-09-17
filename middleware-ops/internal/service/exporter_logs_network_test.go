package service

import (
	"strings"
	"testing"
)

// 锁定 redis_exporter「兜底路径掩盖真实原因」这一条诊断（INC-012）。
//
// 真实故障：日志里只有
//
//	redis_exporter_last_scrape_error{err="dial redis: unknown network redis"} 1
//
// 而真正的原因（连接被拒 / 超时 / 口令不对）被 redis_exporter 的 fallback 吞掉了：
// DialURL 失败后它按 "://" 拆开重试，于是把 scheme 当成了网络类型。
func TestDescribeExporterLogUnknownNetwork(t *testing.T) {
	// 用户实际看到的形态：没有 debug，日志里只有这一句（真实原因被吞）。
	logs := `time="2026-09-18T01:20:00Z" level=error msg="Couldn't connect to redis instance (redis://203.195.191.75:6379)"
# HELP redis_exporter_last_scrape_error The last scrape error status.
redis_exporter_last_scrape_error{err="dial redis: unknown network redis"} 1`

	got := DescribeExporterLog(logs)
	if got == "" {
		t.Fatal("出现 unknown network 时必须给出结论")
	}
	// 必须点明"这是马甲"，并给出拿到真实原因的办法。
	for _, want := range []string{"unknown network", "redis_exporter", "真实", "REDIS_EXPORTER_DEBUG", "redis-cli"} {
		if !strings.Contains(got, want) {
			t.Fatalf("结论应包含 %q：%s", want, got)
		}
	}
}

// 日志里同时有"马甲"和真实原因时，必须报真实原因（马甲只做兜底）。
func TestDescribeExporterLogPrefersRealReason(t *testing.T) {
	logs := `level=error msg="Couldn't connect to redis instance"
level=debug msg="DialURL() failed, err: dial tcp 203.195.191.75:6379: connect: connection refused"
redis_exporter_last_scrape_error{err="dial redis: unknown network redis"} 1`

	got := DescribeExporterLog(logs)
	if !strings.Contains(got, "连不上目标端口") {
		t.Fatalf("有真实原因时应报真实原因，实际：%s", got)
	}
	if strings.Contains(got, "马甲") {
		t.Fatalf("不应停留在兜底解释：%s", got)
	}
	// 认证失败同理（它比"unknown network"更具体）。
	auth := `redis_exporter_last_scrape_error{err="dial redis: unknown network redis"} 1
level=debug msg="DialURL() failed, err: WRONGPASS invalid username-password pair"`
	if got := DescribeExporterLog(auth); !strings.Contains(got, "认证失败") {
		t.Fatalf("含 WRONGPASS 时应报认证失败，实际：%s", got)
	}
}

// 同一个日志里若已经含真实原因，也要能被识别（不能只认马甲）。
func TestDescribeExporterLogKeepsRealReasons(t *testing.T) {
	cases := []struct {
		name, logs, want string
	}{
		{"连接被拒", `dial tcp 10.0.0.9:6379: connect: connection refused`, "连不上目标端口"},
		{"认证失败", `WRONGPASS invalid username-password pair`, "认证失败"},
		{"解析不了", `lookup app-redis on 127.0.0.11:53: no such host`, "解析不了"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DescribeExporterLog(c.logs)
			if !strings.Contains(got, c.want) {
				t.Fatalf("应识别为 %q，实际：%s", c.want, got)
			}
		})
	}
}
