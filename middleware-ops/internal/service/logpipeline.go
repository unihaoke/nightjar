package service

import (
	"context"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/logpipe"
)

// LogPipeline 是「日志集成」的平台侧接收链路：Kafka 消费 → 平台日志事件。
//
// 为什么单独一个服务而不是塞进 LogAlertService：日志告警域只负责"事件怎么存、怎么去重、
// 怎么告警"，而"日志从哪里来"是可替换的接入方式（Filebeat+Kafka / 应用直推 Hook）。
// 把接入侧独立出来后，换接入方式不需要动告警逻辑。
type LogPipeline struct {
	cfg      *config.Config
	logs     *LogAlertService
	log      *zap.Logger
	consumer *logpipe.Consumer
}

// NewLogPipeline 构造日志接收链路。
func NewLogPipeline(cfg *config.Config, logs *LogAlertService, log *zap.Logger) *LogPipeline {
	return &LogPipeline{cfg: cfg, logs: logs, log: log}
}

// Start 启动 Kafka 消费者。
//
// 未配置 broker 时**不启动**也不报错：日志集成是可选的接入能力，
// 平台必须在没有 Kafka 的环境里照常启动（只写一条说明日志，页面显示未启用）。
func (p *LogPipeline) Start(ctx context.Context) {
	if p == nil || p.cfg == nil {
		return
	}
	if !p.cfg.Kafka.Active() {
		p.log.Info("未启用日志总线：日志集成不可用（在 .env 里配置 KAFKA_BROKERS 后重启即可）",
			zap.Bool("kafka_enabled", p.cfg.Kafka.Enabled))
		return
	}
	p.consumer = logpipe.New(logpipe.Options{
		Brokers:        p.cfg.Kafka.NonEmptyBrokers(),
		Topic:          p.cfg.Kafka.LogTopic,
		GroupID:        p.cfg.Kafka.GroupID,
		ClientID:       p.cfg.Kafka.ClientID,
		MaxBytes:       p.cfg.Kafka.MaxBytes,
		SessionTimeout: p.cfg.Kafka.SessionTimeout,
		CommitInterval: p.cfg.Kafka.CommitInterval,
		StartOffset:    p.cfg.Kafka.StartOffset,
		Ingest:         p.ingest,
		Log:            p.log,
	})
	if p.consumer == nil {
		p.log.Warn("日志消费未启动：消费者参数不完整（brokers/topic/回调）")
		return
	}
	p.consumer.Start(ctx)
}

// ingest 把一条 Filebeat 日志交给既有的日志告警链路。
//
// 指纹、窗口去重、告警、AI 诊断入口全都在 LogAlertService.Ingest 里——
// 接入侧只做"翻译"，不重复实现任何判定逻辑。
func (p *LogPipeline) ingest(ctx context.Context, rec logpipe.Record) error {
	return p.logs.IngestRecord(ctx, rec)
}

// Close 停止消费（进程退出时调用）。
func (p *LogPipeline) Close() error {
	if p == nil || p.consumer == nil {
		return nil
	}
	return p.consumer.Close()
}

// LogPipelineStatus 是页面「Kafka 采集链路」卡片的数据契约。
//
// 字段名与前端逐字对应（snake_case），改名不会有编译错误、只会让页面静默变空。
type LogPipelineStatus struct {
	Enabled         bool     `json:"enabled"`
	Brokers         []string `json:"brokers"`
	ExternalAddress string   `json:"external_address"`
	Topic           string   `json:"topic"`
	GroupID         string   `json:"group_id"`
	Running         bool     `json:"running"`
	Consumed        int64    `json:"consumed"`
	Dropped         int64    `json:"dropped"`
	Failed          int64    `json:"failed"`
	LastMessageAt   string   `json:"last_message_at"`
	LastError       string   `json:"last_error"`
	// Note 用一句话解释"为什么现在收不到日志"：未启用 / 未运行 / 最近的错误。
	// 页面上宁可显示原因，也不要只显示一个 0（本项目硬约定，见 INC-016）。
	Note string `json:"note"`
}

