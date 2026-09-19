package logpipe

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// 本文件锁定「日志集成」接收侧最容易出错的三件事：
//  1. Filebeat 事件 → 平台记录的字段映射（映射错了，日志页上就全是"unknown"服务，
//     指纹去重也跟着失效）；
//  2. 脏消息的处理策略（必须提交位点，否则一条坏消息堵死整个分区）；
//  3. Status 的 JSON 字段名——前端契约就是它，改名等于悄悄破坏页面。

// TestParseEvent 校验事件解析与"空正文"的判定。
func TestParseEvent(t *testing.T) {
	valid := `{"@timestamp":"2024-05-01T10:00:00.123Z","message":"boom","fields":{"service":"order-api"}}`
	event, err := ParseEvent([]byte(valid))
	if err != nil {
		t.Fatalf("合法事件解析失败：%v", err)
	}
	if event.Message != "boom" || event.Fields.Service != "order-api" {
		t.Fatalf("字段解析错误：%+v", event)
	}

	if _, err := ParseEvent([]byte("{不是 JSON")); err == nil {
		t.Fatal("非法 JSON 必须报错（否则会被当成空事件静默丢弃）")
	}
	// 空正文不是"错误"，是一种正常形态：Filebeat 可能推来只带元数据的事件。
	if _, err := ParseEvent([]byte(`{"message":"   "}`)); err != ErrEmptyMessage {
		t.Fatalf("空 message 应返回 ErrEmptyMessage，实际 %v", err)
	}
}

// TestEventRecordMapping 校验字段映射与兜底顺序。
func TestEventRecordMapping(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantService string
		wantServer  string
		wantLevel   string
		wantMessage string
		wantStack   string
		wantPath    string
	}{
		{
			name: "正常：fields 里带服务与环境",
			raw: `{"@timestamp":"2024-05-01T10:00:00Z","message":"ERROR db down",
			      "fields":{"service":"order-api","environment":"prod","server":"10.0.0.9"},
			      "log":{"file":{"path":"/var/log/app/order.log"}}}`,
			wantService: "order-api", wantServer: "10.0.0.9", wantLevel: "ERROR",
			wantMessage: "ERROR db down", wantPath: "/var/log/app/order.log",
		},
		{
			name:        "服务名缺失时退回 server，再退回 host.name",
			raw:         `{"message":"boom","fields":{"server":"10.0.0.9"},"host":{"name":"node-1"}}`,
			wantService: "10.0.0.9", wantServer: "10.0.0.9", wantLevel: "ERROR", wantMessage: "boom",
		},
		{
			name:        "都没有时兜底 unknown（不能让 service 为空，指纹的主键就是它）",
			raw:         `{"message":"boom"}`,
			wantService: "unknown", wantServer: "unknown", wantLevel: "ERROR", wantMessage: "boom",
		},
		{
			name:        "Filebeat 解析出的 log.level 优先于内容推断",
			raw:         `{"message":"ERROR-looking text","log":{"level":"warn"}}`,
			wantService: "unknown", wantServer: "unknown", wantLevel: "WARN", wantMessage: "ERROR-looking text",
		},
		{
			// 堆栈内部的缩进要保留（只去掉首尾空白）：缩进是堆栈可读性的一部分。
			name:        "多行堆栈：首行进正文，其余进堆栈",
			raw:         `{"message":"java.lang.NullPointerException: boom\n\tat com.demo.A.f(A.java:10)\n\tat com.demo.B.g(B.java:20)"}`,
			wantService: "unknown", wantServer: "unknown", wantLevel: "ERROR",
			wantMessage: "java.lang.NullPointerException: boom",
			wantStack:   "at com.demo.A.f(A.java:10)\n\tat com.demo.B.g(B.java:20)",
		},
	}
	for _, c := range cases {
		event, err := ParseEvent([]byte(c.raw))
		if err != nil {
			t.Fatalf("%s: 解析失败：%v", c.name, err)
		}
		got := event.Record()
		if got.Service != c.wantService {
			t.Fatalf("%s: service=%q，期望 %q", c.name, got.Service, c.wantService)
		}
		if got.ServerName != c.wantServer {
			t.Fatalf("%s: server=%q，期望 %q", c.name, got.ServerName, c.wantServer)
		}
		if got.Level != c.wantLevel {
			t.Fatalf("%s: level=%q，期望 %q", c.name, got.Level, c.wantLevel)
		}
		if got.Message != c.wantMessage {
			t.Fatalf("%s: message=%q，期望 %q", c.name, got.Message, c.wantMessage)
		}
		if got.Stacktrace != c.wantStack {
			t.Fatalf("%s: stacktrace=%q，期望 %q", c.name, got.Stacktrace, c.wantStack)
		}
		if got.LogPath != c.wantPath {
			t.Fatalf("%s: log_path=%q，期望 %q", c.name, got.LogPath, c.wantPath)
		}
	}
}

