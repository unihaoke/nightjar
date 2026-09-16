package monitor

import (
	"fmt"
	"sort"
	"strings"
)

// Profile 是中间件类型的指标画像（指标名 → 中文展示名/单位/阈值）。
//
// 覆盖设计文档 4.2 列出的核心指标集合。
type Profile struct {
	MWType  string
	Metrics []MetricSpec
}

// ThresholdMode 显式声明阈值判定方式。
//
// 不使用「0 表示未配置」这类哨兵值：ES 集群 red 状态的取值就是 0，
// 与「未配置」无法区分，曾导致 red 被判成 warning、yellow 被判成 ok。
type ThresholdMode string

const (
	// ThresholdNone 纯观测型指标（无阈值）。
	ThresholdNone ThresholdMode = ""
	// ThresholdHigherWorse 越高越差：value >= Critical → critical，value >= Warning → warning。
	ThresholdHigherWorse ThresholdMode = "higher_worse"
	// ThresholdLowerWorse 越低越差（含边界）：value <= Warning → warning，value <= Critical → critical。
	// 采用含边界比较，允许 0 作为有效临界值（ES red=0）。约束：Warning > Critical。
	ThresholdLowerWorse ThresholdMode = "lower_worse"
	// ThresholdBoolDown 布尔型「正常/异常」：value <= Critical → critical，否则 ok（如 up 指标，Critical=0）。
	ThresholdBoolDown ThresholdMode = "bool_down"
)

// MetricSpec 描述一个指标的元信息与 PromQL 模板。
type MetricSpec struct {
	Name        string
	DisplayName string
	Unit        string
	Category    string
	// Expr 中 {selector} 会被替换为实例标签匹配串。
	Expr string
	// Mode 为阈值判定方式；Mode 为空表示无阈值。
	Mode ThresholdMode
	// WarningThreshold / CriticalThreshold 语义随 Mode 变化，见 ThresholdMode 注释。
	WarningThreshold  float64
	CriticalThreshold float64
	// Generator 为模拟器的生成参数。
	Generator SeriesSpec
}

// SeriesSpec 描述模拟数据的形态。
type SeriesSpec struct {
	Base      float64
	Amplitude float64
	Noise     float64
	// Trend 为每采样点的线性漂移。
	Trend float64
	Unit  string
}

