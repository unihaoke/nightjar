package service

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"middleware-ops/internal/config"
)

// 本文件锁定「日志集成」服务层里那些**纯函数判定**：
// 路径解析、取消布尔、host:port 拆分、时长文案。
// 它们都很小，但每一个判错的后果都落在"日志收不到"或"显示错误的时间"上——
// 而这两种现象从现象上几乎无法区分是配置问题还是解析问题。

// TestSplitLogPaths 校验日志路径的拆分规则。
//
// 真实场景：多行输入框里一行一个 glob；也有人用逗号写在同一行。
// 两种都要吃，且要丢掉空行与多余空白——否则会把 " " 当成一个路径交给 Filebeat，
// Filebeat 会为它报一个看不懂的 glob 错误。
func TestSplitLogPaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"多行", "/var/log/app/*.log\n/data/logs/**/*.log", []string{"/var/log/app/*.log", "/data/logs/**/*.log"}},
		{"逗号", "/var/log/a.log, /var/log/b.log", []string{"/var/log/a.log", "/var/log/b.log"}},
		{"分号与混用", "/a.log;/b.log\n/c.log", []string{"/a.log", "/b.log", "/c.log"}},
		{"空白与空行被丢弃", "\n  \n /a.log \n\n", []string{"/a.log"}},
		{"空输入", "   \n ", []string{}},
	}
	for _, c := range cases {
		got := splitLogPaths(c.in)
		if len(got) != len(c.want) {
			t.Fatalf("%s: splitLogPaths(%q) = %v，期望 %v", c.name, c.in, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("%s: 第 %d 项 = %q，期望 %q", c.name, i, got[i], c.want[i])
			}
		}
	}
}

// TestIsFalse 校验"显式关闭"的判定：未填写时必须保持默认开启。
//
// 这一条直接对应集成表单的「合并多行堆栈」开关：多行合并默认开，
// 若把空串判成 false，Java 应用的每行堆栈都会变成一条独立事件，指纹会瞬间爆掉。
func TestIsFalse(t *testing.T) {
	for _, raw := range []string{"false", "FALSE", "0", "no", "off", "n", " false "} {
		if !isFalse(raw) {
			t.Fatalf("isFalse(%q) 应为 true", raw)
		}
	}
	for _, raw := range []string{"", "  ", "true", "1", "yes", "on"} {
		if isFalse(raw) {
			t.Fatalf("isFalse(%q) 应为 false（保持默认开启）", raw)
		}
	}
}

// TestSplitHostPortForProbe 校验自检探测用的 host/port 拆分。
func TestSplitHostPortForProbe(t *testing.T) {
	cases := []struct {
		address  string
		wantHost string
		wantPort int
	}{
		{"10.0.0.5:9092", "10.0.0.5", 9092},
		{"kafka.internal:9092", "kafka.internal", 9092},
		{"10.0.0.5", "10.0.0.5", 0},
		{"", "", 0},
		{"[fd00::1]:9092", "fd00::1", 9092},
	}
	for _, c := range cases {
		if got := splitHost(c.address); got != c.wantHost {
			t.Fatalf("splitHost(%q)=%q，期望 %q", c.address, got, c.wantHost)
		}
		if got := splitPort(c.address); got != c.wantPort {
			t.Fatalf("splitPort(%q)=%d，期望 %d", c.address, got, c.wantPort)
		}
	}
}

// TestHumanDuration 校验时长文案：自检里要说"3 分钟前"，而不是"3m12.5s 前"。
func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second:   "45 秒",
		3 * time.Minute:    "3 分钟",
		5 * time.Hour:      "5 小时",
		50 * time.Hour:     "2 天",
		2 * 24 * time.Hour: "2 天",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Fatalf("humanDuration(%v)=%q，期望 %q", d, got, want)
		}
	}
}

// TestLogPipelineStatusJSONContract 锁定日志页「Kafka 采集链路」卡片的字段契约。
//
// 前端按这些 snake_case 字段取值，改名不会有编译错误、只会让页面静默变空，
// 所以在这里显式钉住（与 logpipe 包里的同名守卫形成双保险：那边守的是消费者状态，
// 这边守的是页面真正消费的那个结构）。
func TestLogPipelineStatusJSONContract(t *testing.T) {
	raw, err := json.Marshal(LogPipelineStatus{})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	text := string(raw)
	for _, key := range []string{
		`"enabled"`, `"brokers"`, `"external_address"`, `"topic"`, `"group_id"`,
		`"running"`, `"consumed"`, `"dropped"`, `"failed"`,
		`"last_message_at"`, `"last_error"`, `"note"`,
	} {
		if !strings.Contains(text, key) {
			t.Fatalf("LogPipelineStatus 缺少字段 %s（前端契约不能改名）：%s", key, text)
		}
	}
}

// TestLogPipelineStatusExplainsWhyEmpty 校验"收不到日志时页面有原因可看"。
//
// 未启用 Kafka 时，状态里的 note 必须明确说明原因——只给一个 consumed=0
// 会让人以为是采集坏了（本项目硬约定：没有数据要说明原因，见 INC-016）。
func TestLogPipelineStatusExplainsWhyEmpty(t *testing.T) {
	cfg := &config.Config{}
	cfg.Kafka.Enabled = false
	pipeline := NewLogPipeline(cfg, nil, nil)

	status := pipeline.Status()
	if status.Running || status.Consumed != 0 {
		t.Fatalf("未启用时不应处于运行中：%+v", status)
	}
	if !strings.Contains(status.Note, "关闭") {
		t.Fatalf("未启用时必须说明原因，实际 note=%q", status.Note)
	}

	// 启用但没配 brokers：同样是"不可用 + 原因"，而不是静默的 0。
	cfg.Kafka.Enabled = true
	status = pipeline.Status()
	if status.Enabled != true || len(status.Brokers) != 0 {
		t.Fatalf("状态应如实反映配置：%+v", status)
	}
	if !strings.Contains(status.Note, "brokers") {
		t.Fatalf("缺少 brokers 时必须提示配置项，实际 note=%q", status.Note)
	}
}