// TestInferLevel 校验没有 log.level 时的关键字推断（顺序很重要：FATAL 不能被 WARN 抢走）。
func TestInferLevel(t *testing.T) {
	cases := map[string]string{
		"FATAL: cannot start":        "FATAL",
		"panic: runtime error":       "FATAL",
		"java.lang.RuntimeException": "ERROR",
		"connection error to redis":  "ERROR",
		"WARN slow query":            "WARN",
		// 同级混合时按"更严重优先"：这里既有 FATAL 也有 WARN，必须判 FATAL。
		"FATAL while WARN appears": "FATAL",
		"全部都没有关键字":                 "ERROR",
	}
	for message, want := range cases {
		if got := inferLevel(message); got != want {
			t.Fatalf("inferLevel(%q)=%q，期望 %q", message, got, want)
		}
	}
}

// TestParseTimestamp 校验时间解析：解析不出来时返回零值（上层用当前时间，不伪造时间）。
func TestParseTimestamp(t *testing.T) {
	if got := parseTimestamp("2024-05-01T10:00:00.123456789Z"); got.IsZero() || got.Location() != time.UTC {
		t.Fatalf("RFC3339Nano 应解析成 UTC，实际 %v", got)
	}
	if got := parseTimestamp("2024-05-01T10:00:00Z"); got.IsZero() {
		t.Fatalf("RFC3339 应解析成功，实际 %v", got)
	}
	if got := parseTimestamp("not-a-time"); !got.IsZero() {
		t.Fatalf("非法时间应返回零值，实际 %v", got)
	}
}

// TestNewDisabled 校验"没配 broker 就不启动消费者"：平台必须能在没有 Kafka 的环境下启动。
func TestNewDisabled(t *testing.T) {
	ingest := func(context.Context, Record) (IngestOutcome, error) { return IngestOutcome{}, nil }
	if c := New(Options{Ingest: ingest}); c != nil {
		t.Fatal("brokers 为空时应返回 nil（未启用日志接入）")
	}
	if c := New(Options{Brokers: []string{"kafka:9092"}, Ingest: ingest}); c != nil {
		t.Fatal("topic 为空时应返回 nil")
	}
	if c := New(Options{Brokers: []string{"kafka:9092"}, Topic: "t"}); c != nil {
		t.Fatal("没有 ingest 回调时应返回 nil")
	}
	if c := New(Options{Brokers: []string{"kafka:9092"}, Topic: "t", Ingest: ingest}); c == nil {
		t.Fatal("参数齐备时应返回可用消费者")
	}
}

// TestConsumerStartAndClose 在不存在的 broker 上启动：必须不 panic、状态可查、可关闭。
//
// 这条用例模拟"Kafka 还没起来/地址填错"的现场：平台不能因此崩溃或卡住。
func TestConsumerStartAndClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	consumer := New(Options{
		Brokers: []string{"127.0.0.1:1"}, // 必然连不上，且失败很快
		Topic:   "mwops-logs", GroupID: "test-group", ClientID: "test",
		Ingest: func(context.Context, Record) (IngestOutcome, error) { return IngestOutcome{}, nil },
		Log:    zap.NewNop(),
	})
	if consumer == nil {
		t.Fatal("应返回可用消费者")
	}
	consumer.Start(ctx)

	// 等待 ensureTopic 的失败被记录（不依赖具体实现细节，只看最终状态）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if consumer.Status().LastError != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	status := consumer.Status()
	if !status.Running {
		t.Fatal("Start 之后应处于运行中")
	}
	if status.LastError == "" {
		t.Fatal("连不上 broker 时必须记录 last_error，否则页面显示「运行中」会误导使用者")
	}
	if status.Topic != "mwops-logs" || status.GroupID != "test-group" {
		t.Fatalf("状态里应带上 topic/group：%+v", status)
	}
	if err := consumer.Close(); err != nil {
		t.Fatalf("Close 不应报错：%v", err)
	}
}

// TestStatusJSONContract 锁定前端契约的字段名。
//
// 前端的「Kafka 采集链路」卡片就是按这些 snake_case 字段取值；改名不会有编译错误，
// 只会让页面静默变空，所以在这里显式钉住。
func TestStatusJSONContract(t *testing.T) {
	raw, err := json.Marshal(Status{})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	text := string(raw)
	for _, key := range []string{
		`"running"`, `"topic"`, `"group_id"`, `"brokers"`, `"consumed"`,
		`"dropped"`, `"failed"`, `"lag"`, `"last_message_at"`, `"last_error"`,
	} {
		if !strings.Contains(text, key) {
			t.Fatalf("Status 缺少字段 %s（前端契约不能改名）：%s", key, text)
		}
	}
}

// TestProbeWithoutBrokers 校验未配置时的探测结果：给出可执行的解释而不是一个 error。
func TestProbeWithoutBrokers(t *testing.T) {
	result := Probe(context.Background(), nil, "mwops-logs", time.Second)
	if result.OK {
		t.Fatal("没有 broker 时不应报告成功")
	}
	if !strings.Contains(result.Message, "未配置") {
		t.Fatalf("应说明「未配置 Kafka」，实际 %q", result.Message)
	}
}
