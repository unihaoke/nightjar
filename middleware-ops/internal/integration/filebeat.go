package integration

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// 本文件渲染目标机的 filebeat.yml（日志集成的主产物）。
//
// 设计约定（与 docs/LOG_INTEGRATION.md §二 / §五 对应）：
//   - 平台提供 Kafka 与消费链路，采集由目标机上的 Filebeat 完成：
//     因此这里只生成"采集 → 推送"这一段配置，不涉及 Exporter / Prometheus；
//   - 手写 YAML 而**不引入 yaml 库**：键的顺序就是运维 review 这份配置时的阅读顺序
//     （inputs → processors → 输出），且同一份输入必须产出逐字节相同的文件——
//     Ansible `copy` 靠内容 md5 判定是否变更，任何随机的键序都会导致"每次重放都重启 Filebeat"；
//   - 字段名与平台消费者（internal/logpipe）的映射表严格对齐：
//     fields.service → Service、fields.environment → Environment、fields.server → ServerName
//     （server 是**目标机地址**，平台按它归集 server_instances；集成名走 fields.integration）。
//
// 同时自校验：渲染前把输入校验完，渲染后再用 yaml.v3 解析一遍（validateYAMLParse），
// 保证"平台生成的 filebeat.yml 至少是合法 YAML"，而不是等目标机 Filebeat 报
// `Exiting: error loading config file` 才发现。

// LogInput 是一次日志集成的渲染输入。
//
// 字段全部来自集成表单（模板 TypeLog 的 Options），不含任何平台侧连接信息：
// 平台 Kafka 的地址由调用方解析成 KafkaHosts 传进来（见 §八的 external_host / external_port）。
type LogInput struct {
	// Name 为集成名：用于目标机配置目录名与 fields.integration。
	// 注意它**不是** fields.server —— server 必须是目标机地址，否则平台会按日志重复登记服务器。
	Name string
	// Host 为目标服务器地址（本机填 127.0.0.1）。
	Host string
	// Service / Environment 写入事件字段，供日志页归集与筛选。
	Service     string
	Environment string
	// Paths 为日志文件通配（目标机上的绝对路径），支持 glob（** 表示递归）。
	Paths []string
	// Level 为最低级别：ERROR / WARN / INFO（INFO 表示不过滤）。
	Level string
	// Multiline 为是否合并多行堆栈；MultilinePattern 为空时按 Java 默认。
	Multiline        bool
	MultilinePattern string
	// KafkaHosts 形如 ["10.0.0.5:9092"]（**必须是目标机能访问到的地址**）。
	KafkaHosts []string
	// Topic 为平台消费的日志 topic（默认 mwops-logs）。
	Topic string
	// FilebeatVersion 仅用于注释与安装产物，不写进 filebeat.yml（Filebeat 不认识该配置项）。
	FilebeatVersion string
	// InstallMode 取值 auto / package / docker；本文件不消费它，由 ansible_log.go 使用。
	// 放在这里是为了让"一次日志集成"只有一个入参结构体（调用方不必拼两个对象）。
	InstallMode string
}

// 级别过滤取值（与前端下拉一致）。
const (
	// LogLevelError 只保留含 ERROR 的行。
	LogLevelError = "ERROR"
	// LogLevelWarn 保留含 ERROR 与 WARN 的行。
	LogLevelWarn = "WARN"
	// LogLevelInfo 不过滤（全量采集）。
	LogLevelInfo = "INFO"
)

// 安装方式取值（与模板 MWOPS_LOG_INSTALL_MODE 的选项一致）。
const (
	// LogInstallAuto 优先复用已安装的 Filebeat，其次 docker，最后包安装。
	LogInstallAuto = "auto"
	// LogInstallPackage 用发行版包（deb/rpm）+ systemd。
	LogInstallPackage = "package"
	// LogInstallDocker 用官方镜像起容器。
	LogInstallDocker = "docker"
)

// filebeatDefaultImageRepo 是 Filebeat 官方镜像仓库（docker 安装方式使用）。
const filebeatDefaultImageRepo = "docker.elastic.co/beats/filebeat"

