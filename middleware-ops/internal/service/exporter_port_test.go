package service

import "testing"

// 锁定「Exporter 端口与实例端口冲突」的自愈规则（INC-010）。
//
// 真实故障：集成里的 Exporter 端口被填成了实例端口 6379，host 网络下 Exporter 直接监听
// 宿主端口 → 与 Redis 抢同一个端口（bind 失败或把实例遮住）。

func TestExporterPortConflictFix(t *testing.T) {
	const tplPort = 9121
	cases := []struct {
		name         string
		port         int
		instancePort int
		addrHost     string
		targetHost   string
		want         int
	}{
		{"同机回环地址 + 同端口 → 改用模板端口", 6379, 6379, "127.0.0.1", "203.195.191.75", tplPort},
		{"同机（地址即目标机）+ 同端口 → 改用模板端口", 6379, 6379, "203.195.191.75", "203.195.191.75", tplPort},
		{"实例在别的机器 → 同端口无妨", 6379, 6379, "10.0.0.5", "203.195.191.75", 0},
		{"端口不同 → 不动", 9121, 6379, "127.0.0.1", "203.195.191.75", 0},
		{"未配置端口 → 不动", 0, 6379, "127.0.0.1", "203.195.191.75", 0},
		{"模板端口恰好也是冲突端口 → 无法自愈，不动", 6379, 6379, "127.0.0.1", "203.195.191.75", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tplPortValue := tplPort
			if c.name == "模板端口恰好也是冲突端口 → 无法自愈，不动" {
				tplPortValue = 6379
			}
			got := exporterPortConflictFix(c.port, c.instancePort, c.addrHost, c.targetHost, tplPortValue)
			if got != c.want {
				t.Fatalf("want %d, got %d", c.want, got)
			}
		})
	}
}

func TestSameMachine(t *testing.T) {
	cases := []struct {
		addr, target string
		want         bool
	}{
		{"127.0.0.1", "203.195.191.75", true}, // 回环 = 跑 Exporter 的那台机器
		{"localhost", "10.0.0.9", true},       // 同上
		{"::1", "10.0.0.9", true},             // 同上
		{"10.0.0.9", "10.0.0.9", true},        // 完全相同
		{"10.0.0.9", "10.0.0.10", false},      // 不同机器
		{"app-redis", "10.0.0.9", false},      // 容器名（远程模式下本就该改）
		{"", "10.0.0.9", false},               // 空地址
		{"10.0.0.9 ", " 10.0.0.9", true},      // 忽略空白
		{"10.0.0.9", "", false},               // 目标机未知
	}
	for _, c := range cases {
		if got := sameMachine(c.addr, c.target); got != c.want {
			t.Fatalf("sameMachine(%q, %q) = %v, want %v", c.addr, c.target, got, c.want)
		}
	}
}
