// Package integration 实现「集成中心」的组件模板、参数校验与采集配置渲染。
//
// 设计参照腾讯云 Prometheus 监控服务的「数据采集 → 集成中心」：
// 在页面上选择组件（Redis / MySQL / PostgreSQL / Kafka / Elasticsearch / Nginx），
// 填写名称、地址、账号口令、自定义标签与 Exporter 参数，保存后由平台自动完成
// 「Exporter 暴露 + Prometheus 抓取 + 实例纳管 + 告警规则」四件事。
//
// 本包刻意只做纯计算：模板定义、入参校验、env/args 渲染、抓取配置与 compose
// 片段生成。它不访问数据库、不调用 Docker、不写文件，因此可以被单元测试完全
// 覆盖，也便于把同样的产物复制到别的 Prometheus 部署里。
package integration

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// 组件类型
// ---------------------------------------------------------------------------

// 支持的组件类型（与 internal/monitor 的指标画像键一致）。
const (
	TypeRedis = "redis"
	TypeMySQL = "mysql"
	TypePG    = "pg"
	TypeKafka = "kafka"
	TypeES    = "es"
	TypeNginx = "nginx"
)

// OptionTarget 描述 Exporter 参数的落地形式。
type OptionTarget string

const (
	// TargetEnv 表示参数以环境变量注入（如 REDIS_EXPORTER_EXCLUDE_SLOWLOG_METRICS）。
	TargetEnv OptionTarget = "env"
	// TargetArg 表示参数以命令行开关注入（如 --collect.global_status）。
	TargetArg OptionTarget = "arg"
)

// Option 是模板暴露给使用者的 Exporter 参数。
//
// 对应腾讯云控制台的「环境变量」（redis）与「Exporter 配置」（mysql）两栏：
// 同一个概念在本平台统一成一张参数表，用 Target 决定落地形态。
type Option struct {
	Key    string       `json:"key"`
	Label  string       `json:"label"`
	Target OptionTarget `json:"target"`
	// Kind 取值 bool / string / number，决定前端控件与渲染规则。
	Kind    string `json:"kind"`
	Default string `json:"default"`
	Help    string `json:"help"`
}

// AlertTemplate 是集成后自动创建的推荐告警规则。
type AlertTemplate struct {
	Name        string  `json:"name"`
	MetricName  string  `json:"metric_name"`
	Operator    string  `json:"operator"`
	Threshold   float64 `json:"threshold"`
	Level       string  `json:"level"`
	TimeWindow  int     `json:"time_window"`
	Cooldown    int     `json:"cooldown"`
	Description string  `json:"description"`
}

// Dashboard 指向该组件对应的 Grafana 大盘（开箱即用大盘，与文档一致）。
type Dashboard struct {
	Title string `json:"title"`
	// ID 为 grafana.com 大盘编号，可在 Grafana 里按编号导入。
	ID string `json:"id"`
}

// Template 是一个组件的集成模板。
type Template struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Component   string `json:"component"`
	Description string `json:"description"`
	// Phase 1 表示支持「纳管 + 监控 + 告警 + AI 诊断」，2 表示仅纳管。
	Phase int `json:"phase"`
	// Image 为官方 Exporter 镜像；平台可据此一键拉起容器。
	Image string `json:"image"`
	// ExporterPort 为 Exporter 自身的监听端口（Prometheus 抓取目标）。
	ExporterPort int `json:"exporter_port"`
	// DefaultPort 为被管中间件的默认端口（用于地址补全）。
	DefaultPort int    `json:"default_port"`
	MetricsPath string `json:"metrics_path"`
	// NeedsAuth 表示该组件通常需要账号口令。
	NeedsAuth bool `json:"needs_auth"`
	// MonitorUser 表示该组件支持**由平台代建只读监控账号**，并给出默认账号名。
	// 为空表示不需要账号（如 Redis：口令由目标自身鉴权决定），或不支持代建。
	MonitorUser string `json:"monitor_user"`
	// AddressLabel / AddressHint 用于前端表单文案。
	AddressLabel string `json:"address_label"`
	AddressHint  string `json:"address_hint"`
	// AddressIsURL 为 true 时地址按 URL 处理（可含 scheme 与 path）。
	AddressIsURL bool `json:"address_is_url"`
	// URLScheme / URLPath 为 URL 形态地址的默认值。
	URLScheme string   `json:"url_scheme"`
	URLPath   string   `json:"url_path"`
	Options   []Option `json:"options"`
	// Notes 是与该组件相关的注意事项（来自官方/云厂商文档的踩坑点）。
	Notes []string `json:"notes"`
	// Docs 为参考文档链接。
	Docs      []string        `json:"docs"`
	Alerts    []AlertTemplate `json:"alerts"`
	Dashboard Dashboard       `json:"dashboard"`
}

