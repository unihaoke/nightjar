package logpipe

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/segmentio/kafka-go"
	"go.uber.org/zap"
)

// IngestFunc 把一条解析后的日志交给平台既有链路（service.LogAlertService.Ingest）。
//
// 用函数类型注入而不是直接依赖 service 包：既避免包循环，也让消费者可以脱离数据库单测。
type IngestFunc func(ctx context.Context, rec Record) error

// Options 是消费者参数（全部来自 config.KafkaConfig，便于测试时直接构造）。
type Options struct {
	Brokers        []string
	Topic          string
	GroupID        string
	ClientID       string
	MaxBytes       int
	SessionTimeout time.Duration
	CommitInterval time.Duration
	// StartOffset 取值 earliest | latest（其它值按 earliest 处理：宁可重复，不要漏日志）。
	StartOffset string
	// Partitions 为自动建 topic 时的分区数；<=0 时用 3。
	Partitions int
	Ingest     IngestFunc
	Log        *zap.Logger
}

// Consumer 是 Kafka 消费组消费者：把 Filebeat 推送的日志搬进平台日志事件链路。
type Consumer struct {
	opts   Options
	log    *zap.Logger
	reader *kafka.Reader

	// counters 用原子计数：Status 会在 HTTP 请求里读，与消费 goroutine 并发。
	consumed      atomic.Int64
	dropped       atomic.Int64
	failed        atomic.Int64
	lastMessageAt atomic.Int64 // UnixNano，0 表示还没收到过
	lag           atomic.Int64

	mu        sync.RWMutex
	running   bool
	lastError string
	closed    bool
}

// New 构造消费者。brokers 为空时返回 nil——调用方据此判定"未启用日志接入"，
// 不做任何 Kafka 连接尝试（平台必须能无 Kafka 启动）。
func New(opts Options) *Consumer {
	if len(opts.Brokers) == 0 || strings.TrimSpace(opts.Topic) == "" || opts.Ingest == nil {
		return nil
	}
	log := opts.Log
	if log == nil {
		log = zap.NewNop()
	}
	return &Consumer{opts: opts, log: log}
}

// Status 是消费者的运行状态（供页面与 /healthz 展示）。
type Status struct {
	Running     bool     `json:"running"`
	Topic       string   `json:"topic"`
	GroupID     string   `json:"group_id"`
	Brokers     []string `json:"brokers"`
	Consumed    int64    `json:"consumed"`
	Dropped     int64    `json:"dropped"`
	Failed      int64    `json:"failed"`
	Lag         int64    `json:"lag"`
	LastMessage string   `json:"last_message_at"`
	LastError   string   `json:"last_error"`
}

// Status 返回当前状态快照。
func (c *Consumer) Status() Status {
	if c == nil {
		return Status{Brokers: []string{}}
	}
	c.mu.RLock()
	running, lastError := c.running, c.lastError
	c.mu.RUnlock()

	last := ""
	if nanos := c.lastMessageAt.Load(); nanos > 0 {
		last = time.Unix(0, nanos).UTC().Format(time.RFC3339)
	}
	brokers := c.opts.Brokers
	if brokers == nil {
		brokers = []string{}
	}
	return Status{
		Running: running, Topic: c.opts.Topic, GroupID: c.opts.GroupID, Brokers: brokers,
		Consumed: c.consumed.Load(), Dropped: c.dropped.Load(), Failed: c.failed.Load(),
		Lag: c.lag.Load(), LastMessage: last, LastError: lastError,
	}
}

// Start 启动消费循环（非阻塞）。
//
// 平台启动时若 Kafka 不可达，这里**不能**阻断启动：消费循环在后台重试，
// 页面上的日志集成会显示"Kafka 未连接"，其余功能照常。
func (c *Consumer) Start(ctx context.Context) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.running || c.closed {
		c.mu.Unlock()
		return
	}
	c.running = true
	c.mu.Unlock()

	c.reader = kafka.NewReader(c.readerConfig())
	go c.loop(ctx)
	c.log.Info("日志消费已启动（Filebeat → Kafka → 平台日志事件）",
		zap.Strings("brokers", c.opts.Brokers), zap.String("topic", c.opts.Topic),
		zap.String("group_id", c.opts.GroupID))
}

