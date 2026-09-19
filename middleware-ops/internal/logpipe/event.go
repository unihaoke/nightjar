// Package logpipe 实现「日志集成」的平台侧接收链路：从 Kafka 消费 Filebeat 推送的日志，
// 解析成平台既有的日志事件语义（错误指纹 / 窗口去重 / 告警 / AI 诊断入口）。
//
// 职责边界（刻意划清）：
//   - 本包只做「Kafka → 结构化记录」这一段，不碰数据库、不碰 HTTP；
//   - 落库与去重复用 service.LogAlertService.Ingest，避免同一套指纹逻辑出现两份实现；
//   - 依赖通过函数类型注入（IngestFunc），因此本包可以脱离数据库与 Kafka 单测。
package logpipe

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Event 是 Filebeat 推送到 Kafka 的一条事件文档。
//
// 字段取自 Filebeat 的默认 JSON 编码（output.kafka 不写 codec 段时就是 JSON）：
//
//	{"@timestamp":"...","message":"...","fields":{"service":"...","environment":"...","server":"..."},
//	 "log":{"level":"...","file":{"path":"..."}},"host":{"name":"..."},"agent":{"name":"filebeat"}}
//
// 只声明平台真正会用到的字段：多声明一层无关嵌套，就会在下一次 Filebeat 升级改结构时
// 悄悄失效而没人发现。
type Event struct {
	Timestamp string `json:"@timestamp"`
	Message   string `json:"message"`
	Fields    struct {
		Service     string `json:"service"`
		Environment string `json:"environment"`
		Server      string `json:"server"`
	} `json:"fields"`
	Log struct {
		Level string `json:"level"`
		File  struct {
			Path string `json:"path"`
		} `json:"file"`
	} `json:"log"`
	Host struct {
		Name string `json:"name"`
	} `json:"host"`
}

// Record 是交给平台日志告警链路的一条结构化记录。
type Record struct {
	// ServerName / ServerIP 用于把事件归集到某台服务器（平台按二者之一匹配既有记录）。
	ServerName string
	ServerIP   string
	Service    string
	// Environment 目前不落库（服务器记录上带环境），保留它是为了让上层能按需使用。
	Environment string
	Level       string
	Message     string
	Stacktrace  string
	LogPath     string
	Timestamp   time.Time
}

// ErrEmptyMessage 表示事件里没有正文：不是错误日志，直接丢弃即可（不计入失败）。
var ErrEmptyMessage = fmt.Errorf("事件缺少 message 字段")

// ParseEvent 解析一条 Filebeat 事件。
//
// 解析失败（JSON 非法、没有 message）返回错误：调用方应当**提交位点并丢弃**这条消息，
// 否则一条脏消息会把整个分区堵死（见 consumer.go 的处理策略）。
func ParseEvent(raw []byte) (Event, error) {
	var event Event
	if err := json.Unmarshal(raw, &event); err != nil {
		return Event{}, fmt.Errorf("解析 Filebeat 事件失败: %w", err)
	}
	if strings.TrimSpace(event.Message) == "" {
		return Event{}, ErrEmptyMessage
	}
	return event, nil
}

// RecordOf 把事件映射成平台记录。
//
// 几个约定（都有理由）：
//   - 服务名：优先集成时写入的 fields.service；缺失时退回 fields.server，再退回 host.name。
//     service 是日志事件的主键之一（指纹按服务+消息去重），绝不能为空；
//   - 级别：优先 Filebeat 解析出的 log.level；否则按内容关键字推断，最终兜底 ERROR
//     ——因为日志集成的 Filebeat 输入端默认只放行 ERROR 及以上，能到这儿的本来就该按错误看；
//   - 正文/堆栈：多行合并后 message 里是"首行 + 堆栈"，拆开分别存放，
//     否则指纹会被整段堆栈的细节撑爆，同一个 bug 每次堆栈行号不同就会被当成新事件。
func (e Event) Record() Record {
	service := firstNonEmpty(e.Fields.Service, e.Fields.Server, e.Host.Name, "unknown")
	// 服务器名同样兜底到 service：日志事件的"归属"不能为空——
	// 平台会按它自动登记服务器，留空会让这条日志在"按服务器"的视图里彻底消失。
	server := firstNonEmpty(e.Fields.Server, e.Host.Name, service)

	message, stacktrace := splitMessage(e.Message)
	level := strings.ToUpper(strings.TrimSpace(e.Log.Level))
	if level == "" {
		level = inferLevel(e.Message)
	}

	return Record{
		ServerName:  server,
		Service:     service,
		Environment: strings.TrimSpace(e.Fields.Environment),
		Level:       level,
		Message:     message,
		Stacktrace:  stacktrace,
		LogPath:     strings.TrimSpace(e.Log.File.Path),
		Timestamp:   parseTimestamp(e.Timestamp),
	}
}

// parseTimestamp 解析 Filebeat 的 @timestamp；解析不出来就让上层用当前时间（不伪造时间）。
func parseTimestamp(raw string) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

// splitMessage 把「首行 + 堆栈」拆开；单行事件则只有正文。
func splitMessage(raw string) (message, stacktrace string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		return strings.TrimSpace(trimmed[:idx]), strings.TrimSpace(trimmed[idx+1:])
	}
	return trimmed, ""
}

// levelKeywords 按"从严重到轻微"排列：命中即返回，避免 FATAL 被 WARN 抢先匹配。
var levelKeywords = []struct {
	keyword string
	level   string
}{
	{"FATAL", "FATAL"},
	{"PANIC", "FATAL"},
	{"EXCEPTION", "ERROR"},
	{"ERROR", "ERROR"},
	{"SEVERE", "ERROR"},
	{"WARN", "WARN"},
}

// inferLevel 从正文推断级别：Filebeat 未解析出 log.level 时的兜底。
//
// 大小写都查（很多应用打的是小写 error），但只做整词边界匹配的简化版——
// 宁可用"包含"，也不要漏掉真正的错误：漏掉的代价是这条日志不进告警。
func inferLevel(message string) string {
	upper := strings.ToUpper(message)
	for _, item := range levelKeywords {
		if strings.Contains(upper, item.keyword) {
			return item.level
		}
	}
	return "ERROR"
}

// firstNonEmpty 返回第一个非空（去空白后）的值。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