// ---------------------------------------------------------------------------
// 模板注册表
// ---------------------------------------------------------------------------

// templates 是内置组件模板表。
//
// 镜像版本与文档侧保持一致：redis_exporter 使用支持 REDIS_EXPORTER_* 环境变量
// 的版本；mysqld_exporter 使用 v0.15.x（DATA_SOURCE_NAME + --collect.* 开关）。
var templates = map[string]Template{
	TypeRedis: {
		Type: TypeRedis, Name: "Redis", Component: "redis_exporter",
		Description:  "开源 Redis / Valkey 内存数据库指标暴露（内存、连接、命中率、淘汰、慢查询）",
		Phase:        1,
		Image:        "oliver006/redis_exporter:v1.66.0",
		ExporterPort: 9121, DefaultPort: 6379, MetricsPath: "/metrics",
		NeedsAuth:    true,
		AddressLabel: "连接地址", AddressHint: "如 10.0.0.11:6379；也可写完整 redis:// 前缀",
		Options: []Option{
			{Key: "REDIS_EXPORTER_EXCLUDE_SLOWLOG_METRICS", Label: "跳过 SLOWLOG 指标", Target: TargetEnv, Kind: "bool",
				Help: "云数据库 Redis 集群架构不支持 SLOWLOG 命令，开启后不再采集慢日志指标"},
			{Key: "REDIS_EXPORTER_EXCLUDE_LATENCY_HISTOGRAM_METRICS", Label: "跳过 LATENCY HISTOGRAM 指标", Target: TargetEnv, Kind: "bool",
				Help: "集群架构或 Redis < 7 不支持 LATENCY HISTOGRAM，开启以避免告警噪音"},
			{Key: "REDIS_EXPORTER_CHECK_KEYS", Label: "采集指定 key", Target: TargetEnv, Kind: "string",
				Help: "形如 db0=session:*，多个用逗号分隔；key 很多时会影响抓取耗时"},
			{Key: "REDIS_EXPORTER_APPEND_INSTANCE_ROLE_LABEL", Label: "附加 master/replica 标签", Target: TargetEnv, Kind: "bool",
				Help: "为指标附加 instance_role 标签，便于区分主从（会增加序列基数）"},
		},
		Notes: []string{
			"REDIS_ADDR 形如 redis://<host>:<port>；账号密码分别用 REDIS_USER / REDIS_PASSWORD 注入，不要写进 URL。",
			"redis_exporter 没有 SERVICE_NAME 之类的实例名开关，instance_name 由平台写入 Prometheus 抓取标签（file_sd）。",
			"开启 requirepass 的实例务必填写密码，否则 Exporter 侧 redis_up=0。",
		},
		Docs: []string{
			"https://cloud.tencent.com/document/product/1416/111839",
			"https://github.com/oliver006/redis_exporter",
		},
		Alerts: []AlertTemplate{
			{Name: "Redis 内存使用率过高", MetricName: "memory_usage_percent", Operator: ">", Threshold: 85,
				Level: "critical", TimeWindow: 5, Cooldown: 10, Description: "存在触发 maxmemory 淘汰策略的风险"},
			{Name: "Redis 命中率过低", MetricName: "keyspace_hit_rate", Operator: "<", Threshold: 90,
				Level: "warning", TimeWindow: 10, Cooldown: 15, Description: "缓存穿透压力可能传导至下游数据库"},
			{Name: "Redis 出现 key 淘汰", MetricName: "evicted_keys", Operator: ">", Threshold: 0,
				Level: "warning", TimeWindow: 5, Cooldown: 10, Description: "内存不足已开始淘汰数据，可能影响业务命中"},
		},
		Dashboard: Dashboard{Title: "Redis Dashboard for Prometheus Redis Exporter", ID: "763"},
	},
	TypeMySQL: {
		Type: TypeMySQL, Name: "MySQL", Component: "mysqld_exporter",
		Description:  "MySQL / MariaDB 指标暴露（连接、QPS、慢查询、缓冲池、主从延迟）",
		Phase:        1,
		Image:        "prom/mysqld-exporter:v0.15.1",
		ExporterPort: 9104, DefaultPort: 3306, MetricsPath: "/metrics",
		NeedsAuth:    true,
		MonitorUser:  "mwops_exporter",
		AddressLabel: "连接地址", AddressHint: "如 10.0.0.12:3306",
		Options: []Option{
			{Key: "collect.global_status", Label: "global_status", Target: TargetArg, Kind: "bool", Default: "true",
				Help: "从 SHOW GLOBAL STATUS 采集（默认开启）"},
			{Key: "collect.global_variables", Label: "global_variables", Target: TargetArg, Kind: "bool", Default: "true",
				Help: "从 SHOW GLOBAL VARIABLES 采集（默认开启）"},
			{Key: "collect.info_schema.tables", Label: "info_schema.tables", Target: TargetArg, Kind: "bool", Default: "true",
				Help: "从 information_schema.tables 采集表统计（默认开启；库表很多时抓取会变慢，可关闭）"},
			{Key: "collect.info_schema.innodb_metrics", Label: "info_schema.innodb_metrics", Target: TargetArg, Kind: "bool",
				Help: "从 information_schema.innodb_metrics 采集指标"},
			{Key: "collect.perf_schema.eventsstatements", Label: "perf_schema.eventsstatements", Target: TargetArg, Kind: "bool",
				Help: "从 performance_schema.events_statements_summary_by_digest 采集语句级指标"},
			{Key: "collect.slave_status", Label: "slave_status", Target: TargetArg, Kind: "bool", Default: "true",
				Help: "从 SHOW SLAVE STATUS 采集主从信息（默认开启）"},
			{Key: "collect.auto_increment.columns", Label: "auto_increment.columns", Target: TargetArg, Kind: "bool",
				Help: "采集自增列与最大值，用于容量预警"},
			{Key: "collect.binlog_size", Label: "binlog_size", Target: TargetArg, Kind: "bool",
				Help: "采集已注册 binlog 文件的当前大小"},
		},
		Notes: []string{
			"需要预先创建只读监控账号并授权：CREATE USER 'exporter'@'%' IDENTIFIED BY '...' WITH MAX_USER_CONNECTIONS 3; " +
				"GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'exporter'@'%';（MAX_USER_CONNECTIONS 避免高频抓取压垮实例）",
			"凭据走官方方式：--mysqld.username + MYSQLD_EXPORTER_PASSWORD + --mysqld.address，" +
				"**不拼 DATA_SOURCE_NAME** —— 因此口令含 @ ( ) / : ? 也不会出现 invalid DSN（那类 up=0 从此消失）。",
			"MySQL 低于 5.6 / MariaDB 低于 10.1 时部分指标采集不到，属预期现象。",
			"采集开关是 mysqld_exporter 的命令行参数（--collect.*），不是环境变量；上游默认开启的项被关闭时输出 --no-<flag>。",
		},
		Docs: []string{
			"https://cloud.tencent.com/document/product/1416/111841",
			"https://github.com/prometheus/mysqld_exporter",
		},
		Alerts: []AlertTemplate{
			{Name: "MySQL 连接数接近上限", MetricName: "threads_connected", Operator: ">", Threshold: 400,
				Level: "critical", TimeWindow: 5, Cooldown: 10, Description: "连接池接近 max_connections，新连接可能被拒"},
			{Name: "MySQL 慢查询升高", MetricName: "slow_queries", Operator: ">", Threshold: 5,
				Level: "warning", TimeWindow: 5, Cooldown: 10, Description: "慢查询速率升高，检查索引与锁等待"},
		},
		Dashboard: Dashboard{Title: "MySQL Overview", ID: "7362"},
	},
	TypePG: {
		Type: TypePG, Name: "PostgreSQL", Component: "postgres_exporter",
		Description:  "PostgreSQL 指标暴露（连接、事务、缓存命中、锁等待、复制延迟）",
		Phase:        1,
		Image:        "prometheuscommunity/postgres-exporter:v0.16.0",
		ExporterPort: 9187, DefaultPort: 5432, MetricsPath: "/metrics",
		NeedsAuth:    true,
		MonitorUser:  "mwops_exporter",
		AddressLabel: "连接地址", AddressHint: "如 10.0.0.13:5432",
		Options: []Option{
			{Key: "auto-discover-databases", Label: "自动发现所有库", Target: TargetArg, Kind: "bool",
				Help: "为每个数据库生成独立指标，实例库很多时序列会显著增加"},
			{Key: "disable-settings-metrics", Label: "关闭 settings 指标", Target: TargetArg, Kind: "bool",
				Help: "不采集 pg_settings（暴露配置项，敏感环境可关闭）"},
			{Key: "extend.query-path", Label: "自定义查询文件", Target: TargetArg, Kind: "string",
				Help: "挂载到容器内的 queries.yaml 路径，用于采集自定义指标"},
		},
		Notes: []string{
			"需要预建账号并授权：CREATE USER exporter WITH PASSWORD '...'; GRANT pg_monitor TO exporter;",
			"连接串通过 DATA_SOURCE_NAME 注入：postgresql://user:pass@host:port/postgres?sslmode=disable",
		},
		Docs: []string{"https://github.com/prometheus-community/postgres_exporter"},
		Alerts: []AlertTemplate{
			{Name: "PostgreSQL 连接数偏高", MetricName: "connections", Operator: ">", Threshold: 150,
				Level: "warning", TimeWindow: 5, Cooldown: 10, Description: "连接数接近上限"},
			{Name: "PostgreSQL 主从延迟", MetricName: "replication_lag_seconds", Operator: ">", Threshold: 30,
				Level: "critical", TimeWindow: 5, Cooldown: 10, Description: "从库回放延迟超过 30 秒"},
		},
		Dashboard: Dashboard{Title: "PostgreSQL Database", ID: "9628"},
	},
	TypeKafka: {
		Type: TypeKafka, Name: "Kafka", Component: "kafka_exporter",
		Description:  "Kafka 指标暴露（Broker、Topic 分区、消费者组 Lag）",
		Phase:        1,
		Image:        "danielqsj/kafka-exporter:v1.7.0",
		ExporterPort: 9308, DefaultPort: 9092, MetricsPath: "/metrics",
		NeedsAuth:    false,
		AddressLabel: "Broker 地址", AddressHint: "如 10.0.0.14:9092；多 Broker 用逗号分隔",
		Options: []Option{
			{Key: "topic.filter", Label: "Topic 过滤正则", Target: TargetArg, Kind: "string",
				Help: "只采集匹配的 topic，降低序列基数"},
			{Key: "group.filter", Label: "消费组过滤正则", Target: TargetArg, Kind: "string",
				Help: "只采集匹配的消费组"},
			{Key: "kafka.labels", Label: "附加 kafka 标签", Target: TargetArg, Kind: "string",
				Help: "附加到所有 kafka_* 指标的标签，形如 k1=v1,k2=v2"},
		},
		Notes: []string{
			"kafka_exporter 不提供 instance_name 标签，实例名由平台写入抓取标签。",
			"开启 SASL/ACL 的集群需要额外参数（--sasl.enabled 等），当前模板未覆盖，请用自定义 compose 片段。",
		},
		Docs: []string{"https://github.com/danielqsj/kafka_exporter"},
		Alerts: []AlertTemplate{
			{Name: "Kafka 消费积压", MetricName: "consumer_lag", Operator: ">", Threshold: 10000,
				Level: "critical", TimeWindow: 5, Cooldown: 10, Description: "消费速率低于生产速率，积压持续增长"},
			{Name: "Kafka ISR 副本不足", MetricName: "under_replicated_partitions", Operator: ">", Threshold: 0,
				Level: "critical", TimeWindow: 3, Cooldown: 10, Description: "存在副本不足分区，写入可靠性下降"},
		},
		Dashboard: Dashboard{Title: "Kafka Exporter Overview", ID: "7589"},
	},
	TypeES: {
		Type: TypeES, Name: "Elasticsearch", Component: "elasticsearch_exporter",
		Description:  "Elasticsearch 指标暴露（集群健康、分片、JVM 堆、检索与索引速率）",
		Phase:        1,
		Image:        "prometheuscommunity/elasticsearch-exporter:v1.7.0",
		ExporterPort: 9114, DefaultPort: 9200, MetricsPath: "/metrics",
		NeedsAuth:    true,
		AddressLabel: "集群地址", AddressHint: "如 http://10.0.0.15:9200",
		AddressIsURL: true, URLScheme: "http",
		Options: []Option{
			{Key: "es.all", Label: "采集所有节点", Target: TargetArg, Kind: "bool",
				Help: "采集每个节点的指标，而不仅是选中的节点"},
			{Key: "es.indices", Label: "采集索引指标", Target: TargetArg, Kind: "bool",
				Help: "索引数量很多时序列会显著增加"},
			{Key: "es.snapshots", Label: "采集快照指标", Target: TargetArg, Kind: "bool",
				Help: "从 snapshot stats API 采集快照状态"},
			{Key: "es.cluster_settings", Label: "采集集群设置", Target: TargetArg, Kind: "bool",
				Help: "采集集群级别的配置项"},
		},
		Notes: []string{
			"地址需带 scheme（http/https），HTTPS 自签证书场景需另行挂载 CA。",
			"账号口令通过 --es.username / --es.password 传入。",
		},
		Docs: []string{"https://github.com/prometheus-community/elasticsearch_exporter"},
		Alerts: []AlertTemplate{
			{Name: "Elasticsearch 集群非 green", MetricName: "cluster_status", Operator: "<", Threshold: 2,
				Level: "critical", TimeWindow: 5, Cooldown: 10, Description: "集群处于 yellow/red，分片分配异常"},
			{Name: "Elasticsearch JVM 堆偏高", MetricName: "jvm_heap_used_percent", Operator: ">", Threshold: 85,
				Level: "warning", TimeWindow: 5, Cooldown: 10, Description: "堆使用率过高，Full GC 风险升高"},
		},
		Dashboard: Dashboard{Title: "Elasticsearch Exporter", ID: "2322"},
	},
	TypeNginx: {
		Type: TypeNginx, Name: "Nginx", Component: "nginx-prometheus-exporter",
		Description:  "Nginx 指标暴露（连接数、请求速率、5xx 错误率、upstream 响应时间）",
		Phase:        1,
		Image:        "nginx/nginx-prometheus-exporter:1.3.0",
		ExporterPort: 9113, DefaultPort: 80, MetricsPath: "/metrics",
		NeedsAuth:    false,
		AddressLabel: "stub_status 地址", AddressHint: "如 http://10.0.0.16:80/stub_status",
		AddressIsURL: true, URLScheme: "http", URLPath: "/stub_status",
		Options: []Option{
			{Key: "nginx.ssl-verify", Label: "校验 TLS 证书", Target: TargetArg, Kind: "bool", Default: "true",
				Help: "抓取 HTTPS stub_status 时校验证书"},
		},
		Notes: []string{
			"被管 Nginx 必须开启 stub_status 并放通 Exporter 访问：",
			"location /stub_status { stub_status; allow <exporter-ip>; deny all; }",
		},
		Docs: []string{"https://github.com/nginxinc/nginx-prometheus-exporter"},
		Alerts: []AlertTemplate{
			{Name: "Nginx 5xx 错误率过高", MetricName: "error_rate_5xx", Operator: ">", Threshold: 2,
				Level: "critical", TimeWindow: 3, Cooldown: 10, Description: "上游异常，检查 upstream 健康状态"},
		},
		Dashboard: Dashboard{Title: "NGINX Prometheus Exporter", ID: "9614"},
	},
}