// readerConfig 组装 kafka-go 的 Reader 配置。
func (c *Consumer) readerConfig() kafka.ReaderConfig {
	maxBytes := c.opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 10 * 1024 * 1024
	}
	sessionTimeout := c.opts.SessionTimeout
	if sessionTimeout <= 0 {
		sessionTimeout = 30 * time.Second
	}
	// StartOffset：earliest 从头消费（宁可重复处理，也不漏掉积压）；
	// 日志事件本身有指纹与窗口去重，重复处理的代价远低于漏日志。
	startOffset := kafka.FirstOffset
	if strings.EqualFold(strings.TrimSpace(c.opts.StartOffset), "latest") {
		startOffset = kafka.LastOffset
	}
	return kafka.ReaderConfig{
		Brokers: c.opts.Brokers,
		GroupID: c.opts.GroupID,
		Topic:   c.opts.Topic,
		// Dialer 承载 ClientID（kafka-go 把 ClientID 放在 Dialer 上）。
		Dialer:         &kafka.Dialer{ClientID: c.opts.ClientID, Timeout: 10 * time.Second},
		MinBytes:       1,
		MaxBytes:       maxBytes,
		MaxWait:        time.Second,
		StartOffset:    startOffset,
		CommitInterval: c.opts.CommitInterval,
		SessionTimeout: sessionTimeout,
		Logger:         kafkaLogger{c.log, false},
		ErrorLogger:    kafkaLogger{c.log, true},
	}
}

// loop 是消费主循环。
//
// 位点提交策略（关键）：
//   - 解析失败（脏消息）：计数 dropped → **提交位点**。否则一条脏消息会永远堵在这个分区前面，
//     把后面所有日志都卡住——这是"宁可丢一条坏数据，也不能停一整条管道"；
//   - 落库失败：计数 failed → **不提交**，退避后重试同一条。数据库抖动不能变成丢日志。
func (c *Consumer) loop(ctx context.Context) {
	defer func() {
		if c.reader != nil {
			if err := c.reader.Close(); err != nil {
				c.log.Warn("关闭 Kafka 消费者失败", zap.Error(err))
			}
		}
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	c.ensureTopic(ctx)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			c.log.Info("日志消费已停止（上下文结束）")
			return
		}
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			c.setError(fmt.Sprintf("拉取消息失败：%v", err))
			c.log.Warn("拉取 Kafka 消息失败，退避后重试", zap.Error(err))
			sleepCtx(ctx, backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		record, parseErr := c.recordOf(msg)
		if parseErr != nil {
			c.dropped.Add(1)
			c.log.Warn("丢弃一条无法解析的日志消息（已提交位点，避免堵住分区）",
				zap.String("topic", msg.Topic), zap.Int("partition", msg.Partition),
				zap.Int64("offset", msg.Offset), zap.String("raw_prefix", prefixOf(msg.Value, 200)),
				zap.Error(parseErr))
			c.commit(ctx, msg)
			continue
		}

		if err := c.opts.Ingest(ctx, record); err != nil {
			c.failed.Add(1)
			c.setError(fmt.Sprintf("写入日志事件失败：%v", err))
			c.log.Warn("日志事件落库失败，未提交位点，退避后重试",
				zap.Int("partition", msg.Partition), zap.Int64("offset", msg.Offset), zap.Error(err))
			sleepCtx(ctx, backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}

		// 成功：重置退避、计数、提交位点。
		backoff = time.Second
		c.consumed.Add(1)
		c.lastMessageAt.Store(time.Now().UnixNano())
		c.setError("")
		c.commit(ctx, msg)
		if stats := c.reader.Stats(); stats.Lag > 0 {
			c.lag.Store(stats.Lag)
		}
	}
}

// recordOf 解析消息并映射成平台记录。
func (c *Consumer) recordOf(msg kafka.Message) (Record, error) {
	event, err := ParseEvent(msg.Value)
	if err != nil {
		return Record{}, err
	}
	record := event.Record()
	if strings.TrimSpace(record.Service) == "" {
		return Record{}, fmt.Errorf("事件缺少服务名（fields.service / fields.server / host.name 都为空）")
	}
	return record, nil
}

// commit 提交位点；失败只告警——下轮会重新消费并再次提交（去重由日志事件层承担）。
func (c *Consumer) commit(ctx context.Context, msg kafka.Message) {
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := c.reader.CommitMessages(commitCtx, msg); err != nil {
		c.log.Warn("提交 Kafka 位点失败（该条可能被重复消费，日志事件层会去重）", zap.Error(err))
	}
}

// ensureTopic 在启动时确保日志主题存在（幂等）。
//
// 为什么还要做这件事：compose 里开了 topic 自动创建，但那是"第一条消息到达时"才创建，
// 而且如果平台用的是外部 Kafka（自动创建被关掉），第一条日志会被直接丢弃。
// 这里主动建一次：已存在时的报错属于正常路径，只记 debug。
func (c *Consumer) ensureTopic(ctx context.Context) {
	partitions := c.opts.Partitions
	if partitions <= 0 {
		partitions = 3
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	conn, err := kafka.DialContext(dialCtx, "tcp", c.opts.Brokers[0])
	if err != nil {
		c.setError(fmt.Sprintf("连接 Kafka 失败：%v", err))
		c.log.Warn("日志消费启动时连不上 Kafka（会持续重试；请检查 brokers 与网络）",
			zap.String("broker", c.opts.Brokers[0]), zap.Error(err))
		return
	}
	defer func() { _ = conn.Close() }()

	if err := conn.CreateTopics(kafka.TopicConfig{
		Topic: c.opts.Topic, NumPartitions: partitions, ReplicationFactor: 1,
	}); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "exist") {
			c.log.Debug("日志主题已存在，跳过创建", zap.String("topic", c.opts.Topic))
		} else {
			// 不阻断：broker 可能禁止客户端建 topic，但主题已由运维建好，照样能消费。
			c.log.Warn("日志主题创建失败（可能已存在或 broker 禁止建 topic，不影响消费）",
				zap.String("topic", c.opts.Topic), zap.Error(err))
		}
		return
	}
	c.log.Info("日志主题已创建", zap.String("topic", c.opts.Topic), zap.Int("partitions", partitions))
}

