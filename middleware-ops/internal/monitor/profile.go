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
}

// profiles 保存全部中间件画像。
var profiles = map[string]Profile{
	"redis": {
		MWType: "redis",
		// 指标顺序即统一监控页的下拉顺序；**第一条是切换实例后的默认指标**。
		Metrics: []MetricSpec{
			{Name: "redis_up", DisplayName: "服务可用", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `redis_up{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "memory_used_bytes", DisplayName: "已用内存", Unit: "B", Category: "resource", Mode: ThresholdNone,
				Expr:             `redis_memory_used_bytes{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "client_usage_percent", DisplayName: "客户端连接使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `redis_connected_clients{selector} / redis_config_maxclients{selector} * 100`,
				WarningThreshold: 70, CriticalThreshold: 85},
			{Name: "expired_keys", DisplayName: "过期 key 速率", Unit: "/s", Category: "resource", Mode: ThresholdNone,
				Expr:             `rate(redis_expired_keys_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "net_input_bytes", DisplayName: "入向流量", Unit: "B/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(redis_net_input_bytes_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "net_output_bytes", DisplayName: "出向流量", Unit: "B/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(redis_net_output_bytes_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "rejected_connections", DisplayName: "拒绝连接速率", Unit: "/s", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(redis_rejected_connections_total{selector}[5m])`,
				WarningThreshold: 1, CriticalThreshold: 10},
			{Name: "master_link_up", DisplayName: "主从链路", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `redis_master_link_up{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "rdb_last_bgsave_status", DisplayName: "RDB 持久化状态", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `redis_rdb_last_bgsave_status{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "uptime_seconds", DisplayName: "运行时长", Unit: "s", Category: "reliability", Mode: ThresholdNone,
				Expr:             `redis_uptime_in_seconds{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "memory_usage_percent", DisplayName: "内存使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `redis_memory_used_bytes{selector} / redis_memory_max_bytes{selector} * 100`,
				WarningThreshold: 70, CriticalThreshold: 85},
			{Name: "connected_clients", DisplayName: "连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `redis_connected_clients{selector}`,
				WarningThreshold: 800, CriticalThreshold: 1000},
			{Name: "instantaneous_ops_per_sec", DisplayName: "QPS", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `rate(redis_commands_processed_total{selector}[5m])`,
				WarningThreshold: 20000, CriticalThreshold: 40000},
			{Name: "keyspace_hit_rate", DisplayName: "命中率", Unit: "%", Category: "performance", Mode: ThresholdLowerWorse,
				Expr:             `rate(redis_keyspace_hits_total{selector}[5m]) / (rate(redis_keyspace_hits_total{selector}[5m]) + rate(redis_keyspace_misses_total{selector}[5m])) * 100`,
				WarningThreshold: 90, CriticalThreshold: 80},
			{Name: "db_keys", DisplayName: "key 数量", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `sum(redis_db_keys{selector})`,
				WarningThreshold: 5000000, CriticalThreshold: 10000000},
			{Name: "slowlog_length", DisplayName: "慢查询数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `redis_slowlog_length{selector}`,
				WarningThreshold: 10, CriticalThreshold: 50},
			{Name: "evicted_keys", DisplayName: "淘汰 key 数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(redis_evicted_keys_total{selector}[5m])`,
				WarningThreshold: 1, CriticalThreshold: 20},
			{Name: "blocked_clients", DisplayName: "阻塞客户端", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `redis_blocked_clients{selector}`,
				WarningThreshold: 1, CriticalThreshold: 5},
		},
	},
	"kafka": {
		MWType: "kafka",
		Metrics: []MetricSpec{
			{Name: "broker_up", DisplayName: "Broker 状态", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `up{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "partition_count", DisplayName: "Topic 分区数", Unit: "", Category: "resource", Mode: ThresholdNone,
				Expr:             `sum(kafka_topic_partitions{selector})`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "consumer_lag", DisplayName: "消费者组 Lag", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `sum(kafka_consumergroup_lag{selector})`,
				WarningThreshold: 10000, CriticalThreshold: 100000},
			{Name: "produce_rate", DisplayName: "生产速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `sum(rate(kafka_topic_partition_current_offset{selector}[5m]))`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "consume_rate", DisplayName: "消费速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `sum(rate(kafka_consumergroup_current_offset{selector}[5m]))`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "under_replicated_partitions", DisplayName: "ISR 副本不足分区", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `sum(kafka_topic_partition_under_replicated_partition{selector})`,
				WarningThreshold: 1, CriticalThreshold: 10},
		},
	},
	"mysql": {
		MWType: "mysql",
		// 指标顺序即统一监控页的下拉顺序；**第一条是切换实例后的默认指标**。
		Metrics: []MetricSpec{
			{Name: "mysql_up", DisplayName: "服务可用", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `mysql_up{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "threads_running", DisplayName: "运行中线程", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `mysql_global_status_threads_running{selector}`,
				WarningThreshold: 30, CriticalThreshold: 60},
			{Name: "connection_usage_percent", DisplayName: "连接使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `mysql_global_status_threads_connected{selector} / mysql_global_variables_max_connections{selector} * 100`,
				WarningThreshold: 70, CriticalThreshold: 85},
			{Name: "buffer_pool_usage_percent", DisplayName: "缓冲池使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `mysql_global_status_innodb_buffer_pool_bytes_data{selector} / mysql_global_variables_innodb_buffer_pool_size{selector} * 100`,
				WarningThreshold: 85, CriticalThreshold: 95},
			{Name: "aborted_connects", DisplayName: "连接失败速率", Unit: "/s", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(mysql_global_status_aborted_connects{selector}[5m])`,
				WarningThreshold: 1, CriticalThreshold: 5},
			{Name: "row_lock_waits", DisplayName: "行锁等待速率", Unit: "/s", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(mysql_global_status_innodb_row_lock_waits{selector}[5m])`,
				WarningThreshold: 1, CriticalThreshold: 10},
			{Name: "tmp_disk_tables", DisplayName: "磁盘临时表速率", Unit: "/s", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `rate(mysql_global_status_created_tmp_disk_tables{selector}[5m])`,
				WarningThreshold: 5, CriticalThreshold: 50},
			{Name: "replication_io_running", DisplayName: "复制 IO 线程", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `mysql_slave_status_slave_io_running{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "replication_sql_running", DisplayName: "复制 SQL 线程", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `mysql_slave_status_slave_sql_running{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "uptime_seconds", DisplayName: "运行时长", Unit: "s", Category: "reliability", Mode: ThresholdNone,
				Expr:             `mysql_global_status_uptime{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "qps", DisplayName: "QPS", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `rate(mysql_global_status_queries{selector}[5m])`,
				WarningThreshold: 8000, CriticalThreshold: 15000},
			{Name: "tps", DisplayName: "TPS", Unit: "", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(mysql_global_status_questions{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "threads_connected", DisplayName: "连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `mysql_global_status_threads_connected{selector}`,
				WarningThreshold: 400, CriticalThreshold: 500},
			{Name: "slow_queries", DisplayName: "慢查询数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(mysql_global_status_slow_queries{selector}[5m])`,
				WarningThreshold: 5, CriticalThreshold: 30},
			{Name: "buffer_pool_hit_rate", DisplayName: "缓冲池命中率", Unit: "%", Category: "performance", Mode: ThresholdLowerWorse,
				Expr:             `(1 - rate(mysql_global_status_innodb_buffer_pool_reads{selector}[5m]) / rate(mysql_global_status_innodb_buffer_pool_read_requests{selector}[5m])) * 100`,
				WarningThreshold: 95, CriticalThreshold: 90},
			{Name: "replication_lag_seconds", DisplayName: "主从延迟", Unit: "s", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `mysql_slave_status_seconds_behind_master{selector}`,
				WarningThreshold: 5, CriticalThreshold: 30},
		},
	},
	"pg": {
		MWType: "pg",
		Metrics: []MetricSpec{
			{Name: "qps", DisplayName: "QPS", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `rate(pg_stat_database_xact_commit{selector}[5m])`,
				WarningThreshold: 6000, CriticalThreshold: 12000},
			{Name: "tps", DisplayName: "TPS", Unit: "", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(pg_stat_database_xact_rollback{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "connections", DisplayName: "连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `pg_stat_activity_count{selector}`,
				WarningThreshold: 150, CriticalThreshold: 200},
			{Name: "slow_queries", DisplayName: "慢查询数", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(pg_stat_statements_mean_time_seconds_count{selector}[5m])`,
				WarningThreshold: 5, CriticalThreshold: 30},
			{Name: "cache_hit_rate", DisplayName: "缓存命中率", Unit: "%", Category: "performance", Mode: ThresholdLowerWorse,
				Expr:             `pg_stat_database_blks_hit{selector} / (pg_stat_database_blks_hit{selector} + pg_stat_database_blks_read{selector}) * 100`,
				WarningThreshold: 95, CriticalThreshold: 90},
			{Name: "lock_waits", DisplayName: "锁等待", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `pg_locks_count{selector}`,
				WarningThreshold: 5, CriticalThreshold: 20},
			// 主从延迟：postgres_exporter 的 replication 采集器暴露 pg_replication_lag（秒）。
			// 这个指标此前**只在推荐告警里被引用、画像里却没有**，于是"PostgreSQL 主从延迟"这条
			// 推荐规则永远不会触发（由 TestTemplateAlertMetricsExistInProfiles 抓出）。
			// 非从库实例不会有该时序，因此不会误报。
			{Name: "replication_lag_seconds", DisplayName: "主从延迟", Unit: "s", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `pg_replication_lag{selector}`,
				WarningThreshold: 5, CriticalThreshold: 30},
		},
	},
	"es": {
		MWType: "es",
		Metrics: []MetricSpec{
			{Name: "cluster_status", DisplayName: "集群健康状态", Unit: "", Category: "reliability", Mode: ThresholdLowerWorse,
				Expr: `elasticsearch_cluster_health_status{selector}`,
				// ES 语义：2=green(正常) 1=yellow 0=red。含边界比较下，告警线须设在「低于正常值」：
				// value<=1 → yellow/red 至少 warning；value<=0 → red 判 critical；value=2 → ok。
				WarningThreshold: 1, CriticalThreshold: 0},
			{Name: "node_count", DisplayName: "节点数", Unit: "", Category: "resource", Mode: ThresholdLowerWorse,
				Expr: `elasticsearch_cluster_health_number_of_nodes{selector}`,
				// 越低越差：≤1 个节点告警、0 个节点严重（此前误写为 3/1，会把 5 节点健康集群判为 warning）。
				WarningThreshold: 1, CriticalThreshold: 0},
			{Name: "index_count", DisplayName: "索引数量", Unit: "", Category: "resource", Mode: ThresholdNone,
				Expr:             `elasticsearch_cluster_health_number_of_indices{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "search_rate", DisplayName: "搜索速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(elasticsearch_indices_search_query_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "indexing_rate", DisplayName: "索引速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(elasticsearch_indices_indexing_index_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "jvm_heap_used_percent", DisplayName: "JVM 堆使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `elasticsearch_jvm_memory_used_bytes{area="heap",selector} / elasticsearch_jvm_memory_max_bytes{area="heap",selector} * 100`,
				WarningThreshold: 75, CriticalThreshold: 85},
			{Name: "unassigned_shards", DisplayName: "未分配分片", Unit: "", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `elasticsearch_cluster_health_unassigned_shards{selector}`,
				WarningThreshold: 1, CriticalThreshold: 20},
		},
	},
	"nginx": {
		MWType: "nginx",
		Metrics: []MetricSpec{
			{Name: "active_connections", DisplayName: "活跃连接数", Unit: "", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `nginx_connections_active{selector}`,
				WarningThreshold: 3000, CriticalThreshold: 5000},
			{Name: "request_rate", DisplayName: "请求速率", Unit: "/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `rate(nginx_http_requests_total{selector}[5m])`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "error_rate_5xx", DisplayName: "错误率(5xx)", Unit: "%", Category: "reliability", Mode: ThresholdHigherWorse,
				Expr:             `rate(nginx_http_requests_total{status=~"5..",selector}[5m]) / rate(nginx_http_requests_total{selector}[5m]) * 100`,
				WarningThreshold: 0.5, CriticalThreshold: 2},
			{Name: "upstream_response_time", DisplayName: "响应时间(P95)", Unit: "ms", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `histogram_quantile(0.95, rate(nginx_upstream_response_time_bucket{selector}[5m]))`,
				WarningThreshold: 500, CriticalThreshold: 2000},
		},
	},
	"rabbitmq": {
		MWType: "rabbitmq",
		Metrics: []MetricSpec{
			{Name: "queue_messages", DisplayName: "队列消息数", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `sum(rabbitmq_queue_messages{selector})`,
				WarningThreshold: 50000, CriticalThreshold: 200000},
			{Name: "consumers", DisplayName: "消费者数", Unit: "", Category: "resource", Mode: ThresholdLowerWorse,
				Expr:             `sum(rabbitmq_queue_consumers{selector})`,
				WarningThreshold: 1, CriticalThreshold: 0},
			{Name: "memory_usage_percent", DisplayName: "内存使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr:             `rabbitmq_process_resident_memory_bytes{selector} / rabbitmq_resident_memory_limit_bytes{selector} * 100`,
				WarningThreshold: 70, CriticalThreshold: 85},
		},
	},
	// 主机监控（node_exporter）：采集对象是服务器本身，不是中间件实例。
	// 指标名与告警模板（internal/integration/template.go 的 node 模板）一一对应。
	"node": {
		MWType: "node",
		Metrics: []MetricSpec{
			{Name: "host_up", DisplayName: "主机可达", Unit: "", Category: "reliability", Mode: ThresholdBoolDown,
				Expr:             `up{selector}`,
				WarningThreshold: 0, CriticalThreshold: 0},
			{Name: "cpu_usage_percent", DisplayName: "CPU 使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr: `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) by (instance_name) * 100)`,
				WarningThreshold: 80, CriticalThreshold: 92},
			{Name: "memory_usage_percent", DisplayName: "内存使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr: `(1 - node_memory_MemAvailable_bytes{selector} / node_memory_MemTotal_bytes{selector}) * 100`,
				WarningThreshold: 85, CriticalThreshold: 93},
			{Name: "disk_usage_percent", DisplayName: "磁盘使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr: `max(100 - (node_filesystem_avail_bytes{selector, fstype!~"tmpfs|overlay"} / node_filesystem_size_bytes{selector, fstype!~"tmpfs|overlay"} * 100))`,
				WarningThreshold: 85, CriticalThreshold: 93},
			{Name: "load1", DisplayName: "1 分钟负载", Unit: "", Category: "performance", Mode: ThresholdHigherWorse,
				Expr:             `node_load1{selector}`,
				WarningThreshold: 8, CriticalThreshold: 16},
			{Name: "filesystem_inodes_used_percent", DisplayName: "inode 使用率", Unit: "%", Category: "resource", Mode: ThresholdHigherWorse,
				Expr: `max(100 - (node_filesystem_files_free{selector, fstype!~"tmpfs|overlay"} / node_filesystem_files{selector, fstype!~"tmpfs|overlay"} * 100))`,
				WarningThreshold: 85, CriticalThreshold: 95},
			{Name: "network_receive_bytes_rate", DisplayName: "入向流量", Unit: "B/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `sum(rate(node_network_receive_bytes_total{selector, device!="lo"}[5m]))`,
				WarningThreshold: 80 * 1024 * 1024, CriticalThreshold: 200 * 1024 * 1024},
			{Name: "network_transmit_bytes_rate", DisplayName: "出向流量", Unit: "B/s", Category: "performance", Mode: ThresholdNone,
				Expr:             `sum(rate(node_network_transmit_bytes_total{selector, device!="lo"}[5m]))`,
				WarningThreshold: 80 * 1024 * 1024, CriticalThreshold: 200 * 1024 * 1024},
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

// jobOf 计算实例查询使用的 job 标签值。
//
// 纳管时填了 prom_job 就以它为准；否则按 jobPrefix + "-" + 中间件类型 兜底
// （如 middleware-exporter-redis），与 deploy/prometheus/ 下的 job 命名约定一致。
func jobOf(t Target, jobPrefix string) string {
	if t.Job != "" {
		return t.Job
	}
	if jobPrefix != "" && t.MWType != "" {
		return fmt.Sprintf("%s-%s", jobPrefix, t.MWType)
	}
	return ""
}

// buildSelector 依据目标实例构造 PromQL 标签匹配串。
//
// 匹配优先级（与纳管表单的字段一一对应）：
//  1. job：prom_job，为空时按 <前缀>-<类型> 兜底；
//  2. 实例：prom_instance 非空 → instance="..."；否则 → instance_name="<实例名>"。
//
// 注意 2 是「二选一」而不是「都要满足」：一旦填了 prom_instance，实例名就不再参与
// 匹配。这是接入时最容易踩的坑（自建 Exporter 场景只上报 instance_name，
// 把 instance 填成 IP:端口 就永远查不到数据），因此自检接口会显式提示。
func buildSelector(t Target, jobPrefix string) string {
	parts := make([]string, 0, 3)
	if job := jobOf(t, jobPrefix); job != "" {
		parts = append(parts, fmt.Sprintf(`job="%s"`, job))
	}
	if t.Instance != "" {
		parts = append(parts, fmt.Sprintf(`instance="%s"`, t.Instance))
	} else if t.Name != "" {
		parts = append(parts, fmt.Sprintf(`instance_name="%s"`, t.Name))
	}
	return strings.Join(parts, ",")
}

// SelectorFor 返回实例在 Prometheus 中的标签匹配串（供接入自检与排障核对）。
func SelectorFor(target Target, jobPrefix string) string {
	return buildSelector(target, jobPrefix)
}