// filebeatDefaultJavaPattern 是 Java 日志的默认多行合并正则（YAML 里需再转义一次反斜杠）。
//
// 语义：行首是 `2024-01-01 12:00:00` 这类时间戳 → 认定为**新事件的第一行**；
// 其余行（`\tat com.foo.Bar`、`Caused by: …`）归到上一条事件里。
//
// 换语言时改这个 pattern 即可（模板的 MWOPS_LOG_MULTILINE_PATTERN 可直接覆盖）：
//
//	Python ：'^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}'（logging 默认格式）
//	        或 '^Traceback \(most recent call last\):'（只合并 traceback 块）
//	Go     ：'^[0-9]{4}/[0-9]{2}/[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}'（log 包默认前缀）
//
// 注意：negate 必须为 true、match 必须为 after —— 这两项与"pattern 描述首行特征"配套，
// 反过来（pattern 描述续行）会让所有堆栈都被拆成一行一条。
const filebeatDefaultJavaPattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}`

// RenderFilebeatConfig 渲染 filebeat.yml 的内容。
//
// 输出是**纯文本 YAML**（末尾带换行），调用方负责落到目标机（Ansible copy 的 content）。
// 校验失败时返回错误，绝不"渲染一半"：宁可让使用者在保存集成时看到"日志路径不能为空"，
// 也不要下发一份 Filebeat 启动即失败的配置（那种失败在目标机上只是一行 Exiting: error loading config）。
func RenderFilebeatConfig(in LogInput) (string, error) {
	// 集成名会同时作为容器名、配置目录名与事件里的 server 字段，必须与其它集成同一套规范。
	if err := ValidateName(in.Name); err != nil {
		return "", err
	}
	paths := normalizeLogPaths(in.Paths)
	if len(paths) == 0 {
		return "", fmt.Errorf("日志路径不能为空：请至少填写一个目标机上的日志文件通配（如 /var/log/app/*.log）")
	}
	hosts := normalizeStringList(in.KafkaHosts)
	if len(hosts) == 0 {
		return "", fmt.Errorf("Kafka 地址不能为空：请先配置平台 Kafka 的对外地址（KAFKA_ADVERTISED_HOST / MWOPS_KAFKA_EXTERNAL_HOST）")
	}
	topic := strings.TrimSpace(in.Topic)
	if topic == "" {
		return "", fmt.Errorf("Kafka topic 不能为空：日志集成必须知道把事件推到哪个 topic（默认 mwops-logs）")
	}
	level := strings.ToUpper(strings.TrimSpace(in.Level))
	if level == "" {
		// 与模板默认值一致：级别是"降低噪音"的手段，表单没给值时按 ERROR 收敛。
		level = LogLevelError
	}
	switch level {
	case LogLevelError, LogLevelWarn, LogLevelInfo:
	default:
		return "", fmt.Errorf("最低级别 %q 不合法：只能是 %s / %s / %s（%s 表示不过滤）",
			in.Level, LogLevelError, LogLevelWarn, LogLevelInfo, LogLevelInfo)
	}

	service := strings.TrimSpace(in.Service)
	if service == "" {
		// 服务名留空时回落集成名：消费者按 service 归集日志，空值会让所有服务混在一起。
		service = strings.TrimSpace(in.Name)
	}
	environment := strings.TrimSpace(in.Environment)
	if environment == "" {
		environment = "dev"
	}
	server := strings.TrimSpace(in.Host)
	if server == "" {
		server = "127.0.0.1"
	}

	var b strings.Builder
	writeFilebeatHeader(&b, in, level)
	writeFilebeatInputs(&b, in, paths, service, environment, server, level)
	writeFilebeatProcessors(&b, level)
	writeFilebeatOutput(&b, hosts, topic, service)
	content := b.String()
	// 渲染后自校验：这份配置最终要写进目标机的 /etc/filebeat/filebeat.yml，
	// 一旦 YAML 非法，Filebeat 会以 `Exiting: error loading config file` 秒退，
	// 而使用者在平台上只能看到"目标机没有日志"。这里提前用自己的解析器拦下。
	if err := validateYAMLParse("filebeat.yml", content); err != nil {
		return "", err
	}
	return content, nil
}

// validateYAMLParse 用仓库既有的 yaml.v3 校验任意 YAML 文本。
//
// 与 validatePlaybookYAML 的分工：后者针对 playbook 还有"裸 {{ }} / Go 模板语法"两项
// 额外检查；filebeat.yml 里出现 {{ 只可能是使用者的日志路径里带了花括号（合法内容），
// 因此这里只做纯粹的 YAML 解析，不套用 playbook 的引号规则。
func validateYAMLParse(kind, content string) error {
	var doc any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return fmt.Errorf("平台生成的 %s 不是合法 YAML（这属于平台模板缺陷，请提交工单）：%s", kind, firstLine(err.Error()))
	}
	return nil
}

// writeFilebeatHeader 写入文件头注释。
//
// 这几行是目标机上的"说明书"：运维看到 /etc/filebeat/filebeat.yml 时，
// 第一眼要能判断出「这份文件不是人写的、手改会在下次重放时被覆盖」。
func writeFilebeatHeader(b *strings.Builder, in LogInput, level string) {
	b.WriteString("# ==============================================================================\n")
	b.WriteString("# 本文件由平台「集成中心」渲染（集成：" + in.Name + "，组件：filebeat）。\n")
	b.WriteString("# 请勿手工修改：下次「重新应用 / 保存集成」会按模板重新渲染并覆盖这里的内容。\n")
	b.WriteString("# 需要调整采集行为，请在平台页面上改「日志路径 / 最低级别 / 多行合并」等参数。\n")
	b.WriteString("#\n")
	b.WriteString("# 输出为 JSON：Filebeat 的 output.kafka 默认就是 JSON 编码（每行一个事件对象），\n")
	b.WriteString("# 平台消费者依赖该默认行为读取 message / fields.* / log.file.path，\n")
	b.WriteString("# 因此这里**刻意不写 codec 段**——一旦改成 format: text，平台侧字段映射会全部落空。\n")
	if v := strings.TrimSpace(in.FilebeatVersion); v != "" {
		b.WriteString("# 目标机 Filebeat 版本：" + v + "（filestream 输入要求 ≥ 7.15）\n")
	}
	b.WriteString("# 最低级别：" + level + "（在目标机侧过滤，降低 Kafka 与平台负载）\n")
	b.WriteString("# ==============================================================================\n")
}

// writeFilebeatInputs 渲染 filebeat.inputs（filestream）。
func writeFilebeatInputs(b *strings.Builder, in LogInput, paths []string, service, environment, server, level string) {
	b.WriteString("filebeat.inputs:\n")
	b.WriteString("  - type: filestream\n")
	b.WriteString("    enabled: true\n")
	// id 让 Filebeat 在注册表里稳定识别这份输入：不加时 Filebeat 会用内部生成的名字，
	// 配置一改就可能被当成"新输入"，已读位点丢失、历史日志被重采一遍。
	b.WriteString("    id: " + yamlScalar("mwops-"+in.Name) + "\n")
	b.WriteString("    paths:\n")
	for _, item := range paths {
		b.WriteString("      - " + yamlScalar(item) + "\n")
	}
	// fields 写入事件字段；fields_under_root 刻意用 **false**：
	// true 会把 service / environment / server 提升到事件顶层，正好覆盖 Filebeat 自己的
	// 顶层字段（`server` 会压掉 Filebeat 自带的 host 元数据、`environment` 会与 7.x 的
	// agent 字段语义打架）。平台消费者统一按 `fields.*` 读取（见 LOG_INTEGRATION.md §五），
	// 保持 false 才能做到"平台字段与应用字段互不污染"。
	//
	// server / integration 的分工（很关键，写反会让平台多出一堆假服务器）：
	//   - fields.server      = **目标机地址**（in.Host）。平台按它反查/登记 server_instances，
	//     填集成名的话每条日志都会注册一台新"服务器"，服务器列表会迅速膨胀；
	//   - fields.integration = 集成名。平台暂不消费，仅用于排障时把日志与集成对上号。
	b.WriteString("    fields:\n")
	b.WriteString("      service: " + yamlScalar(service) + "\n")
	b.WriteString("      environment: " + yamlScalar(environment) + "\n")
	b.WriteString("      server: " + yamlScalar(server) + "\n")
	b.WriteString("      integration: " + yamlScalar(in.Name) + "\n")
	b.WriteString("    fields_under_root: false\n")
	// exclude_lines：把 Filebeat 自己的行排掉，否则"连接被拒"这类错误会被采回来 → 平台又生成
	// 一条告警 → 噪音自激。这是日志采集最常见的自污染来源。
	b.WriteString("    exclude_lines: ['^DBG', '^\\s*$']\n")
	// fingerprint：按"文件开头一段内容 + 大小"识别文件身份，日志轮转/被截断后不会从头重采。
	// 不加时 filebeat 用 inode+device，2022 年后新部署的 daemonset 日志量翻倍问题即源于此。
	b.WriteString("    fingerprint:\n")
	b.WriteString("      enabled: true\n")
	b.WriteString("      file_identity.native: ~\n")
	b.WriteString("    parsers:\n")
	if in.Multiline {
		pattern := strings.TrimSpace(in.MultilinePattern)
		if pattern == "" {
			pattern = filebeatDefaultJavaPattern
		}
		writeMultilineParser(b, pattern)
		return
	}
	// 关闭多行合并时也保留 parsers 段（只放 ndjson）：
	// 这样"开关多行"不会改变配置结构，diff 只有 multiline 那几行，便于 review。
	b.WriteString("      - ndjson:\n")
	b.WriteString("          target: \"\"\n")
	b.WriteString("          overwrite_keys: false\n")
	b.WriteString("          add_error_key: true\n")
}

// writeMultilineParser 渲染多行合并解析器。
func writeMultilineParser(b *strings.Builder, pattern string) {
	b.WriteString("      - multiline:\n")
	// 反斜杠是正则的一部分，但 YAML 单引号标量不做转义（'' 才是单引号本身），
	// 因此这里只需照原样输出，不要手动加一层 \ 转义（加了会被 Filebeat 当字面量）。
	b.WriteString("          pattern: '" + strings.ReplaceAll(pattern, "'", "''") + "'\n")
	b.WriteString("          negate: true\n")
	b.WriteString("          match: after\n")
	// max_lines：一条被误判为"未结束"的日志不能无限吞下去（例如日志尾部一直没有新时间戳），
	// 否则该文件的事件会一直攒在内存里，直到 Filebeat 重启才吐出来。
	b.WriteString("          max_lines: 500\n")
	b.WriteString("          timeout: 5s\n")
}

// writeFilebeatProcessors 渲染级别过滤。
//
// 三档行为（这是"保留"而不是"丢弃"的正向表达，避免漏写 not 造成反效果）：
//
//	ERROR → 只保留含 ERROR 的行
//	WARN  → 保留含 ERROR 或 WARN 的行
//	INFO  → **不生成任何 drop_event**，即不过滤（全收）
//
// 注意：drop_event 的 when 是"命中则丢弃"，因此要"保留含 ERROR 的行"必须写成
// `when.not.contains.message: "ERROR"` —— 漏掉 not 会把唯一的错误日志全丢掉，
// 而且现象是"平台一条日志都收不到"，很难往回追到这一行。
func writeFilebeatProcessors(b *strings.Builder, level string) {
	terms := levelMatchTerms(level)
	if len(terms) == 0 {
		return
	}
	b.WriteString("processors:\n")
	b.WriteString("  # 级别过滤：" + level + "。命中 contains 才保留，未命中的事件在目标机侧丢弃（不占 Kafka 带宽）。\n")
	b.WriteString("  - drop_event:\n")
	b.WriteString("      when:\n")
	b.WriteString("        not:\n")
	if len(terms) == 1 {
		b.WriteString("          contains:\n")
		b.WriteString("            message: " + yamlScalar(terms[0]) + "\n")
		return
	}
	// 多条件用 or：OR(contains ERROR, contains WARN) 取反 = 两者都不含才丢弃。
	b.WriteString("          or:\n")
	for _, term := range terms {
		b.WriteString("            - contains:\n")
		b.WriteString("                message: " + yamlScalar(term) + "\n")
	}
}

// levelMatchTerms 返回该级别下"应当保留"的关键字列表；空表示不过滤。
func levelMatchTerms(level string) []string {
	switch level {
	case LogLevelError:
		return []string{"ERROR"}
	case LogLevelWarn:
		return []string{"ERROR", "WARN"}
	default:
		// INFO（以及任何已被 RenderFilebeatConfig 校验过的取值）→ 不过滤。
		return nil
	}
}

// writeFilebeatOutput 渲染 output.kafka。
func writeFilebeatOutput(b *strings.Builder, hosts []string, topic, service string) {
	b.WriteString("output.kafka:\n")
	b.WriteString("  # 地址必须是**目标机视角**能访问到的平台地址（advertised 地址）。\n")
	b.WriteString("  # 配错的典型现象：连上 9092 握手成功，拿到 broker 元数据后立刻断开并报\n")
	b.WriteString("  # dial tcp 127.0.0.1:9092: connect: connection refused（broker 把客户端引导去了它自己的 localhost）。\n")
	b.WriteString("  hosts:\n")
	for _, host := range hosts {
		b.WriteString("    - " + yamlScalar(host) + "\n")
	}
	b.WriteString("  topic: " + yamlScalar(topic) + "\n")
	b.WriteString("  # 刻意不写 codec 段：JSON 是 output.kafka 的默认编码，平台消费者按 JSON 解析。\n")
	b.WriteString("  # round_robin 让事件均匀分布到各分区（削峰优先）；reachable_only: false 表示\n")
	b.WriteString("  # 分区 leader 暂时不可达时仍然发送，由客户端重试而不是直接丢事件。\n")
	b.WriteString("  partition.round_robin:\n")
	b.WriteString("    reachable_only: false\n")
	b.WriteString("  # 只等 leader 落盘（1）：日志是事件源不是账本，牺牲少量可靠性换吞吐；\n")
	b.WriteString("  # 平台侧靠「本地缓冲 + 位点」保证不丢，不依赖 acks=-1。\n")
	b.WriteString("  required_acks: 1\n")
	b.WriteString("  compression: gzip\n")
	// max_message_bytes 必须 ≤ broker 的 message.max.bytes（默认 1048576），
	// 取 1000000 留出协议开销余量；调大这里会让 broker 直接拒收超大消息。
	b.WriteString("  max_message_bytes: 1000000\n")
	b.WriteString("  client_id: " + yamlScalar("mwops-filebeat-"+service) + "\n")
	// 目标机 → 平台 Kafka 常见的是跨网段/弱网；默认 10s 太短，一次抖动就会重连风暴。
	b.WriteString("  timeout: 30\n")
	b.WriteString("  # broker 不可达时在本地队列缓冲（Filebeat 会退避重试并保留位点，不丢日志）。\n")
	b.WriteString("  max_retries: 3\n")
	b.WriteString("  retry.backoff.init: 1s\n")
	b.WriteString("  retry.backoff.max: 60s\n")
	b.WriteString("  # 关掉指标上报：目标机不一定能解析 apm-server 域名，开着只会在日志里刷连接错误。\n")
	b.WriteString("  # （Filebeat 8.x 默认关闭，这里显式写出，避免将来默认值变化带来噪音。）\n")
}

// normalizeLogPaths 归一化日志路径。
//
// 前端是多行文本框，因此换行与逗号都要能吃下（"多行/逗号分隔的 glob"是模板里写死的语义）；
// 同时去重并保持填写顺序——顺序即运维的意图（先采哪个、后采哪个），不排序。
// 路径里的空格是合法的（/var/log/my app/*.log），因此**不做**按空格拆分。
func normalizeLogPaths(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	// 先按换行/逗号/分号把每个元素再拆一层：MWOPS_LOG_PATHS 是一个字段，
	// 使用者很可能整段粘进来（"a.log, b.log" 或两行）。
	for _, item := range items {
		fields := strings.FieldsFunc(item, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ',' || r == ';'
		})
		for _, field := range fields {
			trimmed := strings.TrimSpace(field)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			out = append(out, trimmed)
		}
	}
	return out
}

// normalizeStringList 去空白、去重、保持顺序（Kafka hosts 可能是逗号分隔的一串）。
func normalizeStringList(items []string) []string {
	out := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		for _, field := range strings.Split(item, ",") {
			trimmed := strings.TrimSpace(field)
			if trimmed == "" || seen[trimmed] {
				continue
			}
			seen[trimmed] = true
			out = append(out, trimmed)
		}
	}
	return out
}