// setError 记录最近一次错误（空串表示恢复正常）。
func (c *Consumer) setError(message string) {
	c.mu.Lock()
	c.lastError = message
	c.mu.Unlock()
}

// Close 停止消费（进程退出时调用）。
func (c *Consumer) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	c.closed = true
	reader := c.reader
	c.mu.Unlock()
	if reader == nil {
		return nil
	}
	return reader.Close()
}

// ProbeResult 是「测试 Kafka 连接」的结果。
type ProbeResult struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	Address    string `json:"address"`
	Topic      string `json:"topic"`
	Partitions int    `json:"partitions"`
	LatencyMS  int64  `json:"latency_ms"`
}

// Probe 探测 Kafka 可达性与日志主题是否存在（页面「测试连接」与自检第 1 步共用）。
//
// 特意返回**结构化结果**而不是 error：调用方要把它原样展示给使用者，
// 所以"哪一步失败、失败在哪个地址"比一个 error 更重要。
func Probe(ctx context.Context, brokers []string, topic string, timeout time.Duration) ProbeResult {
	result := ProbeResult{Topic: topic}
	if len(brokers) == 0 {
		result.Message = "未配置 Kafka brokers（日志集成不可用）"
		return result
	}
	result.Address = brokers[0]
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	dialer := &kafka.Dialer{Timeout: timeout}
	partitions, err := dialer.LookupPartitions(dialCtx, "tcp", brokers[0], topic)
	result.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		result.Message = fmt.Sprintf("连接 Kafka 失败（%s）：%v；请确认 brokers 可达、topic 已创建", brokers[0], err)
		return result
	}
	result.OK = true
	result.Partitions = len(partitions)
	result.Message = fmt.Sprintf("Kafka 可达，主题 %s 存在（%d 个分区）", topic, len(partitions))
	return result
}

// sleepCtx 可被取消的退避等待。
func sleepCtx(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// prefixOf 截取原始消息前缀用于日志（避免把整条日志写进平台日志）。
func prefixOf(raw []byte, limit int) string {
	if len(raw) <= limit {
		return string(raw)
	}
	return string(raw[:limit]) + "…"
}

// kafkaLogger 把 kafka-go 的内部日志接到 zap 上。
//
// 为什么必须接：连接不上 Kafka 时，kafka-go 的默认日志会打到标准错误里，
// 与平台的日志体系割裂，排查时容易"什么都看不到"。这里统一收口成 warn 级。
type kafkaLogger struct {
	log   *zap.Logger
	isErr bool
}

func (l kafkaLogger) Printf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	if l.isErr {
		l.log.Warn("kafka 客户端错误：" + message)
		return
	}
	l.log.Debug("kafka 客户端：" + message)
}