// Templates 返回全部模板（按类型名排序，供前端渲染集成中心卡片）。
func Templates() []Template {
	out := make([]Template, 0, len(templates))
	for _, tpl := range templates {
		out = append(out, tpl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// TemplateOf 返回指定组件模板。
func TemplateOf(mwType string) (Template, bool) {
	tpl, ok := templates[strings.ToLower(strings.TrimSpace(mwType))]
	return tpl, ok
}

// SupportedTypes 返回可集成的组件类型。
func SupportedTypes() []string {
	out := make([]string, 0, len(templates))
	for key := range templates {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// ---------------------------------------------------------------------------
// 地址解析
// ---------------------------------------------------------------------------

// Address 是解析后的被管实例地址。
type Address struct {
	Scheme string
	Host   string
	Port   int
	Path   string
	Raw    string
}

// HostPort 返回 Prometheus 抓取与 TCP 探测使用的 host:port。
func (a Address) HostPort() string {
	return net.JoinHostPort(a.Host, strconv.Itoa(a.Port))
}

// URL 返回带 scheme 的访问地址（未指定 scheme 时使用 fallback）。
func (a Address) URL(fallbackScheme string) string {
	scheme := a.Scheme
	if scheme == "" {
		scheme = fallbackScheme
	}
	if scheme == "" {
		return a.HostPort()
	}
	out := scheme + "://" + a.HostPort()
	if a.Path != "" {
		out += a.Path
	}
	return out
}

// ParseAddress 解析集成表单里的「地址」字段。
//
// 支持三种写法（与云厂商控制台的填写习惯一致）：
//
//	10.0.0.11:6379           → host=10.0.0.11 port=6379
//	10.0.0.12                → 端口用模板默认值补全
//	http://10.0.0.13:9200/x  → 保留 scheme 与 path
func ParseAddress(raw string, defaultPort int, defaultScheme, defaultPath string) (Address, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Address{}, fmt.Errorf("连接地址不能为空")
	}
	// 补全 scheme 后再走标准解析，避免手写字符串切分漏掉 path/带认证的 URL。
	candidate := trimmed
	if !strings.Contains(candidate, "://") {
		candidate = "//" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return Address{}, fmt.Errorf("连接地址 %q 无法解析: %w", raw, err)
	}
	host := parsed.Hostname()
	if host == "" {
		return Address{}, fmt.Errorf("连接地址 %q 缺少主机名", raw)
	}
	port := defaultPort
	if parsed.Port() != "" {
		value, convErr := strconv.Atoi(parsed.Port())
		if convErr != nil || value <= 0 || value > 65535 {
			return Address{}, fmt.Errorf("连接地址 %q 的端口非法", raw)
		}
		port = value
	}
	if port <= 0 {
		return Address{}, fmt.Errorf("连接地址 %q 未指定端口，且该组件没有默认端口", raw)
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = defaultScheme
	}
	path := strings.TrimRight(parsed.Path, "/")
	if path == "" {
		path = defaultPath
	}
	return Address{Scheme: scheme, Host: host, Port: port, Path: path, Raw: trimmed}, nil
}

// ---------------------------------------------------------------------------
// 输入与校验
// ---------------------------------------------------------------------------

// Instance 是一次集成的渲染输入（来自集成表单）。
type Instance struct {
	// Name 为集成名称，同时作为纳管实例名与 instance_name 标签值。
	Name     string
	MWType   string
	Address  Address
	Username string
	Password string
	// Labels 是自定义指标标签（对应控制台的「标签」栏）。
	Labels map[string]string
	// Options 是 Exporter 参数值（key 取自模板的 Options）。
	Options map[string]string

	Environment string
	GroupName   string
}

// namePattern 与云厂商控制台的集成名称规范一致：
// 小写字母/数字开头结尾，可用中划线连接，可用点分段；保证能直接作为
// Prometheus 标签值与容器名使用。
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)

// reservedLabelNames 是不允许被自定义标签覆盖的保留标签。
var reservedLabelNames = map[string]bool{
	"job": true, "instance": true, "instance_name": true, "__address__": true, "mw_type": true,
}

// ValidateName 校验集成名称。
func ValidateName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return fmt.Errorf("集成名称不能为空")
	}
	if len(trimmed) > 63 {
		return fmt.Errorf("集成名称不能超过 63 个字符")
	}
	if !namePattern.MatchString(trimmed) {
		return fmt.Errorf("集成名称 %q 不合法：只能包含小写字母、数字、中划线与点，且以字母或数字开头结尾（如 jd-redis、redis.prod-01）", trimmed)
	}
	return nil
}

// ValidateLabels 校验自定义标签。
func ValidateLabels(labels map[string]string) error {
	for key, value := range labels {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("自定义标签名不能为空")
		}
		if reservedLabelNames[key] {
			return fmt.Errorf("自定义标签 %q 是平台保留标签，不能覆盖", key)
		}
		if !labelNamePattern.MatchString(key) {
			return fmt.Errorf("自定义标签名 %q 不合法：只能包含字母、数字与下划线，且以字母或下划线开头", key)
		}
		if strings.ContainsAny(value, "\n\r") {
			return fmt.Errorf("自定义标签 %q 的值不能包含换行", key)
		}
	}
	return nil
}

var labelNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate 校验某个模板下的集成入参。
func (t Template) Validate(in Instance) error {
	if err := ValidateName(in.Name); err != nil {
		return err
	}
	if in.Address.Host == "" {
		return fmt.Errorf("连接地址不能为空")
	}
	if in.Address.Port <= 0 || in.Address.Port > 65535 {
		return fmt.Errorf("连接地址的端口必须在 1-65535 之间")
	}
	if t.NeedsAuth && strings.TrimSpace(in.Username) == "" && t.Type != TypeES {
		// ES 允许匿名/仅口令；其余组件给出明确提示，避免误配成匿名访问。
		return fmt.Errorf("%s 集成需要填写监控账号（只读账号）", t.Name)
	}
	if err := ValidateLabels(in.Labels); err != nil {
		return err
	}
	known := make(map[string]bool, len(t.Options))
	for _, opt := range t.Options {
		known[opt.Key] = true
	}
	for key := range in.Options {
		if !known[key] {
			return fmt.Errorf("不支持的 Exporter 参数 %q（模板 %s 未声明，拒绝透传任意参数）", key, t.Type)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 渲染：Exporter 的 env 与命令行参数
// ---------------------------------------------------------------------------

// RenderEnv 渲染 Exporter 容器需要的环境变量（不含密钥掩码，调用方决定是否脱敏）。
func (t Template) RenderEnv(in Instance) map[string]string {
	env := map[string]string{}
	switch t.Type {
	case TypeRedis:
		env["REDIS_ADDR"] = in.Address.URL("redis")
		if in.Username != "" {
			env["REDIS_USER"] = in.Username
		}
		if in.Password != "" {
			env["REDIS_PASSWORD"] = in.Password
		}
	case TypeMySQL:
		// 官方推荐方式：地址与用户名走 flag，口令走 MYSQLD_EXPORTER_PASSWORD。
		// 刻意**不拼 DATA_SOURCE_NAME**：口令里的 @ ( ) / : ? 会破坏 DSN 解析，
		// 表现为 Exporter 启动即失败、Prometheus 侧 up=0，而原因很难看出来。
		if in.Password != "" {
			env["MYSQLD_EXPORTER_PASSWORD"] = in.Password
		}
	case TypePG:
		env["DATA_SOURCE_NAME"] = pgDSN(in)
	}
	for _, opt := range t.Options {
		if opt.Target != TargetEnv {
			continue
		}
		if value, ok := t.optionValue(in, opt); ok {
			env[opt.Key] = value
		}
	}
	return env
}

// RenderArgs 渲染 Exporter 容器的命令行参数。
func (t Template) RenderArgs(in Instance) []string {
	args := make([]string, 0, len(t.Options)+3)
	switch t.Type {
	case TypeMySQL:
		// 地址与用户名走 flag（口令走 MYSQLD_EXPORTER_PASSWORD 环境变量）：
		// 这样 DSN 里不再拼接口令，特殊字符不会导致解析失败。
		args = append(args, "--mysqld.address="+in.Address.HostPort())
		if in.Username != "" {
			args = append(args, "--mysqld.username="+in.Username)
		}
	case TypeKafka:
		args = append(args, "--kafka.server="+in.Address.HostPort())
	case TypeES:
		args = append(args, "--es.uri="+in.Address.URL("http"))
		if in.Username != "" {
			args = append(args, "--es.username="+in.Username)
		}
		if in.Password != "" {
			args = append(args, "--es.password="+in.Password)
		}
	case TypeNginx:
		args = append(args, "--nginx.scrape-uri="+in.Address.URL("http"))
	}
	for _, opt := range t.Options {
		if opt.Target != TargetArg {
			continue
		}
		value, ok := t.optionValue(in, opt)
		if opt.Kind == "bool" {
			// bool 开关三态：
			//   显式为真           → --collect.xxx
			//   显式为假但上游默认开 → --no-collect.xxx（否则 exporter 仍会按默认值采集）
			//   显式为假且上游默认关 → 不输出
			// kingpin 对 bool flag 统一支持 --no- 前缀，这是"关掉默认开启的采集项"的唯一写法。
			if ok && isTruthy(value) {
				args = append(args, "--"+opt.Key)
				continue
			}
			if isTruthy(opt.Default) {
				args = append(args, "--no-"+opt.Key)
			}
			continue
		}
		if !ok {
			continue
		}
		args = append(args, "--"+opt.Key+"="+value)
	}
	return args
}

// optionValue 取参数最终值（未填写时回落到模板默认值）。
func (t Template) optionValue(in Instance, opt Option) (string, bool) {
	value := strings.TrimSpace(in.Options[opt.Key])
	if value == "" {
		value = strings.TrimSpace(opt.Default)
	}
	if value == "" {
		return "", false
	}
	return value, true
}

// mysqlDSN 已移除：mysqld_exporter 的凭据改为 --mysqld.username / --mysqld.address
// 加 MYSQLD_EXPORTER_PASSWORD 环境变量，不再拼装 DATA_SOURCE_NAME——
// 拼串方式会让口令里的 @ ( ) / : ? 破坏 DSN 解析（表现为 Exporter 启动失败、up=0）。

// pgDSN 拼装 postgres_exporter 的 DATA_SOURCE_NAME。
func pgDSN(in Instance) string {
	credential := in.Username
	if in.Password != "" {
		credential = in.Username + ":" + in.Password
	}
	if credential == "" {
		credential = "postgres"
	}
	return fmt.Sprintf("postgresql://%s@%s/postgres?sslmode=disable", credential, in.Address.HostPort())
}

// isTruthy 判断布尔型参数取值。
func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on", "y":
		return true
	default:
		return false
	}
}

// ContainerName 返回 Exporter 容器名（可直接作为 docker 容器名使用）。
func ContainerName(name string) string {
	sanitized := strings.NewReplacer(".", "-", "_", "-").Replace(strings.TrimSpace(name))
	return "mwops-exporter-" + sanitized
}

// SanitizeEnvValue 去掉会导致容器环境变量注入异常的值（换行等）。
func SanitizeEnvValue(value string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(value)
}