// profiles 保存全部中间件画像。
var profiles = map[string]Profile{
	"redis": {
		MWType: "redis",
		Metrics: []MetricSpec{
			{Name: "memory_usage_percent", DisplayName: "内存使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `redis_memory_used_bytes{selector} / redis_memory_max_bytes{selector} * 100`,
				WarningThreshold: 70, CriticalThreshold: 85, Generator: SeriesSpec{Base: 62, Amplitude: 8, Noise: 3, Trend: 0.35}},
			{Name: "connected_clients", DisplayName: "连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `redis_connected_clients{selector}`,
				WarningThreshold: 800, CriticalThreshold: 1000, Generator: SeriesSpec{Base: 420, Amplitude: 60, Noise: 25, Trend: 1.2}},
			{Name: "instantaneous_ops_per_sec", DisplayName: "QPS", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `rate(redis_commands_processed_total{selector}[5m])`,
				WarningThreshold: 20000, CriticalThreshold: 40000, Generator: SeriesSpec{Base: 6000, Amplitude: 1500, Noise: 600, Trend: 12}},
			{Name: "keyspace_hit_rate", DisplayName: "命中率", Unit: "%", Category: "performance", Mode: ThresholdLowerWorse,
				Expr:             `rate(redis_keyspace_hits_total{selector}[5m]) / (rate(redis_keyspace_hits_total{selector}[5m]) + rate(redis_keyspace_misses_total{selector}[5m])) * 100`,
				WarningThreshold: 90, CriticalThreshold: 80, Generator: SeriesSpec{Base: 97, Amplitude: 1.5, Noise: 0.8, Trend: -0.06}},
			{Name: "db_keys", DisplayName: "key 数量", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `sum(redis_db_keys{selector})`,
				WarningThreshold: 5000000, CriticalThreshold: 10000000, Generator: SeriesSpec{Base: 1200000, Amplitude: 50000, Noise: 20000, Trend: 900}},
			{Name: "slowlog_length", DisplayName: "慢查询数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `redis_slowlog_length{selector}`,
				WarningThreshold: 10, CriticalThreshold: 50, Generator: SeriesSpec{Base: 2, Amplitude: 2, Noise: 1.5, Trend: 0.05}},
			{Name: "evicted_keys", DisplayName: "淘汰 key 数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(redis_evicted_keys_total{selector}[5m])`,
				WarningThreshold: 1, CriticalThreshold: 20, Generator: SeriesSpec{Base: 0.5, Amplitude: 1, Noise: 0.6, Trend: 0.02}},
			{Name: "blocked_clients", DisplayName: "阻塞客户端", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `redis_blocked_clients{selector}`,
				WarningThreshold: 1, CriticalThreshold: 5, Generator: SeriesSpec{Base: 0, Amplitude: 0.6, Noise: 0.4}},
		},
	},
	"kafka": {
		MWType: "kafka",
		Metrics: []MetricSpec{
			{Name: "broker_up", DisplayName: "Broker 状态", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `up{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 1, Amplitude: 0, Noise: 0}},
			{Name: "partition_count", DisplayName: "Topic 分区数", Unit: "", Category: "resource", Mode: ThresholdNone,
				Expr:             `sum(kafka_topic_partitions{selector})`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 180, Amplitude: 0, Noise: 0}},
			{Name: "consumer_lag", DisplayName: "消费者组 Lag", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `sum(kafka_consumergroup_lag{selector})`,
				WarningThreshold: 10000, CriticalThreshold: 100000, Generator: SeriesSpec{Base: 4200, Amplitude: 3000, Noise: 900, Trend: 120}},
			{Name: "produce_rate", DisplayName: "生产速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `sum(rate(kafka_topic_partition_current_offset{selector}[5m]))`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 3200, Amplitude: 400, Noise: 150, Trend: 8}},
			{Name: "consume_rate", DisplayName: "消费速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `sum(rate(kafka_consumergroup_current_offset{selector}[5m]))`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 2900, Amplitude: 500, Noise: 220, Trend: 3}},
			{Name: "under_replicated_partitions", DisplayName: "ISR 副本不足分区", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `sum(kafka_topic_partition_under_replicated_partition{selector})`,
				WarningThreshold: 1, CriticalThreshold: 10, Generator: SeriesSpec{Base: 0, Amplitude: 0.8, Noise: 0.5}},
		},
	},
	"mysql": {
		MWType: "mysql",
		Metrics: []MetricSpec{
			{Name: "qps", DisplayName: "QPS", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `rate(mysql_global_status_queries{selector}[5m])`,
				WarningThreshold: 8000, CriticalThreshold: 15000, Generator: SeriesSpec{Base: 2400, Amplitude: 500, Noise: 200, Trend: 6}},
			{Name: "tps", DisplayName: "TPS", Unit: "", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(mysql_global_status_questions{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 900, Amplitude: 200, Noise: 80, Trend: 2}},
			{Name: "threads_connected", DisplayName: "连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `mysql_global_status_threads_connected{selector}`,
				WarningThreshold: 400, CriticalThreshold: 500, Generator: SeriesSpec{Base: 230, Amplitude: 40, Noise: 15, Trend: 0.8}},
			{Name: "slow_queries", DisplayName: "慢查询数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(mysql_global_status_slow_queries{selector}[5m])`,
				WarningThreshold: 5, CriticalThreshold: 30, Generator: SeriesSpec{Base: 1.2, Amplitude: 1.5, Noise: 1, Trend: 0.06}},
			{Name: "buffer_pool_hit_rate", DisplayName: "缓冲池命中率", Unit: "%", Category: "performance", Mode: ThresholdLowerWorse,
				Expr:             `(1 - rate(mysql_global_status_innodb_buffer_pool_reads{selector}[5m]) / rate(mysql_global_status_innodb_buffer_pool_read_requests{selector}[5m])) * 100`,
				WarningThreshold: 95, CriticalThreshold: 90, Generator: SeriesSpec{Base: 98.5, Amplitude: 0.6, Noise: 0.3, Trend: -0.02}},
			{Name: "replication_lag_seconds", DisplayName: "主从延迟", Unit: "s", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `mysql_slave_status_seconds_behind_master{selector}`,
				WarningThreshold: 5, CriticalThreshold: 30, Generator: SeriesSpec{Base: 0.8, Amplitude: 1.2, Noise: 0.7, Trend: 0.05}},
		},
	},
	"pg": {
		MWType: "pg",
		Metrics: []MetricSpec{
			{Name: "qps", DisplayName: "QPS", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `rate(pg_stat_database_xact_commit{selector}[5m])`,
				WarningThreshold: 6000, CriticalThreshold: 12000, Generator: SeriesSpec{Base: 1800, Amplitude: 400, Noise: 160, Trend: 5}},
			{Name: "tps", DisplayName: "TPS", Unit: "", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(pg_stat_database_xact_rollback{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 60, Amplitude: 20, Noise: 12, Trend: 0.4}},
			{Name: "connections", DisplayName: "连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `pg_stat_activity_count{selector}`,
				WarningThreshold: 150, CriticalThreshold: 200, Generator: SeriesSpec{Base: 88, Amplitude: 18, Noise: 8, Trend: 0.5}},
			{Name: "slow_queries", DisplayName: "慢查询数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(pg_stat_statements_mean_time_seconds_count{selector}[5m])`,
				WarningThreshold: 5, CriticalThreshold: 30, Generator: SeriesSpec{Base: 0.9, Amplitude: 1.2, Noise: 0.8, Trend: 0.05}},
			{Name: "cache_hit_rate", DisplayName: "缓存命中率", Unit: "%", Category: "performance", Mode: ThresholdLowerWorse,
				Expr:             `pg_stat_database_blks_hit{selector} / (pg_stat_database_blks_hit{selector} + pg_stat_database_blks_read{selector}) * 100`,
				WarningThreshold: 95, CriticalThreshold: 90, Generator: SeriesSpec{Base: 99.1, Amplitude: 0.4, Noise: 0.2, Trend: -0.01}},
			{Name: "lock_waits", DisplayName: "锁等待", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `pg_locks_count{selector}`,
				WarningThreshold: 5, CriticalThreshold: 20, Generator: SeriesSpec{Base: 1.5, Amplitude: 2, Noise: 1.2, Trend: 0.04}},
		},
	},
	"es": {
		MWType: "es",
		Metrics: []MetricSpec{
			{Name: "cluster_status", DisplayName: "集群健康状态", Unit: "", Category: "reliability", Mode: ThresholdLowerWorse,
				Expr: `elasticsearch_cluster_health_status{selector}`,
				// ES 语义：2=green(正常) 1=yellow 0=red。含边界比较下，告警线须设在「低于正常值」：
				// value<=1 → yellow/red 至少 warning；value<=0 → red 判 critical；value=2 → ok。
				WarningThreshold: 1, CriticalThreshold: 0, Generator: SeriesSpec{Base: 2, Amplitude: 0, Noise: 0}},
			{Name: "node_count", DisplayName: "节点数", Unit: "", Category: "resource", Mode: ThresholdLowerWorse,
				Expr: `elasticsearch_cluster_health_number_of_nodes{selector}`,
				// 越低越差：≤1 个节点告警、0 个节点严重（此前误写为 3/1，会把 5 节点健康集群判为 warning）。
				WarningThreshold: 1, CriticalThreshold: 0, Generator: SeriesSpec{Base: 5, Amplitude: 0, Noise: 0}},
			{Name: "index_count", DisplayName: "索引数量", Unit: "", Category: "resource", Mode: ThresholdNone,
				Expr:             `elasticsearch_cluster_health_number_of_indices{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 240, Amplitude: 4, Noise: 2, Trend: 0.3}},
			{Name: "search_rate", DisplayName: "搜索速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(elasticsearch_indices_search_query_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 620, Amplitude: 120, Noise: 60, Trend: 2}},
			{Name: "indexing_rate", DisplayName: "索引速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(elasticsearch_indices_indexing_index_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 1400, Amplitude: 300, Noise: 120, Trend: 5}},
			{Name: "jvm_heap_used_percent", DisplayName: "JVM 堆使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `elasticsearch_jvm_memory_used_bytes{area="heap",selector} / elasticsearch_jvm_memory_max_bytes{area="heap",selector} * 100`,
				WarningThreshold: 75, CriticalThreshold: 85, Generator: SeriesSpec{Base: 68, Amplitude: 7, Noise: 4, Trend: 0.3}},
			{Name: "unassigned_shards", DisplayName: "未分配分片", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `elasticsearch_cluster_health_unassigned_shards{selector}`,
				WarningThreshold: 1, CriticalThreshold: 20, Generator: SeriesSpec{Base: 0, Amplitude: 1.2, Noise: 0.8}},
		},
	},
	"nginx": {
		MWType: "nginx",
		Metrics: []MetricSpec{
			{Name: "active_connections", DisplayName: "活跃连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `nginx_connections_active{selector}`,
				WarningThreshold: 3000, CriticalThreshold: 5000, Generator: SeriesSpec{Base: 1200, Amplitude: 260, Noise: 90, Trend: 4}},
			{Name: "request_rate", DisplayName: "请求速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(nginx_http_requests_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0, Generator: SeriesSpec{Base: 2600, Amplitude: 600, Noise: 200, Trend: 8}},
			{Name: "error_rate_5xx", DisplayName: "错误率(5xx)", Unit: "%", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(nginx_http_requests_total{status=~"5..",selector}[5m]) / rate(nginx_http_requests_total{selector}[5m]) * 100`,
				WarningThreshold: 0.5, CriticalThreshold: 2, Generator: SeriesSpec{Base: 0.12, Amplitude: 0.15, Noise: 0.1, Trend: 0.006}},
			{Name: "upstream_response_time", DisplayName: "响应时间(P95)", Unit: "ms", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `histogram_quantile(0.95, rate(nginx_upstream_response_time_bucket{selector}[5m]))`,
				WarningThreshold: 500, CriticalThreshold: 2000, Generator: SeriesSpec{Base: 180, Amplitude: 45, Noise: 30, Trend: 1.2}},
		},
	},
	"rabbitmq": {
		MWType: "rabbitmq",
		Metrics: []MetricSpec{
			{Name: "queue_messages", DisplayName: "队列消息数", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `sum(rabbitmq_queue_messages{selector})`,
				WarningThreshold: 50000, CriticalThreshold: 200000, Generator: SeriesSpec{Base: 8000, Amplitude: 3000, Noise: 1200, Trend: 60}},
			{Name: "consumers", DisplayName: "消费者数", Unit: "", Category: "resource", Mode: ThresholdLowerWorse,
				Expr:             `sum(rabbitmq_queue_consumers{selector})`,
				WarningThreshold: 1, CriticalThreshold: 0, Generator: SeriesSpec{Base: 12, Amplitude: 0, Noise: 0}},
			{Name: "memory_usage_percent", DisplayName: "内存使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `rabbitmq_process_resident_memory_bytes{selector} / rabbitmq_resident_memory_limit_bytes{selector} * 100`,
				WarningThreshold: 70, CriticalThreshold: 85, Generator: SeriesSpec{Base: 45, Amplitude: 6, Noise: 3, Trend: 0.2}},
		},
	},
}

// ProfileOf 返回中间件类型的指标画像，未知类型返回空画像。
func ProfileOf(mwType string) Profile {
	if p, ok := profiles[strings.ToLower(mwType)]; ok {
		return p
	}
	return Profile{MWType: mwType}
}

// SupportedTypes 返回支持监控的中间件类型列表。
func SupportedTypes() []string {
	out := make([]string, 0, len(profiles))
	for k := range profiles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// MetricNames 返回某类型的指标名集合（供告警规则下拉框）。
func MetricNames(mwType string) []string {
	p := ProfileOf(mwType)
	out := make([]string, 0, len(p.Metrics))
	for _, m := range p.Metrics {
		out = append(out, m.Name)
	}
	return out
}

// SpecOf 返回某类型下指定指标的规格。
func SpecOf(mwType, metric string) (MetricSpec, bool) {
	for _, m := range ProfileOf(mwType).Metrics {
		if m.Name == metric {
			return m, true
		}
	}
	return MetricSpec{}, false
}

// buildSelector 依据目标实例构造 PromQL 标签匹配串。
func buildSelector(t Target, jobPrefix string) string {
	parts := make([]string, 0, 3)
	job := t.Job
	if job == "" && jobPrefix != "" && t.MWType != "" {
		job = fmt.Sprintf("%s-%s", jobPrefix, t.MWType)
	}
	if job != "" {
		parts = append(parts, fmt.Sprintf(`job="%s"`, job))
	}
	if t.Instance != "" {
		parts = append(parts, fmt.Sprintf(`instance="%s"`, t.Instance))
	} else if t.Name != "" {
		parts = append(parts, fmt.Sprintf(`instance_name="%s"`, t.Name))
	}
	return strings.Join(parts, ",")
}