// Status 返回采集链路状态。
func (p *LogPipeline) Status() LogPipelineStatus {
	if p == nil || p.cfg == nil {
		return LogPipelineStatus{Brokers: []string{}, Note: "日志链路未装配"}
	}
	kafka := p.cfg.Kafka
	status := LogPipelineStatus{
		Enabled:         kafka.Enabled,
		Brokers:         kafka.NonEmptyBrokers(),
		ExternalAddress: kafka.ExternalAddress(),
		Topic:           kafka.LogTopic,
		GroupID:         kafka.GroupID,
	}
	if status.Brokers == nil {
		status.Brokers = []string{}
	}
	if p.consumer != nil {
		consumer := p.consumer.Status()
		status.Running = consumer.Running
		status.Consumed = consumer.Consumed
		status.Dropped = consumer.Dropped
		status.Failed = consumer.Failed
		status.LastMessageAt = consumer.LastMessage
		if consumer.LastError != "" {
			status.LastError = consumer.LastError
		}
	}
	switch {
	case !kafka.Enabled:
		status.Note = "日志总线已关闭（kafka.enabled=false）：日志集成不可用"
	case len(status.Brokers) == 0:
		status.Note = "未配置 kafka.brokers：日志集成不可用；请在 .env 里配置后重启平台"
	case !status.Running:
		status.Note = "消费者未运行：启动时可能未启用日志总线，请查看平台启动日志"
	case status.LastError != "":
		status.Note = "消费者运行中，但最近一次操作失败：" + status.LastError
	default:
		status.Note = "消费者运行中：目标机 Filebeat 推送的日志会自动进入日志事件列表"
	}
	return status
}

// IngestRecord 把一条 Kafka 日志记录翻译成平台既有的日志上报结构并落库。
//
// 为什么 ServerName 与 ServerIP 都填同一个值：日志集成渲染的 `fields.server` 就是**目标机地址**
// （集成时写入），而 Ingest 是按 IP 反查已登记的服务器记录来归集的——
// 登记的那条记录带着正确的环境（dev/staging/prod），若查不到则按地址自动登记。
func (s *LogAlertService) IngestRecord(ctx context.Context, rec logpipe.Record) error {
	report := LogReport{
		ServerName: rec.ServerName,
		ServerIP:   rec.ServerName,
		Service:    rec.Service,
		Level:      rec.Level,
		Message:    rec.Message,
		Stacktrace: rec.Stacktrace,
		LogPath:    rec.LogPath,
	}
	if !rec.Timestamp.IsZero() {
		ts := rec.Timestamp
		report.Timestamp = &ts
	}
	_, err := s.Ingest(ctx, report)
	return err
}

// LogPipelineProbeResult 是「测试 Kafka 连接」的结果。
type LogPipelineProbeResult struct {
	OK        bool   `json:"ok"`
	Message   string `json:"message"`
	Address   string `json:"address"`
	Topic     string `json:"topic"`
	LatencyMS int64  `json:"latency_ms"`
}

// Probe 探测平台侧 Kafka 可达性与日志主题是否存在。
//
// 注意这里探的是**平台侧** brokers；"目标机能否连上 Kafka"是另一件事（探对外地址），
// 由日志集成的自检在目标机上执行——两者失败的原因完全不同，不能混为一谈。
func (p *LogPipeline) Probe(ctx context.Context) LogPipelineProbeResult {
	result := LogPipelineProbeResult{}
	if p == nil || p.cfg == nil {
		result.Message = "日志链路未装配"
		return result
	}
	probe := logpipe.Probe(ctx, p.cfg.Kafka.NonEmptyBrokers(), p.cfg.Kafka.LogTopic, 10*time.Second)
	result.OK = probe.OK
	result.Message = probe.Message
	result.Address = probe.Address
	result.Topic = probe.Topic
	result.LatencyMS = probe.LatencyMS
	if !probe.OK && strings.TrimSpace(result.Address) == "" {
		result.Address = p.cfg.Kafka.ExternalAddress()
	}
	return result
}
