package integration

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// 本文件锁定「日志集成」两份产物的契约。
//
// 为什么值得逐条固化：
//   - filebeat.yml 是**在目标机上执行的文件**：键写错不会报错，只会安静地采不到日志
//     （`fields_under_root`、`not.contains`、`output.kafka` 的键名都属于这类）；
//   - 安装 playbook 是**在别的机器上执行命令**的能力，必须幂等（"已存在则不需要部署"）、
//     不得出现裸 {{ }}（INC-005/INC-006），并且探测 → 决策 → 下发 → 校验的链路完整。

// logTestInput 构造一份最小可用的日志集成输入（各用例只覆盖自己要验的字段）。
func logTestInput() LogInput {
	return LogInput{
		Name: "order-service", Host: "10.0.0.21", Service: "order-service", Environment: "prod",
		Paths:      []string{"/var/log/app/*.log"},
		Level:      LogLevelError,
		Multiline:  true,
		KafkaHosts: []string{"10.0.0.5:9092"},
		Topic:      "mwops-logs", FilebeatVersion: "8.16.0", InstallMode: LogInstallAuto,
	}
}

// logTestOptions 构造一份最小可用的远程安装参数。
func logTestOptions() RemoteOptions {
	return RemoteOptions{
		Host: "10.0.0.21", SSHUser: "ops", SSHPort: 22, SSHPassword: "ssh-secret", Become: true,
	}
}

// mustRenderFilebeatConfig 渲染配置并在失败时直接结束用例。
func mustRenderFilebeatConfig(t *testing.T, in LogInput) string {
	t.Helper()
	content, err := RenderFilebeatConfig(in)
	if err != nil {
		t.Fatalf("渲染 filebeat.yml 失败：%v", err)
	}
	return content
}

// mustRenderFilebeatInstall 渲染安装产物并在失败时直接结束用例。
func mustRenderFilebeatInstall(t *testing.T, in LogInput, opts RemoteOptions) RemoteArtifacts {
	t.Helper()
	art, err := RenderFilebeatInstall(in, opts)
	if err != nil {
		t.Fatalf("渲染 Filebeat 安装产物失败：%v", err)
	}
	return art
}

// yamlAt 按路径取 YAML 节点（路径形如 "output.kafka.topic"）。
//
// 注意：`filebeat.inputs` 这类键名**本身含点**，因此查找时先按"整段是一个键"匹配，
// 匹配不到才按点拆开递归（顺序反了会把 filebeat.inputs 拆成 filebeat → inputs 而找不到）。
//
// 用真实解析器而不是字符串包含：这份配置是目标机上真的要跑的文件，
// "能解析出这个键、值是这些"才是契约，字符串包含只能证明"模板里打过这个字"。
func yamlAt(t *testing.T, root *yaml.Node, path string) *yaml.Node {
	t.Helper()
	node := lookupYAMLNode(valueNode(root), strings.Split(path, "."))
	if node == nil {
		t.Fatalf("filebeat.yml 里找不到 %q（键名或层级不符）:\n%s", path, dumpYAML(root))
	}
	return node
}

// valueNode 跳过文档节点，取真正的根节点。
func valueNode(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		return node.Content[0]
	}
	return node
}

// lookupYAMLNode 在映射里逐段查找；每段先当整体键名，再按点拆分。
func lookupYAMLNode(node *yaml.Node, segments []string) *yaml.Node {
	if node == nil || len(segments) == 0 {
		return node
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	if len(segments) == 1 {
		if child := mappingChild(node, segments[0]); child != nil {
			return child
		}
		// 末段本身含点但没匹配到：再拆分一次（如 filebeat.inputs）。
		parts := strings.Split(segments[0], ".")
		if len(parts) > 1 {
			return lookupYAMLNode(node, parts)
		}
		return nil
	}
	if child := mappingChild(node, segments[0]); child != nil {
		return lookupYAMLNode(child, segments[1:])
	}
	// 当前段可能是"含点的键"的前缀：把剩余段合并回来再试。
	for cut := len(segments) - 1; cut >= 1; cut-- {
		joined := strings.Join(segments[:cut+1], ".")
		if child := mappingChild(node, joined); child != nil {
			return lookupYAMLNode(child, segments[cut+1:])
		}
	}
	return nil
}

// mappingChild 返回映射中指定键的值节点。
func mappingChild(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// dumpYAML 在失败信息里附带原文（重新渲染一次，便于直接看到键序与缩进）。
func dumpYAML(root *yaml.Node) string {
	out, err := yaml.Marshal(root)
	if err != nil {
		return "<无法序列化>"
	}
	return string(out)
}

// parseYAML 把渲染结果解析成 YAML 文档节点（失败即用例失败）。
func parseYAML(t *testing.T, label, content string) *yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("%s 不是合法 YAML：%v\n%s", label, err, content)
	}
	return &doc
}

// yamlStrings 把序列节点读成字符串切片（同时兼作"它确实是序列"的断言）。
func yamlStrings(t *testing.T, node *yaml.Node, path string) []string {
	t.Helper()
	if node.Kind != yaml.SequenceNode {
		t.Fatalf("%s 应为 YAML 序列，实际 kind=%d", path, node.Kind)
	}
	out := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		out = append(out, item.Value)
	}
	return out
}

// TestRenderFilebeatConfigIsParsableYAML 锁定"渲染出来的就一定得是合法 YAML"。
//
// 这是最廉价也最值钱的一条：配置非法时 Filebeat 以
// `Exiting: error loading config file` 秒退，而平台侧只能看到"目标机没有日志"。
func TestRenderFilebeatConfigIsParsableYAML(t *testing.T) {
	content := mustRenderFilebeatConfig(t, logTestInput())
	// 顶部三行注释是目标机上的"说明书"：手改会被覆盖 + 不写 codec 段的原因。
	for _, want := range []string{
		"本文件由平台「集成中心」渲染",
		"请勿手工修改",
		"output.kafka 默认就是 JSON 编码",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("文件头注释应包含 %q（目标机上唯一的说明来源）：\n%s", want, content)
		}
	}
	parseYAML(t, "filebeat.yml", content)
}

// TestRenderFilebeatConfigPaths 锁定 paths 多值（含多行/逗号分隔的粘贴形式）。
func TestRenderFilebeatConfigPaths(t *testing.T) {
	cases := []struct {
		name  string
		paths []string
		want  []string
	}{
		{
			name:  "多个路径各自成行",
			paths: []string{"/var/log/app/*.log", "/data/logs/**/*.log"},
			want:  []string{"/var/log/app/*.log", "/data/logs/**/*.log"},
		},
		{
			name:  "多行粘贴（前端 textarea 的常见形态）",
			paths: []string{"/var/log/a/*.log\n/var/log/b/*.log\n"},
			want:  []string{"/var/log/a/*.log", "/var/log/b/*.log"},
		},
		{
			name:  "逗号分隔并去重",
			paths: []string{"/var/log/a.log, /var/log/b.log", "/var/log/a.log"},
			want:  []string{"/var/log/a.log", "/var/log/b.log"},
		},
	}
	for _, tc := range cases {
		in := logTestInput()
		in.Paths = tc.paths
		content := mustRenderFilebeatConfig(t, in)
		doc := parseYAML(t, "filebeat.yml", content)
		inputs := yamlAt(t, doc, "filebeat.inputs")
		if inputs.Kind != yaml.SequenceNode || len(inputs.Content) == 0 {
			t.Fatalf("%s：filebeat.inputs 应至少有一条输入：\n%s", tc.name, content)
		}
		got := yamlStrings(t, yamlAt(t, inputs.Content[0], "paths"), "filebeat.inputs[0].paths")
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Fatalf("%s：paths 应为 %v，实际 %v\n%s", tc.name, tc.want, got, content)
		}
		if yamlAt(t, inputs.Content[0], "type").Value != "filestream" {
			t.Fatalf("%s：输入类型应为 filestream（deprecated 的 log 输入没有断点续传）：\n%s", tc.name, content)
		}
	}
}

// TestRenderFilebeatConfigLevels 锁定级别过滤的三档行为。
//
// 这一条最容易写反：drop_event 的 when 是"命中则丢弃"，
// 因此"保留 ERROR"必须写成 `when.not.contains` —— 漏掉 not 会把唯一的错误日志全丢掉，
// 现象却是"平台一条日志都收不到"，很难往回追到这一行。
func TestRenderFilebeatConfigLevels(t *testing.T) {
	cases := []struct {
		level string
		// wantTerms 是应当出现在过滤条件里的关键字（空表示"完全不过滤"）。
		wantTerms []string
		noFilter  bool
	}{
		{level: LogLevelError, wantTerms: []string{"ERROR"}},
		{level: LogLevelWarn, wantTerms: []string{"ERROR", "WARN"}},
		{level: LogLevelInfo, noFilter: true},
		{level: "info", noFilter: true}, // 大小写不敏感（表单可能传小写）
	}
	for _, tc := range cases {
		in := logTestInput()
		in.Level = tc.level
		content := mustRenderFilebeatConfig(t, in)
		if tc.noFilter {
			if strings.Contains(content, "drop_event") {
				t.Fatalf("INFO 表示不过滤，不得生成 drop_event（否则会把日志丢掉）：\n%s", content)
			}
			continue
		}
		if !strings.Contains(content, "drop_event") || !strings.Contains(content, "not:") {
			t.Fatalf("%s 档必须用 drop_event + not（命中才保留），否则错误日志会被丢光：\n%s", tc.level, content)
		}
		for _, term := range tc.wantTerms {
			if !strings.Contains(content, "message: "+term) {
				t.Fatalf("%s 档应匹配 %q：\n%s", tc.level, term, content)
			}
		}
		// WARN 档不得出现别的级别关键字（多一个就意味着少收一类日志）。
		if tc.level == LogLevelWarn && strings.Contains(content, "message: INFO") {
			t.Fatalf("WARN 档不应匹配 INFO：\n%s", content)
		}
		parseYAML(t, tc.level+" 档 filebeat.yml", content)
	}
}

// TestRenderFilebeatConfigMultiline 锁定多行合并的开与关。
func TestRenderFilebeatConfigMultiline(t *testing.T) {
	in := logTestInput()
	in.Multiline = true
	content := mustRenderFilebeatConfig(t, in)
	doc := parseYAML(t, "filebeat.yml", content)
	inputs := yamlAt(t, doc, "filebeat.inputs")
	parsers := yamlAt(t, inputs.Content[0], "parsers")
	if parsers.Kind != yaml.SequenceNode || len(parsers.Content) == 0 {
		t.Fatalf("开启多行合并时应生成 parsers：\n%s", content)
	}
	// parsers 首项是 "- multiline: {...}"，即一个单键映射，取它的值才是 multiline 参数表。
	parser := parsers.Content[0]
	multiline := yamlAt(t, parser, "multiline")
	for path, want := range map[string]string{
		// negate=true + match=after 表示"pattern 描述的是首行特征"；反过来会让所有堆栈
		// 被拆成一行一条（现象是平台上一堆没有上下文的碎片日志）。
		"negate": "true",
		"match":  "after",
	} {
		if got := yamlAt(t, multiline, path).Value; got != want {
			t.Fatalf("multiline.%s 应为 %q，实际 %q（拆错堆栈方向）:\n%s", path, want, got, content)
		}
	}
	pattern := yamlAt(t, multiline, "pattern").Value
	if pattern == "" || !strings.Contains(pattern, "[0-9]{4}") {
		t.Fatalf("pattern 留空时应回落 Java 默认（行首时间戳）:%q\n%s", pattern, content)
	}
	if got := yamlAt(t, multiline, "max_lines").Value; got == "" {
		t.Fatalf("max_lines 必须设置：否则日志尾部会无限攒在内存里:\n%s", content)
	}

	// 自定义 pattern 原文落进配置（Python 场景就是靠这个字段切换）。
	in.MultilinePattern = `^Traceback \(most recent call last\):`
	custom := mustRenderFilebeatConfig(t, in)
	if !strings.Contains(custom, `^Traceback \(most recent call last\):`) {
		t.Fatalf("自定义多行正则应原样写入（不得被二次转义）:\n%s", custom)
	}
	parseYAML(t, "自定义多行的 filebeat.yml", custom)

	// 关闭时不再生成 multiline（但仍保留 parsers 段，便于 diff）。
	in.Multiline = false
	off := mustRenderFilebeatConfig(t, in)
	if strings.Contains(off, "multiline:") {
		t.Fatalf("关闭多行合并时不应生成 multiline:\n%s", off)
	}
	parseYAML(t, "关闭多行的 filebeat.yml", off)
}

// TestRenderFilebeatConfigKafkaOutput 锁定 output.kafka 的关键字段。
func TestRenderFilebeatConfigKafkaOutput(t *testing.T) {
	in := logTestInput()
	in.KafkaHosts = []string{"10.0.0.5:9092", "10.0.0.6:9092"}
	content := mustRenderFilebeatConfig(t, in)
	doc := parseYAML(t, "filebeat.yml", content)

	hosts := yamlStrings(t, yamlAt(t, doc, "output.kafka.hosts"), "output.kafka.hosts")
	if strings.Join(hosts, "|") != "10.0.0.5:9092|10.0.0.6:9092" {
		t.Fatalf("Kafka hosts 应为多值列表，实际 %v\n%s", hosts, content)
	}
	if got := yamlAt(t, doc, "output.kafka.topic").Value; got != "mwops-logs" {
		t.Fatalf("topic 应为 mwops-logs，实际 %q", got)
	}
	// reachable_only 必须显式为 false：true 时分区 leader 暂时不可达就直接丢事件。
	if got := yamlAt(t, doc, "output.kafka.partition.round_robin.reachable_only").Value; got != "false" {
		t.Fatalf("partition.round_robin.reachable_only 应为 false（否则分区不可达即丢日志），实际 %q", got)
	}
	if got := yamlAt(t, doc, "output.kafka.required_acks").Value; got != "1" {
		t.Fatalf("required_acks 应为 1，实际 %q", got)
	}
	if got := yamlAt(t, doc, "output.kafka.compression").Value; got != "gzip" {
		t.Fatalf("compression 应为 gzip，实际 %q", got)
	}
	// max_message_bytes 必须 ≤ broker 的 message.max.bytes（默认 1048576）。
	if got := yamlAt(t, doc, "output.kafka.max_message_bytes").Value; got != "1000000" {
		t.Fatalf("max_message_bytes 应为 1000000（超过 broker 上限会被拒收），实际 %q", got)
	}
	if got := yamlAt(t, doc, "output.kafka.client_id").Value; got == "" {
		t.Fatalf("client_id 必须写出来（否则目标机侧看不出是谁在推日志）：\n%s", content)
	}
	// 刻意不写 codec：JSON 是 output.kafka 的默认编码，平台消费者按 JSON 解析。
	// 按**解析后的键**判定（文件头与注释里会提到 codec 这个词，不能按全文匹配）。
	kafka := yamlAt(t, doc, "output.kafka")
	for i := 0; i+1 < len(kafka.Content); i += 2 {
		if kafka.Content[i].Value == "codec" {
			t.Fatalf("不得写 codec 段（改成 text 会让平台字段映射全部落空）：\n%s", content)
		}
	}
}

// TestRenderFilebeatConfigFieldsUnderRoot 锁定 fields 与 fields_under_root 的取值。
//
// fields_under_root 必须是 **false**：true 会把 service/environment/server 提升到事件顶层，
// 压掉 Filebeat 自带的同名元数据；平台消费者统一按 `fields.*` 读取（LOG_INTEGRATION.md §五）。
//
// server 与 integration 的分工也在这里锁住：server = **目标机地址**（平台按它登记
// server_instances，填集成名会让每条日志都新注册一台"服务器"），integration = 集成名。
func TestRenderFilebeatConfigFieldsUnderRoot(t *testing.T) {
	in := logTestInput()
	in.Name = "order-service"
	in.Service = "order-api"
	in.Environment = "prod"
	in.Host = "10.0.0.21"
	content := mustRenderFilebeatConfig(t, in)
	doc := parseYAML(t, "filebeat.yml", content)
	input := yamlAt(t, doc, "filebeat.inputs").Content[0]
	if got := yamlAt(t, input, "fields_under_root").Value; got != "false" {
		t.Fatalf("fields_under_root 应为 false（平台按 fields.* 读取），实际 %q", got)
	}
	for key, want := range map[string]string{
		"fields.service":     "order-api",
		"fields.environment": "prod",
		"fields.server":      "10.0.0.21",
		"fields.integration": "order-service",
	} {
		if got := yamlAt(t, input, key).Value; got != want {
			t.Fatalf("%s 应为 %q，实际 %q", key, want, got)
		}
	}
	// server 绝不能是集成名：那会让平台按日志去登记一台并不存在的服务器。
	if got := yamlAt(t, input, "fields.server").Value; got == in.Name {
		t.Fatalf("fields.server 必须是目标机地址（%q），不能是集成名（否则平台会重复登记服务器）：\n%s",
			in.Host, content)
	}

	// 服务名留空时回落集成名：空 service 会让日志页把所有服务混在一起。
	in.Service = ""
	fallback := mustRenderFilebeatConfig(t, in)
	if !strings.Contains(fallback, "service: "+in.Name) {
		t.Fatalf("服务名留空时应回落集成名 %q：\n%s", in.Name, fallback)
	}
	// 服务名回落不得带偏 server：它仍然必须是目标机地址。
	if !strings.Contains(fallback, "server: "+in.Host) {
		t.Fatalf("服务名回落时 fields.server 仍应是目标机地址 %q：\n%s", in.Host, fallback)
	}
}

// TestRenderFilebeatConfigValidation 锁定校验失败路径。
//
// 这些输入都必须**明确报错**：渲染出一份"缺 paths / 缺 hosts / 缺 topic"的配置，
// 在目标机上只会表现为 Filebeat 起不来或永远没有日志，使用者无从下手。
func TestRenderFilebeatConfigValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*LogInput)
		wantMsg string
	}{
		{name: "日志路径为空", mutate: func(in *LogInput) { in.Paths = nil }, wantMsg: "日志路径不能为空"},
		{name: "日志路径只有空白", mutate: func(in *LogInput) { in.Paths = []string{" ", "\n", ","} }, wantMsg: "日志路径不能为空"},
		{name: "Kafka 地址为空", mutate: func(in *LogInput) { in.KafkaHosts = nil }, wantMsg: "Kafka 地址不能为空"},
		{name: "Kafka 地址只有逗号", mutate: func(in *LogInput) { in.KafkaHosts = []string{" , "} }, wantMsg: "Kafka 地址不能为空"},
		{name: "topic 为空", mutate: func(in *LogInput) { in.Topic = "  " }, wantMsg: "Kafka topic 不能为空"},
		{name: "级别非法", mutate: func(in *LogInput) { in.Level = "TRACE" }, wantMsg: "不合法"},
		{name: "集成名非法", mutate: func(in *LogInput) { in.Name = "Order_Service" }, wantMsg: "不合法"},
		{name: "集成名为空", mutate: func(in *LogInput) { in.Name = "" }, wantMsg: "集成名称不能为空"},
	}
	for _, tc := range cases {
		in := logTestInput()
		tc.mutate(&in)
		if _, err := RenderFilebeatConfig(in); err == nil {
			t.Fatalf("%s：应报错（否则会下发一份目标机跑不起来的配置）", tc.name)
		} else if !strings.Contains(err.Error(), tc.wantMsg) {
			t.Fatalf("%s：错误信息应包含 %q（便于使用者知道改哪里），实际：%v", tc.name, tc.wantMsg, err)
		}
	}
}

// TestRenderFilebeatInstallKafkaProbe 锁定「目标机 → 平台 Kafka」的 TCP 连通性探测。
//
// 这一步的价值：Kafka advertised 地址配错时，平台侧一切正常、目标机却在报
// `dial tcp 127.0.0.1:9092: connect: connection refused`。只有从目标机真的连一次，
// 现场才有证据区分"网络不通"与"地址配错"。因此它必须：
//   - 探测真实配置里的那个地址（与 filebeat.yml 的 hosts[0] 一致，否则结论无意义）；
//   - 失败不阻断（failed_when: false + 输出结论），因为采集不一定立刻可用；
//   - 结论里给出可照抄的排查命令与 KAFKA_ADVERTISED_HOST 这个具体原因。
func TestRenderFilebeatInstallKafkaProbe(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		in.KafkaHosts = []string{"10.0.0.5:9092", "10.0.0.6:9092"}
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		// 探测目标必须是配置里写的第一个地址（与真实采集用同一个值）。
		if !strings.Contains(art.Playbook, "/dev/tcp/10.0.0.5/9092") {
			t.Fatalf("%s 模式应在目标机上探测 Kafka 地址 10.0.0.5:9092：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.Playbook, "nc -z -w 5 10.0.0.5 9092") {
			t.Fatalf("%s 模式应在没有 bash 时回退 nc：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.Playbook, "KAFKA_ADVERTISED_HOST") {
			t.Fatalf("%s 模式的探测结论应点明 KAFKA_ADVERTISED_HOST（这才是根因所在）：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.Playbook, "nc -vz 10.0.0.5 9092") {
			t.Fatalf("%s 模式应给出可照抄的人工复核命令：\n%s", mode, art.Playbook)
		}
		// 结论要能被自检/部署备注读到，但**不得**让安装整体失败。
		probeAt := strings.Index(art.Playbook, "探测目标机到平台 Kafka 的 TCP 连通性")
		if probeAt < 0 {
			t.Fatalf("%s 模式缺少 Kafka 连通性探测任务：\n%s", mode, art.Playbook)
		}
		task := art.Playbook[probeAt:]
		if !strings.Contains(task, "failed_when: false") {
			t.Fatalf("%s 模式的连通性探测失败不得中断安装（采集不一定立刻可用）：\n%s", mode, task)
		}
		if !strings.Contains(task, "filebeat_kafka_probe") || !strings.Contains(task, "ansible.builtin.debug") {
			t.Fatalf("%s 模式的探测结论应注册为变量并打印出来：\n%s", mode, task)
		}
	}
}

// playbookTaskNames 用 YAML 解析取出 playbook 里的任务名列表（按执行顺序）。
//
// 为什么不用字符串搜索：任务之间的先后关系是这次修复（INC-013）的核心契约，
// 而"某段文本出现得更早"在有注释、有多个分支的情况下很容易看走眼；
// 解析成任务名列表再比下标，才是对"执行顺序"本身的断言。
func playbookTaskNames(t *testing.T, playbook string) []string {
	t.Helper()
	doc := parseYAML(t, "playbook", playbook)
	// playbook 的根是**序列**（一个 play 一个元素），tasks 在 play 里，因此先取第一个 play，
	// 再取它的 tasks（直接 yamlAt(doc, "tasks") 会因为根是序列而找不到）。
	root := valueNode(doc)
	if root.Kind != yaml.SequenceNode || len(root.Content) == 0 {
		t.Fatalf("playbook 根应为序列（play 列表），实际 kind=%d", root.Kind)
	}
	tasks := yamlAt(t, root.Content[0], "tasks")
	if tasks.Kind != yaml.SequenceNode {
		t.Fatalf("playbook 的 tasks 应为序列，实际 kind=%d", tasks.Kind)
	}
	names := make([]string, 0, len(tasks.Content))
	for i, task := range tasks.Content {
		if task.Kind != yaml.MappingNode {
			t.Fatalf("第 %d 个任务不是映射（YAML 结构错位）", i+1)
		}
		name := mappingChild(task, "name")
		if name == nil {
			t.Fatalf("第 %d 个任务缺少 name（无法做顺序断言）", i+1)
		}
		names = append(names, name.Value)
	}
	return names
}

// taskIndex 返回名字以 prefix 开头的第一个任务下标（找不到即用例失败）。
func taskIndex(t *testing.T, names []string, prefix string) int {
	t.Helper()
	for i, name := range names {
		if strings.HasPrefix(name, prefix) {
			return i
		}
	}
	t.Fatalf("任务列表里找不到以 %q 开头的任务，实际任务：\n%s", prefix, strings.Join(names, "\n"))
	return -1
}

// TestRenderFilebeatInstallConfigBeforeContainer 锁定任务顺序（INC-013 的根因修复）。
//
// 线上真实故障（目标机 203.195.191.75，有 docker）：
//
//	fatal: [203.195.191.75]: FAILED! => {"changed": false,
//	  "msg": "can not use content with a dir as dest"}
//
// 根因不是 copy 写错，而是**顺序错了**：docker 分支先跑，
// `-v /etc/filebeat/filebeat.yml:...:ro` 遇到宿主上不存在的路径时，Docker 会把它创建成
// **目录**；紧接着 copy 拿着这个目录当 dest，必然报上面那句。
// 因此固定顺序必须是：
//
//	确保配置目录 → 自愈非普通文件 → 下发 filebeat.yml → 创建容器
//
// 少任何一步，或者把 copy 排到 docker 后面，都会重新掉进这个坑。
func TestRenderFilebeatInstallConfigBeforeContainer(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())
		names := playbookTaskNames(t, art.Playbook)

		dirAt := taskIndex(t, names, "准备 Filebeat 配置目录")
		healAt := taskIndex(t, names, "检查 filebeat.yml 是否被占用成目录")
		copyAt := taskIndex(t, names, "下发 filebeat.yml")
		if !(dirAt < healAt && healAt < copyAt) {
			t.Fatalf("%s 模式的任务顺序必须满足「确保目录(%d) < 自愈(%d) < 下发配置(%d)」："+
				"配置必须在容器挂载之前落成**文件**，否则 Docker 会把缺失的宿主路径创建成目录，"+
				"copy 报 can not use content with a dir as dest：\n%s",
				mode, dirAt, healAt, copyAt, strings.Join(names, "\n"))
		}
		// docker 分支只在 auto / docker 模式下存在（package 模式不碰容器）。
		if mode != LogInstallPackage {
			containerAt := taskIndex(t, names, "创建 Filebeat 容器")
			if copyAt >= containerAt {
				t.Fatalf("%s 模式：docker run 必须排在 copy 之后（copy=%d, run=%d）——"+
					"先起容器就会让 Docker 把 filebeat.yml 的宿主路径创建成目录：\n%s",
					mode, copyAt, containerAt, strings.Join(names, "\n"))
			}
		}
		// Kafka 探测与自检必须在配置下发之后（它们要读的是刚下发的配置）。
		if probe := taskIndex(t, names, "探测目标机到平台 Kafka"); probe < copyAt {
			t.Fatalf("%s 模式：Kafka 探测不应排在配置下发之前（probe=%d, copy=%d）", mode, probe, copyAt)
		}
	}
}

// TestRenderFilebeatInstallHealsPollutedPath 锁定自愈任务及其安全边界。
//
// 用户那台机器现在**已经被污染**：/etc/filebeat/filebeat.yml 是一个目录。
// 只调整顺序防不住它，必须能在下次执行时把目录清掉并重建为文件；
// 但**绝不能**误删正常的配置文件（那会把用户正在用的采集配置清空）。
// 因此 when 条件必须是 `exists and not isreg` —— 只在「存在且不是普通文件」时才删。
func TestRenderFilebeatInstallHealsPollutedPath(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		// 自愈任务：探测 → 注册变量 → 条件删除 → 说明原因。
		// 注意这里用的是**早期**快照 filebeat_config_stat（语义："写配置之前的状态"）；
		// 起容器前的闸门用的是另一份快照 filebeat_config_final，两者不得串（见 INC-016）。
		statBlock := playbookTaskBlock(t, art.Playbook, "检查 filebeat.yml 是否被占用成目录")
		for _, want := range []string{
			"ansible.builtin.stat:",          // 先探测
			"register: filebeat_config_stat", // 结果注册成变量（只服务自愈的 when）
		} {
			if !strings.Contains(statBlock, want) {
				t.Fatalf("%s 模式的探测任务应包含 %q：\n%s", mode, want, statBlock)
			}
		}
		removeBlock := playbookTaskBlock(t, art.Playbook, "清理非普通文件的 filebeat.yml")
		for _, want := range []string{
			"state: absent",                       // 删除非普通文件
			"when:",                               // 必须带条件
			"filebeat_config_stat.stat.exists",    // 存在才处理
			"not filebeat_config_stat.stat.isreg", // 普通文件绝不删
		} {
			if !strings.Contains(removeBlock, want) {
				t.Fatalf("%s 模式的清理任务应包含 %q（否则可能误删正常配置）：\n%s", mode, want, removeBlock)
			}
		}
		debugBlock := playbookTaskBlock(t, art.Playbook, "说明清理原因")
		for _, want := range []string{
			"ansible.builtin.debug:",
			"历史 docker 单文件挂载生成",
			"已清理并重建为文件",
		} {
			if !strings.Contains(debugBlock, want) {
				t.Fatalf("%s 模式的说明任务应包含 %q（使用者要能一眼看懂发生了什么）：\n%s", mode, want, debugBlock)
			}
		}
		// 启动容器前必须有闸门：宁可报错，也不要让 Docker 再制造一个目录。
		// package 模式不碰容器，因此这条只对 auto / docker 断言。
		if mode == LogInstallPackage {
			continue
		}
		existsGuard := playbookTaskBlock(t, art.Playbook, "闸门①：filebeat.yml 必须已存在")
		for _, want := range []string{
			"ansible.builtin.assert:",
			"filebeat_config_final.stat.exists", // 基于**新鲜**快照
			"磁盘空间",                              // 情形 A 的排查方向
		} {
			if !strings.Contains(existsGuard, want) {
				t.Fatalf("%s 模式的闸门①应包含 %q：\n%s", mode, want, existsGuard)
			}
		}
		isregGuard := playbookTaskBlock(t, art.Playbook, "闸门②：filebeat.yml 必须是普通文件")
		for _, want := range []string{
			"ansible.builtin.assert:",
			"filebeat_config_final.stat.isreg", // 基于**新鲜**快照
			"拒绝启动容器",
			"rm -rf", // 情形 B 的修法
		} {
			if !strings.Contains(isregGuard, want) {
				t.Fatalf("%s 模式的闸门②应包含 %q：\n%s", mode, want, isregGuard)
			}
		}
		for label, guard := range map[string]string{"闸门①": existsGuard, "闸门②": isregGuard} {
			if !strings.Contains(guard, "when:") {
				t.Fatalf("%s 模式的%s必须带 when（只在需要起容器时才校验）：\n%s", mode, label, guard)
			}
			// 绝不能退回"写配置之前"的陈旧快照（INC-016 的现场误报就是这么来的）。
			if strings.Contains(guard, "filebeat_config_stat") {
				t.Fatalf("%s 模式的%s不得引用陈旧的 filebeat_config_stat（那是写配置之前的状态）：\n%s",
					mode, label, guard)
			}
		}
	}
}

// TestRenderFilebeatInstallGuardUsesFreshStat 锁定"闸门必须基于新鲜状态"（INC-016）。
//
// 线上真实报错：
//
//	TASK [启动容器前确认 filebeat.yml 已是普通文件（防止 Docker 再制造目录）]
//	fatal: => {"assertion": "filebeat_config_stat.stat.exists", "evaluated_to": false, ...}
//
// 根因是**注册变量是快照**：早期那次 stat（line 68）的语义是"写配置**之前**的状态"，
// 只服务自愈任务的 when。而现场的顺序是：
//
//	文件当时不存在 → 自愈 when 为假（正确地跳过）→ copy 把文件建好
//	→ assert 复用旧快照说"不存在" → 拒绝启动容器
//
// 于是"文件不存在"这条**正确**的路径必然误报。修法是 assert 之前重新 stat，并用新变量；
// 本条用例把这条结构性保证钉死：重新 stat 必须紧邻闸门之前，且闸门只能引用新变量。
func TestRenderFilebeatInstallGuardUsesFreshStat(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())
		names := playbookTaskNames(t, art.Playbook)

		copyAt := taskIndex(t, names, "下发 filebeat.yml")
		finalStatAt := taskIndex(t, names, "启动容器前重新确认 filebeat.yml 的状态")
		guard1At := taskIndex(t, names, "闸门①：filebeat.yml 必须已存在")
		guard2At := taskIndex(t, names, "闸门②：filebeat.yml 必须是普通文件")

		// ① copy < stat(final) < assert，且 stat(final) 紧邻 assert 之前（下标相差 1）：
		//    "基于新鲜状态"最直接的结构性保证。
		if copyAt >= finalStatAt {
			t.Fatalf("%s 模式：重新 stat 必须排在 copy 之后（copy=%d, stat=%d）：\n%s",
				mode, copyAt, finalStatAt, strings.Join(names, "\n"))
		}
		if guard1At-finalStatAt != 1 {
			t.Fatalf("%s 模式：重新 stat 必须紧邻闸门之前（stat=%d, 闸门①=%d，相差应为 1）：\n%s",
				mode, finalStatAt, guard1At, strings.Join(names, "\n"))
		}
		if guard2At != guard1At+1 {
			t.Fatalf("%s 模式：两条闸门应紧邻（闸门①=%d, 闸门②=%d）：\n%s",
				mode, guard1At, guard2At, strings.Join(names, "\n"))
		}

		// ② 重新 stat 注册到新变量；闸门只引用它。
		finalStatBlock := playbookTaskBlock(t, art.Playbook, "启动容器前重新确认 filebeat.yml 的状态")
		for _, want := range []string{
			"ansible.builtin.stat:",
			"register: filebeat_config_final",
			"path: \"{{ filebeat_config_path }}\"",
		} {
			if !strings.Contains(finalStatBlock, want) {
				t.Fatalf("%s 模式的重新 stat 任务应包含 %q：\n%s", mode, want, finalStatBlock)
			}
		}
		for _, label := range []string{"闸门①：filebeat.yml 必须已存在", "闸门②：filebeat.yml 必须是普通文件"} {
			block := playbookTaskBlock(t, art.Playbook, label)
			if !strings.Contains(block, "filebeat_config_final") {
				t.Fatalf("%s 模式的%s必须引用 filebeat_config_final：\n%s", mode, label, block)
			}
			if strings.Contains(block, "filebeat_config_stat") {
				t.Fatalf("%s 模式的%s不得引用陈旧的 filebeat_config_stat（写配置之前的快照）：\n%s",
					mode, label, block)
			}
		}

		// ③ 自愈链仍用**早期**快照：两种用途不能串（合并就会退回 INC-016）。
		healStatBlock := playbookTaskBlock(t, art.Playbook, "检查 filebeat.yml 是否被占用成目录")
		if !strings.Contains(healStatBlock, "register: filebeat_config_stat") {
			t.Fatalf("%s 模式的自愈探测必须仍注册 filebeat_config_stat：\n%s", mode, healStatBlock)
		}
		removeBlock := playbookTaskBlock(t, art.Playbook, "清理非普通文件的 filebeat.yml")
		if !strings.Contains(removeBlock, "filebeat_config_stat.stat.exists and not filebeat_config_stat.stat.isreg") {
			t.Fatalf("%s 模式的自愈删除条件必须引用早期快照 filebeat_config_stat：\n%s", mode, removeBlock)
		}

		// ④ 两种失败情形要给出**不同**的、能照做的提示。
		existsMsg := playbookTaskBlock(t, art.Playbook, "闸门①：filebeat.yml 必须已存在")
		if !strings.Contains(existsMsg, "磁盘空间") || strings.Contains(existsMsg, "rm -rf") {
			t.Fatalf("%s 模式的闸门①（文件不存在）不应提示 rm -rf，而应指向 copy 失败/权限/磁盘：\n%s",
				mode, existsMsg)
		}
		isregMsg := playbookTaskBlock(t, art.Playbook, "闸门②：filebeat.yml 必须是普通文件")
		if !strings.Contains(isregMsg, "rm -rf") {
			t.Fatalf("%s 模式的闸门②（存在但非普通文件）应给出 rm -rf 的修法：\n%s", mode, isregMsg)
		}
	}
}

// TestRenderFilebeatInstallDataDirIsDedicated 锁定数据目录与系统 filebeat 分离。
//
// /var/lib/filebeat 是**系统包安装的 filebeat** 的注册表/位点目录，docker 模式是平台自己
// 托管的那套采集。两者若共用：
//   - 平台的部署会把系统 filebeat 的注册表 chown 给 1000；
//   - 两套采集器往同一个注册表里写位点；
//
// 结果是系统 filebeat 重启后重复采集或状态错乱，平台侧只表现为"日志重复/丢失"，极难定位。
// 因此 docker 模式必须用自己的数据目录，且**由平台显式创建并授权**。
func TestRenderFilebeatInstallDataDirIsDedicated(t *testing.T) {
	const wantDataDir = "/var/lib/mwops-filebeat"
	// 系统包安装的目录不可被平台占用：产物里不该出现这个字面量。
	const systemPackageDataDir = "/var/lib/filebeat"

	cases := []struct {
		mode        string
		wantCreates bool
	}{
		{LogInstallDocker, true},
		{LogInstallAuto, true}, // auto 的 docker 分支也要建（未安装且有 docker 时走它）
		{LogInstallPackage, false},
	}
	for _, tc := range cases {
		in := logTestInput()
		in.InstallMode = tc.mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())
		play := valueNode(parseYAML(t, "playbook", art.Playbook)).Content[0]

		// 断言 ①：变量值就是平台专属目录，且产物里没有系统 filebeat 的数据目录。
		if got := yamlAt(t, play, "vars.filebeat_data_dir").Value; got != wantDataDir {
			t.Fatalf("%s 模式：filebeat_data_dir 应为 %q（平台托管、与系统 filebeat 分离），实际 %q",
				tc.mode, wantDataDir, got)
		}
		if strings.Contains(art.Playbook, systemPackageDataDir) {
			t.Fatalf("%s 模式：产物不得出现系统 filebeat 的数据目录 %q（会污染它的注册表）：\n%s",
				tc.mode, systemPackageDataDir, art.Playbook)
		}

		names := playbookTaskNames(t, art.Playbook)
		dataTask := -1
		for i, name := range names {
			if strings.HasPrefix(name, "创建 Filebeat 数据目录") {
				dataTask = i
				break
			}
		}
		if !tc.wantCreates {
			// 断言 ③：package 分支不得创建这个目录，也不该有任何容器任务。
			if dataTask >= 0 {
				t.Fatalf("package 模式不应创建平台专属数据目录（系统 filebeat 自己会建自己的）：\n%s",
					strings.Join(names, "\n"))
			}
			for _, name := range names {
				if strings.Contains(name, "Filebeat 容器") || strings.Contains(name, "数据目录") {
					t.Fatalf("package 模式不应出现容器/数据目录相关任务（%q）：\n%s", name, strings.Join(names, "\n"))
				}
			}
			// 变量本身仍会渲染（vars 是全局的，供审计），只是没有任何任务碰它。
			if !strings.Contains(art.Playbook, wantDataDir) {
				t.Fatalf("package 模式的产物应保留 filebeat_data_dir 变量定义（供审计）：\n%s", art.Playbook)
			}
			continue
		}
		if dataTask < 0 {
			t.Fatalf("%s 模式必须创建平台专属数据目录（否则容器写不进 meta.json）：\n%s",
				tc.mode, strings.Join(names, "\n"))
		}
		// 断言 ②：创建任务必须带正确的权限与属主。
		dataBlock := playbookTaskBlock(t, art.Playbook, "创建 Filebeat 数据目录")
		for _, want := range []string{
			"path: \"{{ filebeat_data_dir }}\"",
			"mode: '0775'",
			"owner: '1000'",
			"group: '1000'",
		} {
			if !strings.Contains(dataBlock, want) {
				t.Fatalf("%s 模式的数据目录任务应包含 %q：\n%s", tc.mode, want, dataBlock)
			}
		}
		// 断言 ③：docker run 里挂的宿主侧必须来自变量，容器内仍是官方路径。
		run := dockerRunCommand(t, art.Playbook)
		if !strings.Contains(run, "-v \"{{ filebeat_data_dir }}:/usr/share/filebeat/data\"") {
			t.Fatalf("%s 模式：数据目录挂载应写成「{{ filebeat_data_dir }}:/usr/share/filebeat/data」，实际：\n%s",
				tc.mode, run)
		}
		if strings.Contains(run, wantDataDir+":") {
			t.Fatalf("%s 模式：docker run 的宿主侧不得写死 %q（应由变量表达）：\n%s", tc.mode, wantDataDir, run)
		}
	}
}

// TestRenderFilebeatInstallConfigPathsShareOneVariable 锁定「配置目录只有一个来源」。
//
// INC-013 的次生根因：playbook 里同时存在写死的 /etc/filebeat 与 opts.InstallDir 两种
// 「配置目录」概念，谁也没保证它存在。现在统一由 filebeat_config_path 推导出
// filebeat_config_parent_dir，并被 copy 的 dest 与「确保目录」的 path 共同引用。
func TestRenderFilebeatInstallConfigPathsShareOneVariable(t *testing.T) {
	in := logTestInput()
	art := mustRenderFilebeatInstall(t, in, logTestOptions())
	root := valueNode(parseYAML(t, "playbook", art.Playbook))
	// playbook 根是序列（play 列表），vars 在第一个 play 里。
	play := root.Content[0]

	if got := yamlAt(t, play, "vars.filebeat_config_parent_dir").Value; got != "/etc/filebeat" {
		t.Fatalf("filebeat_config_parent_dir 应为 /etc/filebeat，实际 %q", got)
	}
	if got := yamlAt(t, play, "vars.filebeat_config_path").Value; got != "/etc/filebeat/filebeat.yml" {
		t.Fatalf("filebeat_config_path 应为 /etc/filebeat/filebeat.yml，实际 %q", got)
	}
	// config_path 必须真的位于 config_parent_dir 之下：两者一旦分叉，就会出现
	// "建了 A、往 B 写"的必然失败（正是 INC-013 的次生根因）。
	parent := yamlAt(t, play, "vars.filebeat_config_parent_dir").Value
	dest := yamlAt(t, play, "vars.filebeat_config_path").Value
	if !strings.HasPrefix(dest, parent+"/") {
		t.Fatalf("config_path(%q) 必须位于 config_parent_dir(%q) 之下", dest, parent)
	}

	// 「确保目录」的 path 必须引用那个变量，不得写死路径。
	ensure := playbookTaskBlock(t, art.Playbook, "准备 Filebeat 配置目录")
	if !strings.Contains(ensure, "path: \"{{ filebeat_config_parent_dir }}\"") {
		t.Fatalf("确保目录任务必须用 filebeat_config_parent_dir 变量（写死路径会再次分叉）：\n%s", ensure)
	}
	if strings.Contains(ensure, "path: \"/etc/filebeat\"") {
		t.Fatalf("确保目录任务不得写死 /etc/filebeat：\n%s", ensure)
	}
	// copy 的 dest 必须引用 config_path 变量。
	copyBlock := playbookTaskBlock(t, art.Playbook, "下发 filebeat.yml")
	if !strings.Contains(copyBlock, "dest: \"{{ filebeat_config_path }}\"") {
		t.Fatalf("copy 的 dest 必须用 filebeat_config_path：\n%s", copyBlock)
	}
}

// TestRenderFilebeatInstallRemovesStaleContainerBeforeCopy 锁定"删旧容器"这一步及其位置（INC-014）。
//
// 现场（用户第二次执行）：PLAY RECAP `ok=11 changed=1 failed=1`，失败的仍是
// `can not use content with a dir as dest`。ok=11 恰好是修好后的前 11 条任务，
// changed=1 就是自愈任务（它确实删掉了那个目录）——**紧接着的 copy 又看到目录回来了**。
//
// 原因：第一次失败的 docker run 已经把宿主路径创建成目录，容器里的 Filebeat 读不到有效配置
// 就崩溃，`--restart=always` 让它不断重启，而 **Docker 每次启动都会把缺失的绑定源重新
// 创建成目录**。所以"自愈删目录"必须在**删掉容器之后**才有意义：
//
//	stat → absent（非普通文件）→ docker rm -f 旧容器 → copy → assert → docker run
//
// 少了 `docker rm -f`，自愈跑多少次都会被打回原形。
func TestRenderFilebeatInstallRemovesStaleContainerBeforeCopy(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())
		names := playbookTaskNames(t, art.Playbook)

		statAt := taskIndex(t, names, "检查 filebeat.yml 是否被占用成目录")
		removeAt := taskIndex(t, names, "清理非普通文件的 filebeat.yml")
		copyAt := taskIndex(t, names, "下发 filebeat.yml")
		if !(statAt < removeAt && removeAt < copyAt) {
			t.Fatalf("%s 模式：自愈链必须满足「stat(%d) < absent(%d) < copy(%d)」：\n%s",
				mode, statAt, removeAt, copyAt, strings.Join(names, "\n"))
		}
		if mode == LogInstallPackage {
			// package 模式不碰容器：产物里不该出现删容器的任务（否则会误删目标机上别人的容器）。
			for _, name := range names {
				if strings.Contains(name, "Filebeat 容器") {
					t.Fatalf("package 模式不应出现容器相关任务（%q）——package 装的是宿主机 filebeat，"+
						"删容器会把用户自己跑的容器也一起删掉：\n%s", name, strings.Join(names, "\n"))
				}
			}
			continue
		}
		rmAt := taskIndex(t, names, "删除平台托管的旧 Filebeat 容器")
		// 关键顺序：删容器必须排在 copy 之前（打断 Docker 重建绑定源目录的循环）。
		if !(removeAt < rmAt && rmAt < copyAt) {
			t.Fatalf("%s 模式：顺序必须满足「自愈(%d) < 删旧容器(%d) < copy(%d)」。"+
				"少了删容器这一步，崩溃重启的旧容器会在每次启动时把缺失的绑定源重新创建成目录，"+
				"自愈等于白做：\n%s", mode, removeAt, rmAt, copyAt, strings.Join(names, "\n"))
		}
		// 删容器必须排在 docker run 之前（否则把自己的新容器删了）。
		runAt := taskIndex(t, names, "创建 Filebeat 容器")
		if rmAt >= runAt {
			t.Fatalf("%s 模式：删旧容器(%d) 必须在 docker run(%d) 之前：\n%s",
				mode, rmAt, runAt, strings.Join(names, "\n"))
		}

		// 幂等与安全：容器不存在时 docker rm 返回非 0，属正常结果，不得中断 playbook；
		// 并且只在"这次真要走容器安装"的分支里执行。
		rmBlock := playbookTaskBlock(t, art.Playbook, "删除平台托管的旧 Filebeat 容器")
		for _, want := range []string{
			"ansible.builtin.command: docker rm -f \"{{ filebeat_container }}\"",
			"failed_when: false",
			"when:",
		} {
			if !strings.Contains(rmBlock, want) {
				t.Fatalf("%s 模式的删容器任务应包含 %q：\n%s", mode, want, rmBlock)
			}
		}
		if mode == LogInstallAuto && !strings.Contains(rmBlock, "filebeat_has_docker") {
			t.Fatalf("auto 模式的删容器任务应只在「没装 filebeat 且有 docker」时执行：\n%s", rmBlock)
		}
	}
}

// TestRenderFilebeatInstallContainerConfigPath 锁定容器内的配置路径与启动命令（INC-014）。
//
// 这是比"顺序"更根本的一条：之前挂到 /etc/filebeat/filebeat.yml，容器里的 Filebeat
// **根本不会读**那份配置（官方镜像的默认路径是 /usr/share/filebeat/filebeat.yml），
// 于是它拿镜像内置配置去连 Elasticsearch → 连不上 → 崩溃重启。
// 这类错误不会报错，只会表现为"部署成功、平台一条日志都没有"，因此必须用测试钉死。
func TestRenderFilebeatInstallContainerConfigPath(t *testing.T) {
	const wantContainerPath = "/usr/share/filebeat/filebeat.yml"
	for _, mode := range []string{LogInstallAuto, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())
		play := valueNode(parseYAML(t, "playbook", art.Playbook)).Content[0]

		// 容器内路径由变量表达（不写死两处），变量值必须是官方默认路径。
		if got := yamlAt(t, play, "vars.filebeat_container_config_path").Value; got != wantContainerPath {
			t.Fatalf("%s 模式：filebeat_container_config_path 应为 %q（官方镜像默认路径），实际 %q",
				mode, wantContainerPath, got)
		}
		runBlock := playbookTaskBlock(t, art.Playbook, "创建 Filebeat 容器")
		for _, want := range []string{
			// 宿主路径仍来自变量，容器内路径来自另一个变量（同源、单一来源）。
			"-v \"{{ filebeat_config_path }}:{{ filebeat_container_config_path }}:ro\"",
			"{{ filebeat_data_dir }}:/usr/share/filebeat/data",
			// 官方示例的三件套：root 起容器、-e 打日志、放行挂载文件的权限检查。
			"--user=root",
			"filebeat -e --strict.perms=false",
		} {
			if !strings.Contains(runBlock, want) {
				t.Fatalf("%s 模式的 docker run 应包含 %q：\n%s", mode, want, runBlock)
			}
		}
		// 数据目录必须「非 root 也能写」（INC-015）：playbook 以 become 执行时默认是
		// 0755 root:root，官方镜像默认以 filebeat(uid 1000) 运行 → meta.json.new 写入被拒 →
		// 容器起来就退出。--user=root 与显式属主是两道互补的保险，必须同时存在。
		dataBlock := playbookTaskBlock(t, art.Playbook, "创建 Filebeat 数据目录")
		for _, want := range []string{
			"path: \"{{ filebeat_data_dir }}\"",
			"mode: '0775'",
			"owner: '1000'",
			"group: '1000'",
		} {
			if !strings.Contains(dataBlock, want) {
				t.Fatalf("%s 模式的数据目录任务应包含 %q（否则 filebeat 用户写不进 meta.json）：\n%s",
					mode, want, dataBlock)
			}
		}
		if strings.Contains(dataBlock, "mode: '0755'") {
			t.Fatalf("%s 模式的数据目录不得是 0755（root:root 0755 会让非 root 运行的 filebeat 写不进去）：\n%s",
				mode, dataBlock)
		}
		// 绝不能出现"把宿主的 filebeat.yml 挂到容器 /etc/filebeat"这种旧写法。
		if strings.Contains(art.Playbook, ":/etc/filebeat/filebeat.yml") {
			t.Fatalf("%s 模式：容器内配置路径不得再是 /etc/filebeat/filebeat.yml"+
				"（容器不会读它 → 用镜像内置配置连 ES → 崩溃重启）：\n%s", mode, art.Playbook)
		}
		// 顶部注释要写清楚这条约定，避免以后有人改回去。
		if !strings.Contains(art.Playbook, "容器内配置路径固定为 "+wantContainerPath) {
			t.Fatalf("%s 模式：playbook 顶部应说明容器内配置路径的约定：\n%s", mode, art.Playbook)
		}
		// 顺序：重新 stat → assert → docker run，且 copy 必须在最前
		//（"闸门必须基于新鲜状态"见 TestRenderFilebeatInstallGuardUsesFreshStat）。
		names := playbookTaskNames(t, art.Playbook)
		copyAt := taskIndex(t, names, "下发 filebeat.yml")
		finalStatAt := taskIndex(t, names, "启动容器前重新确认 filebeat.yml 的状态")
		guardAt := taskIndex(t, names, "闸门①：filebeat.yml 必须已存在")
		runAt := taskIndex(t, names, "创建 Filebeat 容器")
		if !(copyAt < finalStatAt && finalStatAt < guardAt && guardAt < runAt) {
			t.Fatalf("%s 模式：顺序应为 copy(%d) < 重新 stat(%d) < 闸门(%d) < docker run(%d)：\n%s",
				mode, copyAt, finalStatAt, guardAt, runAt, strings.Join(names, "\n"))
		}
	}
}

// dockerRunCommand 从产物里抠出完整的 docker run 命令（跨行拼接成一行），
// 便于对这种"多行 shell 命令"做逐字断言——只看单行会漏掉换行后的挂载与参数。
func dockerRunCommand(t *testing.T, playbook string) string {
	t.Helper()
	lines := strings.Split(playbook, "\n")
	start := -1
	for i, line := range lines {
		if strings.Contains(line, "docker run -d") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("产物里找不到 docker run 命令：\n%s", playbook)
	}
	// 命令块是 YAML 块标量，每行以 \ 续行，最后一行不带 \ 即为结束。
	var parts []string
	for i := start; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		parts = append(parts, trimmed)
		if !strings.HasSuffix(trimmed, "\\") {
			break
		}
	}
	return strings.Join(parts, " ")
}

// TestRenderFilebeatInstallKafkaProbeOutputIsRobust 锁定"Kafka 探测结论的产出方式"。
//
// 真实故障 INC-027：探测任务本身 ok，但紧接着那条**只负责打印结论**的 debug 任务抛异常
// （`AttributeError: 'NoneType' object has no attribute 'group'`），把整个部署判成失败。
// 两个原因都是"写法花哨换来的坑"：
//  1. 结论标记用了 `{#MWOPS#}`：`{#` 在 Ansible 模板化 module 参数时被当成 **Jinja 注释**整段剥掉，
//     脚本实际只输出 reachable，标记永远不存在；
//  2. 解析用 `regex_search(...) | first | default(...)`：匹配不到时 regex_search 返回 None，
//     再链 `| first` 就抛异常——一条打印任务把部署搞挂了。
//
// 因此这里锁死"朴素写法"：标记必须是纯字母数字、产物里不得出现 `{#`、解析不得用正则，
// 且三种情形（可达/不通/无输出）都要能区分。
func TestRenderFilebeatInstallKafkaProbeOutputIsRobust(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		// ① 禁止 Jinja 注释标记：它会在模板化阶段被吃掉。
		if strings.Contains(art.Playbook, "{#") {
			t.Fatalf("%s 模式：产物不得出现 Jinja 注释起始标记 `{#`（会被整段剥掉，标记打不出来）", mode)
		}
		// ② 结论标记必须是纯字母数字。
		for _, marker := range []string{"MWOPS_KAFKA_REACHABLE", "MWOPS_KAFKA_UNREACHABLE"} {
			if !strings.Contains(art.Playbook, marker) {
				t.Fatalf("%s 模式：探测脚本应输出标记 %s", mode, marker)
			}
		}
		// ③ 解析不得用正则（regex_search 匹配不到会返回 None，链式过滤会抛异常）。
		if strings.Contains(art.Playbook, "regex_search") {
			t.Fatalf("%s 模式：结论解析不得使用 regex_search（匹配失败会让打印任务抛异常）", mode)
		}
		// ④ 三种情形都要能区分：可达 / 明确不通 / 探测没有输出。
		block := playbookTaskBlock(t, art.Playbook, "输出 Kafka 连通性结论")
		for _, want := range []string{"MWOPS_KAFKA_REACHABLE", "MWOPS_KAFKA_UNREACHABLE", "unknown"} {
			if !strings.Contains(block, want) {
				t.Fatalf("%s 模式：结论任务应能区分 %q 的情形：\n%s", mode, want, block)
			}
		}
		// ⑤ 后面的任务失败时也要执行已 notify 的重启 handler：
		// 否则会留下"配置已落盘、服务还用旧配置"的半应用状态（INC-027 的连带后果）。
		if !strings.Contains(art.Playbook, "force_handlers: true") {
			t.Fatalf("%s 模式：play 必须带 force_handlers: true（失败时也要应用已下发的配置）", mode)
		}
	}
}

// TestRenderFilebeatInstallDockerRunCommand 逐字锁定 docker run 命令。
//
// 这条命令是"日志集成到底跑成什么样"的最终事实来源，任何一处改动（挂载路径、用户、
// 启动参数）都会直接影响采集是否可用，而且失败往往**没有报错**（比如配置挂错路径时容器
// 只是安静地用镜像内置配置连 ES）。因此这里做一次整体断言，而不只是零散的关键字包含。
func TestRenderFilebeatInstallDockerRunCommand(t *testing.T) {
	in := logTestInput()
	in.InstallMode = LogInstallDocker
	in.Paths = []string{"/var/log/app/*.log"}
	art := mustRenderFilebeatInstall(t, in, logTestOptions())

	got := dockerRunCommand(t, art.Playbook)
	// 挂载顺序与参数顺序都是刻意固定的，因此这里可以逐字比对。
	want := `docker run -d --name "{{ filebeat_container }}" --restart=always --user=root \` +
		` -v "{{ filebeat_config_path }}:{{ filebeat_container_config_path }}:ro" \` +
		` -v "{{ filebeat_data_dir }}:/usr/share/filebeat/data" \` +
		` -v /var/log/app:/var/log/app:ro \` +
		` "{{ filebeat_image }}" \` +
		` filebeat -e --strict.perms=false`
	if got != want {
		t.Fatalf("docker run 命令与期望不符——这条命令直接决定采集是否可用，改动前请确认：\n"+
			"want: %s\n got: %s\n完整产物：\n%s", want, got, art.Playbook)
	}
}

// playbookTaskBlock 取出以 prefix 命名的任务块（下一个同级任务或 handlers 之前）。
//
// 用缩进判定任务边界：任务头是 "    - name: …"（4 空格），下一个任务头即结束。
func playbookTaskBlock(t *testing.T, playbook, prefix string) string {
	t.Helper()
	lines := strings.Split(playbook, "\n")
	start := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- name: ") && strings.Contains(trimmed, prefix) {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("找不到任务 %q：\n%s", prefix, playbook)
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "- name: ") || strings.HasPrefix(lines[i], "  handlers:") {
			end = i
			break
		}
	}
	return strings.Join(lines[start:end], "\n")
}

// TestRenderFilebeatInstallOverwriteIsDeclared 锁定「覆盖 Filebeat」开关的产物契约。
//
// 背景（用户要求）："是否覆盖 Filebeat——选择则可以覆盖 Filebeat 重新获取 docker，
// 否则如果没有才进行拉取"。语义落在**安装/拉取**这一层：
//
//	关闭（默认）：已安装就复用，不重新下载 deb/rpm、本地已有镜像也不重新拉取；
//	打开：忽略"已存在"，重新下载并强制重装（--reinstall / dnf reinstall 或 rpm --replacepkgs），
//	      docker 模式则重新 pull 镜像并重建容器。
//
// 这里逐条锁住"开关真的接进了产物"：
//  1. 开关必须是 play 变量（产物里一眼可见 true/false），而不是埋在变量文件里；
//  2. 决策需要独立成 filebeat_install_needed，避免把"目标机事实"与"平台意图"混成一个变量；
//  3. 强制重装与普通安装必须是**互斥**的两个分支（when 里带 not），
//     否则未勾选覆盖时也会重装，等于开关没接上；
//  4. 下载任务在勾选覆盖时必须重新下载（get_url force 跟随开关）。
func TestRenderFilebeatInstallOverwriteIsDeclared(t *testing.T) {
	wantFlag := map[bool]string{false: "filebeat_overwrite: false", true: "filebeat_overwrite: true"}
	for _, overwrite := range []bool{false, true} {
		in := logTestInput()
		in.InstallMode = LogInstallPackage
		in.Overwrite = overwrite
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		if !strings.Contains(art.Playbook, wantFlag[overwrite]) {
			t.Fatalf("覆盖=%v：play 变量里应声明 %q（开关必须能从产物一眼看出）：\n%s",
				overwrite, wantFlag[overwrite], art.Playbook)
		}
		// 决策变量：只在「未安装或勾选覆盖」时才安装。
		if !strings.Contains(art.Playbook, "filebeat_install_needed:") {
			t.Fatalf("覆盖=%v：应把「是否需要安装」独立成 filebeat_install_needed 事实：\n%s",
				overwrite, art.Playbook)
		}
		// 两条互斥的安装分支。
		reinstall := playbookTaskBlock(t, art.Playbook, "覆盖安装 Filebeat（已勾选「覆盖」")
		if !strings.Contains(reinstall, "(filebeat_overwrite | bool)") {
			t.Fatalf("覆盖=%v：强制重装任务必须以 filebeat_overwrite 为前提：\n%s", overwrite, reinstall)
		}
		plain := playbookTaskBlock(t, art.Playbook, "安装 Filebeat（apt 解析本地 deb 的依赖）")
		if !strings.Contains(plain, "not (filebeat_overwrite | bool)") {
			t.Fatalf("覆盖=%v：普通安装任务必须带 not (filebeat_overwrite | bool)（否则未勾选覆盖也会重装）：\n%s",
				overwrite, plain)
		}
		// 勾选覆盖时必须重新下载安装包（否则会拿 /tmp 里的旧包去"覆盖安装"）。
		download := playbookTaskBlock(t, art.Playbook, "下载 Filebeat deb 包")
		if !strings.Contains(download, "force:") {
			t.Fatalf("覆盖=%v：下载任务应带 force（跟随覆盖开关）：\n%s", overwrite, download)
		}
	}
}

// TestRenderFilebeatInstallImagePullFollowsOverwrite 锁定镜像拉取策略。
//
// 用户的原始表述就是这一条："勾选覆盖 → 重新获取 docker 镜像；否则**如果没有才拉取**"。
// 因此拉取任务的 when 必须同时满足两件事：
//   - 有 filebeat_overwrite 分支（勾选时无条件拉取）；
//   - 有本地镜像探测分支（未勾选时只在缺失时拉取）。
//
// 只写"总是拉取"会丢掉"没勾选就不出网"的省事路径；只写"缺失才拉取"会让覆盖开关对 docker 失效。
func TestRenderFilebeatInstallImagePullFollowsOverwrite(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		in := logTestInput()
		in.InstallMode = LogInstallDocker
		in.Overwrite = overwrite
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		// 先探测本地镜像（不做字符串解析：用 docker image inspect 的退出码）。
		probe := playbookTaskBlock(t, art.Playbook, "探测本地是否已有 Filebeat 镜像")
		if !strings.Contains(probe, "docker image inspect") {
			t.Fatalf("overwrite=%v：拉取前必须先探测本地镜像：\n%s", overwrite, probe)
		}
		pull := playbookTaskBlock(t, art.Playbook, "拉取 Filebeat 官方镜像")
		for _, want := range []string{"filebeat_overwrite | bool", "filebeat_image_inspect.rc"} {
			if !strings.Contains(pull, want) {
				t.Fatalf("overwrite=%v：拉取任务的 when 应包含 %q（勾选覆盖即无条件拉取，否则仅本地缺失时拉取）：\n%s",
					overwrite, want, pull)
			}
		}
		// 拉到的镜像要真正生效，容器必须重建：旧容器由"删除平台托管的旧容器"这一步清掉。
		remove := playbookTaskBlock(t, art.Playbook, "删除平台托管的旧 Filebeat 容器")
		if !strings.Contains(remove, "docker rm -f") {
			t.Fatalf("覆盖=%v：必须保留「先删旧容器」这一步（否则重新拉取的镜像不会生效）：\n%s",
				overwrite, remove)
		}
	}
}

// TestRenderFilebeatInstallExplicitModeBeatsReuse 锁定"显式安装方式优先于复用"。
//
// 真实缺陷（实现本开关时发现）：早期版本的决策是 `已安装 → reuse / docker / package`，
// **完全不看**使用者显式选的安装方式。于是"显式选 docker + 目标机恰好也装了包版 filebeat"
// 会算出 reuse，handler 便去 `systemctl restart filebeat` 而不是重建容器——
// 容器里的 Filebeat 读不到新配置，采集静默地停留在旧配置上。
//
// 因此决策的第一优先级必须是显式模式，其次才是"复用"。
func TestRenderFilebeatInstallExplicitModeBeatsReuse(t *testing.T) {
	for _, mode := range []string{LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		decision := playbookTaskBlock(t, art.Playbook, "选择实际执行的安装方式")
		explicit := "filebeat_mode == '" + mode + "'"
		if !strings.Contains(decision, explicit) {
			t.Fatalf("%s 模式：决策里必须有显式模式分支（%q），否则会被判成 reuse：\n%s",
				mode, explicit, decision)
		}
		// 显式模式分支必须排在 reuse 分支之前——顺序反了就等于复用优先。
		if strings.Index(decision, explicit) > strings.Index(decision, "reuse") {
			t.Fatalf("%s 模式：显式模式分支必须写在 reuse 之前：\n%s", mode, decision)
		}
		// 覆盖开关与复用互斥：勾选覆盖后不再有 reuse 的可能。
		if !strings.Contains(decision, "filebeat_install_needed | bool") {
			t.Fatalf("%s 模式：决策必须引用 filebeat_install_needed（勾选覆盖时不再复用）：\n%s", mode, decision)
		}
	}
}

// TestSplitHostPort 锁定探测地址的拆解（含 IPv6 与缺端口两种边界）。
func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort string
	}{
		{"10.0.0.5:9092", "10.0.0.5", "9092"},
		{"kafka.internal:19092", "kafka.internal", "19092"},
		{"10.0.0.5", "10.0.0.5", "9092"}, // 缺端口时按 Kafka 默认值
		{"[::1]:9092", "::1", "9092"},    // IPv6 要去掉方括号，否则 nc 解析不了
	}
	for _, tc := range cases {
		host, port := splitHostPort(tc.in)
		if host != tc.wantHost || port != tc.wantPort {
			t.Fatalf("splitHostPort(%q) = (%q, %q)，期望 (%q, %q)", tc.in, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

// TestRenderFilebeatInstallArtifacts 锁定三种安装方式共有的产物契约。
func TestRenderFilebeatInstallArtifacts(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		// 1) 先探测再决策（用户明确要求"如果存在则不需要部署"）。
		for _, want := range []string{"command -v filebeat", "systemctl is-active filebeat.service", "command -v docker"} {
			if !strings.Contains(art.Playbook, want) {
				t.Fatalf("%s 模式应先探测目标机状态（%q），否则会重复安装/覆盖已有 Filebeat：\n%s",
					mode, want, art.Playbook)
			}
		}
		// 2) 配置下发必须 copy + notify，重启只能由 handler 承担。
		if !strings.Contains(art.Playbook, "ansible.builtin.copy") {
			t.Fatalf("%s 模式应通过 copy 下发配置：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.Playbook, "notify: 重启 Filebeat") {
			t.Fatalf("%s 模式的下发任务必须 notify 重启 handler：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.Playbook, "handlers:") || !strings.Contains(art.Playbook, "重启 Filebeat") {
			t.Fatalf("%s 模式缺少 restart handler：\n%s", mode, art.Playbook)
		}
		// 3) 配置内容走变量文件：内联进 playbook 会被 Ansible 当 Jinja 渲染（INC-005/INC-006 同类坑）。
		if !strings.Contains(art.Playbook, "content: \"{{ filebeat_config_content }}\"") {
			t.Fatalf("%s 模式的内联配置应取自变量文件：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.VarsFile, "filebeat_config_content: |") {
			t.Fatalf("%s 模式的 vars 文件应含块标量形式的 filebeat.yml：\n%s", mode, art.VarsFile)
		}

		// 4) 末尾的两条尽力而为校验（失败不阻断，但输出要打到日志）。
		for _, want := range []string{"filebeat test config", "filebeat test output"} {
			if !strings.Contains(art.VarsFile, want) {
				t.Fatalf("%s 模式应包含自检 %q（用于抓 advertised 地址配错这类问题）：\n%s", mode, want, art.VarsFile)
			}
		}
		verifyAt := strings.Index(art.Playbook, "校验 filebeat.yml 配置")
		if verifyAt < 0 {
			t.Fatalf("%s 模式缺少自检任务：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.Playbook[verifyAt:], "failed_when: false") {
			t.Fatalf("%s 模式的自检失败不得让整个 playbook 失败（安装已经成功，自检是信息）：\n%s",
				mode, art.Playbook[verifyAt:])
		}

		// 5) 产物元数据：日志集成没有 Exporter 端口。
		if art.Target != "10.0.0.21" {
			t.Fatalf("%s 模式：日志集成的 Target 应是主机本身，实际 %q", mode, art.Target)
		}
		if art.UnitName != FilebeatUnitName {
			t.Fatalf("%s 模式：UnitName 应为 %s，实际 %q", mode, FilebeatUnitName, art.UnitName)
		}
		if art.ContainerName != "mwops-filebeat" {
			t.Fatalf("%s 模式：ContainerName 应为 mwops-filebeat，实际 %q", mode, art.ContainerName)
		}

		// 6) 安全约定：playbook 不含 SSH 口令，展示用 inventory 用占位符。
		if strings.Contains(art.Playbook, "ssh-secret") {
			t.Fatalf("%s 模式：playbook 不得包含 SSH 口令：\n%s", mode, art.Playbook)
		}
		if !strings.Contains(art.MaskedInventory, "${SSH_PASSWORD}") {
			t.Fatalf("%s 模式：展示用 inventory 应使用占位符：\n%s", mode, art.MaskedInventory)
		}
		if !strings.Contains(art.Inventory, "ansible_password=ssh-secret") {
			t.Fatalf("%s 模式：真实 inventory 应含 SSH 口令（0600、用完即删）", mode)
		}
		if strings.Contains(art.RunCommand, "ssh-secret") {
			t.Fatalf("%s 模式：执行命令不得包含凭据：%s", mode, art.RunCommand)
		}

		// 7) 渲染器自校验必须通过（与既有 Exporter 产物同一道防线）。
		if err := validatePlaybookYAML("日志集成", art.Playbook); err != nil {
			t.Fatalf("%s 模式：playbook 未通过平台自校验：%v", mode, err)
		}
		if err := ensureFilebeatVarsParsable("日志集成", art.VarsFile); err != nil {
			t.Fatalf("%s 模式：vars 文件未通过平台自校验：%v", mode, err)
		}
	}
}

// TestRenderFilebeatInstallHasNoUnquotedJinja 锁定产物里不得出现裸 {{ }}。
//
// 真实故障 INC-005/INC-006：playbook 里以 {{ 开头的值会被 YAML 当 flow mapping
// （ansible 报 unhashable type），行中的 Go 模板语法会被当 Jinja 表达式
// （unexpected '.'）。日志集成新增了 docker run 多行命令与自检命令，两处都容易踩。
func TestRenderFilebeatInstallHasNoUnquotedJinja(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())
		// 复用既有测试的扫描逻辑：冒号后的值若以 {{ 开头必须带引号。
		assertJinjaValuesQuoted(t, "日志集成/"+mode, art.Playbook)
		for i, line := range strings.Split(art.Playbook, "\n") {
			if strings.Contains(line, "{{.") || strings.Contains(line, "{{ .") {
				t.Fatalf("日志集成/%s 第 %d 行含 Go 模板语法（会被 Ansible 当 Jinja 渲染而报错）：%s",
					mode, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestRenderFilebeatInstallModes 锁定三种安装方式各自的分支特征。
//
// auto 必须同时包含 package 与 docker 两条分支：它要能在"目标机已有 docker"和
// "目标机只有发行版包管理器"两类机器上用同一份产物跑通（用 when 条件二选一）。
func TestRenderFilebeatInstallModes(t *testing.T) {
	cases := []struct {
		mode      string
		wantAny   []string
		wantNone  []string
		checkAuto bool
	}{
		{
			mode: LogInstallPackage,
			wantAny: []string{
				"/etc/debian_version",                           // 发行版判定不依赖 gather_facts
				"artifacts.elastic.co",                          // deb/rpm 来源
				"ansible.builtin.apt",                           // debian
				"dnf install",                                   // redhat
				"ansible.builtin.systemd",                       // 启用 systemd 单元
				"https://artifacts.elastic.co/packages/8.x/apt", // 仓库兜底
			},
			// 明确选了 package 就不该出现"真的去跑容器"的命令（vars 里留一个镜像变量不算：
			// 产物里保留它是为了让同一份渲染器三种模式的变量表一致）。
			wantNone: []string{"docker pull", "docker run -d"},
		},
		{
			mode: LogInstallDocker,
			wantAny: []string{
				"docker.elastic.co/beats/filebeat:8.16.0",
				"docker run -d",
				"--restart=always",
				// 容器内配置路径必须与官方镜像默认一致（INC-014），宿主路径仍走变量。
				"{{ filebeat_config_path }}:{{ filebeat_container_config_path }}:ro",
				"--user=root",
				"filebeat -e --strict.perms=false",
			},
			wantNone: []string{"ansible.builtin.apt", "dnf install", ":/etc/filebeat/filebeat.yml"},
		},
		{
			mode:      LogInstallAuto,
			wantAny:   []string{"docker run -d", "dnf install", "docker.elastic.co/beats/filebeat:8.16.0", "artifacts.elastic.co"},
			checkAuto: true,
		},
	}
	for _, tc := range cases {
		in := logTestInput()
		in.InstallMode = tc.mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())
		for _, want := range tc.wantAny {
			if !strings.Contains(art.Playbook, want) {
				t.Fatalf("%s 模式应包含 %q：\n%s", tc.mode, want, art.Playbook)
			}
		}
		// 只在**任务区**里做"不应出现"的判定：handler 里同时保留了 systemd 与 docker
		// 两条重启路径（filebeat_mode 决定走哪条），那是刻意为之，不算"跑错安装路径"。
		tasks := art.Playbook
		if idx := strings.Index(tasks, "  handlers:\n"); idx >= 0 {
			tasks = tasks[:idx]
		}
		for _, none := range tc.wantNone {
			if strings.Contains(tasks, none) {
				t.Fatalf("%s 模式的任务区不应包含 %q（安装路径必须明确，不能让使用者以为选了 A 实际跑了 B）：\n%s",
					tc.mode, none, tasks)
			}
		}
		// 三种模式的 handler 都必须同时覆盖 systemd 与 docker 两条重启路径：
		// 用前面 set_fact 算出的实际方式（filebeat_tested_mode）二选一，
		// 避免"配置变了却没重启"（那会让平台安静地收不到日志）。
		handlers := art.Playbook
		if idx := strings.Index(handlers, "  handlers:\n"); idx >= 0 {
			handlers = handlers[idx:]
		}
		for _, want := range []string{"systemctl restart", "docker rm -f", "filebeat_tested_mode"} {
			if !strings.Contains(handlers, want) {
				t.Fatalf("%s 模式的 handler 应覆盖 %q（重启路径必须与安装路径匹配）：\n%s", tc.mode, want, handlers)
			}
		}
		if !tc.checkAuto {
			continue
		}
		// auto 的决策顺序：已安装 → docker → package，三条分支都必须写在产物里。
		for _, want := range []string{
			"filebeat_present | bool",          // 复用已安装
			"filebeat_has_docker | bool",       // 其次容器
			"not (filebeat_has_docker | bool)", // 最后包安装
		} {
			if !strings.Contains(art.Playbook, want) {
				t.Fatalf("auto 模式应包含分支条件 %q（已装 → docker → package）：\n%s", want, art.Playbook)
			}
		}
	}
}

// TestRenderFilebeatInstallRestartsOnlyThroughHandler 锁定"重启只由 handler 触发"。
//
// 这一条是幂等的关键：ansible.builtin.copy 默认按 checksum 比对，
// 配置内容不变时任务报 ok、**不会**通知 handler，因此重复重放 playbook 不会重启 Filebeat、
// 采集不抖动；一旦把 restart 写进普通任务（而不是 handler），每次重放都会重启一次。
func TestRenderFilebeatInstallRestartsOnlyThroughHandler(t *testing.T) {
	for _, mode := range []string{LogInstallAuto, LogInstallPackage, LogInstallDocker} {
		in := logTestInput()
		in.InstallMode = mode
		art := mustRenderFilebeatInstall(t, in, logTestOptions())

		split := strings.SplitN(art.Playbook, "  handlers:\n", 2)
		if len(split) != 2 {
			t.Fatalf("%s 模式应包含 handlers 段：\n%s", mode, art.Playbook)
		}
		tasks, handlers := split[0], split[1]
		// 任务区里出现 state: restarted / docker restart 都意味着"每次重放都重启"。
		for _, forbidden := range []string{"state: restarted", "docker restart"} {
			if strings.Contains(tasks, forbidden) {
				t.Fatalf("%s 模式的任务区不应出现 %q（重启必须交给 handler，由 copy 的 checksum 决定是否触发）：\n%s",
					mode, forbidden, tasks)
			}
		}
		// handler 里必须有真正的重启动作（否则"配置变了"这件事不会生效）。
		if !strings.Contains(handlers, "restart") {
			t.Fatalf("%s 模式的 handler 应重启 Filebeat：\n%s", mode, handlers)
		}
		// 配置变更 -> 重启 这条链路的说明必须留在产物里（下一个接手的人才知道为什么这么写）。
		if !strings.Contains(art.Playbook, "内容不变时不重启采集") {
			t.Fatalf("%s 模式的配置下发任务应说明 checksum 语义：\n%s", mode, art.Playbook)
		}
	}
}

// TestRenderFilebeatInstallConfigMountsLogDirs 锁定 docker 模式下的日志目录挂载。
//
// 容器只能看见挂载进来的路径：少挂一个目录 = 那批日志永远采不到，
// 而 Filebeat 只会安静地"没有匹配到文件"（docker logs 里连报错都没有）。
func TestRenderFilebeatInstallConfigMountsLogDirs(t *testing.T) {
	in := logTestInput()
	in.InstallMode = LogInstallDocker
	in.Paths = []string{"/var/log/app/*.log", "/data/logs/**/*.log"}
	art := mustRenderFilebeatInstall(t, in, logTestOptions())
	for _, want := range []string{
		"-v /var/log/app:/var/log/app:ro",
		"-v /data/logs:/data/logs:ro",
		// 配置挂载：宿主路径走变量，容器内路径必须与官方镜像默认一致（INC-014）。
		"-v \"{{ filebeat_config_path }}:{{ filebeat_container_config_path }}:ro\"",
	} {
		if !strings.Contains(art.Playbook, want) {
			t.Fatalf("docker 模式应挂载 %q：\n%s", want, art.Playbook)
		}
	}
	// 顶层目录（/var、/data）绝不整体挂进容器。
	for _, forbidden := range []string{"-v /var:/var", "-v /data:/data", "-v /:/"} {
		if strings.Contains(art.Playbook, forbidden) {
			t.Fatalf("docker 模式不得整体挂载 %q（会把宿主其它内容暴露给容器）：\n%s", forbidden, art.Playbook)
		}
	}
}

// TestRenderFilebeatInstallValidation 锁定安装产物的失败路径。
func TestRenderFilebeatInstallValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*LogInput, *RemoteOptions)
		wantMsg string
	}{
		{
			name:    "日志路径为空",
			mutate:  func(in *LogInput, _ *RemoteOptions) { in.Paths = nil },
			wantMsg: "日志路径不能为空",
		},
		{
			name:    "Kafka 地址为空",
			mutate:  func(in *LogInput, _ *RemoteOptions) { in.KafkaHosts = nil },
			wantMsg: "Kafka 地址不能为空",
		},
		{
			name:    "topic 为空",
			mutate:  func(in *LogInput, _ *RemoteOptions) { in.Topic = "" },
			wantMsg: "Kafka topic 不能为空",
		},
		{
			name:    "安装方式非法",
			mutate:  func(in *LogInput, _ *RemoteOptions) { in.InstallMode = "helm" },
			wantMsg: "不合法",
		},
		{
			name:    "目标机地址为空",
			mutate:  func(_ *LogInput, opts *RemoteOptions) { opts.Host = " " },
			wantMsg: "目标服务器地址",
		},
		{
			name:    "SSH 用户名为空",
			mutate:  func(_ *LogInput, opts *RemoteOptions) { opts.SSHUser = "" },
			wantMsg: "SSH 用户名",
		},
	}
	for _, tc := range cases {
		in := logTestInput()
		opts := logTestOptions()
		tc.mutate(&in, &opts)
		if _, err := RenderFilebeatInstall(in, opts); err == nil {
			t.Fatalf("%s：应报错（否则会产出一份跑不通/装错东西的 playbook）", tc.name)
		} else if !strings.Contains(err.Error(), tc.wantMsg) {
			t.Fatalf("%s：错误信息应包含 %q，实际：%v", tc.name, tc.wantMsg, err)
		}
	}
}

// TestLogTemplateRegistration 锁定日志模板的注册信息与"不带 Exporter 语义"。
func TestLogTemplateRegistration(t *testing.T) {
	tpl, ok := TemplateOf(TypeLog)
	if !ok {
		t.Fatal("日志集成（log）模板应存在——否则前端新建集成时选不到 Filebeat")
	}
	if tpl.Component != "filebeat" || tpl.Name == "" || tpl.Description == "" {
		t.Fatalf("日志模板的基础信息不完整：%+v", tpl)
	}
	if tpl.Phase != 1 {
		t.Fatalf("日志集成应为 Phase 1（纳管 + 采集），实际 %d", tpl.Phase)
	}
	if tpl.CategoryOf() != CategoryLog || tpl.Category != CategoryLog {
		t.Fatalf("日志模板的分类应为 %s：%+v", CategoryLog, tpl)
	}
	if tpl.NeedsAuth {
		t.Fatal("日志集成不需要账号口令（Filebeat 只往 Kafka 推数据，凭据由平台侧 KAFKA_* 决定）")
	}
	// Exporter 语义的字段必须全部留空：填任何值都会让前端渲染出一个并不存在的 Exporter。
	if tpl.Image != "" || tpl.ExporterPort != 0 || tpl.MetricsPath != "" || tpl.Release != nil {
		t.Fatalf("日志集成不得带 Exporter 相关字段（Image/ExporterPort/MetricsPath/Release）：%+v", tpl)
	}
	if len(tpl.Alerts) != 0 {
		t.Fatalf("日志集成不产生 Prometheus 告警规则：%+v", tpl.Alerts)
	}
	if tpl.Dashboard.ID != "" || tpl.Dashboard.Title != "" {
		t.Fatalf("日志集成没有 Grafana 大盘：%+v", tpl.Dashboard)
	}
	if tpl.AddressLabel == "" || tpl.AddressHint == "" {
		t.Fatal("日志集成需要地址栏文案（本机也填 127.0.0.1，统一走 SSH 安装）")
	}

	// 日志模板必须放行"端口为 0"：它的地址是**服务器地址**，没有服务端口概念
	// （端口属于 SSH，由凭据字段承担），服务层传的就是 Address.Port = 0。
	// 这里锁住的是"使用者不会看到一个自己也填不出来的必填项"。
	if err := tpl.Validate(Instance{
		Name: "app-log-01", MWType: TypeLog, Environment: "prod",
		Address: Address{Host: "10.0.0.21", Port: 0},
	}); err != nil {
		t.Fatalf("日志集成的地址没有服务端口，Port=0 必须被放行，实际被拒：%v", err)
	}
	// 但"地址为空"仍然要拦（否则产出的 playbook 连目标机都不知道在哪）。
	if err := tpl.Validate(Instance{
		Name: "app-log-01", MWType: TypeLog, Environment: "prod",
		Address: Address{Host: "", Port: 0},
	}); err == nil {
		t.Fatal("日志集成的地址不能为空（需填目标服务器地址）")
	}
	// 名称与标签校验对日志集成同样生效（不能因为分类不同就放松）。
	if err := tpl.Validate(Instance{
		Name: "App_Log", MWType: TypeLog, Address: Address{Host: "10.0.0.21", Port: 0},
	}); err == nil {
		t.Fatal("日志集成仍需遵守集成名规范")
	}
	if err := tpl.Validate(Instance{
		Name: "app-log-01", MWType: TypeLog,
		Address: Address{Host: "10.0.0.21", Port: 0},
		Labels:  map[string]string{"instance": "x"},
	}); err == nil {
		t.Fatal("日志集成仍需拒绝覆盖平台保留标签")
	}
	// 反过来：指标监控模板的端口校验不能被这次改动放松（0 仍然非法）。
	redisTpl, _ := TemplateOf(TypeRedis)
	if err := redisTpl.Validate(Instance{
		Name: "redis-01", MWType: TypeRedis, Address: Address{Host: "10.0.0.11", Port: 0},
	}); err == nil {
		t.Fatal("指标监控模板的端口必须仍在 1-65535 之间（日志模板的例外不得外溢）")
	}

	// Options 必须覆盖渲染器实际读取的每一个字段（缺一个就意味着使用者没地方填）。
	wantOptions := map[string]string{
		"MWOPS_LOG_PATHS":             "string",
		"MWOPS_LOG_SERVICE":           "string",
		"MWOPS_LOG_ENVIRONMENT":       "string",
		"MWOPS_LOG_LEVEL":             "string",
		"MWOPS_LOG_MULTILINE":         "bool",
		"MWOPS_LOG_MULTILINE_PATTERN": "string",
		"MWOPS_LOG_INSTALL_MODE":      "string",
		"MWOPS_LOG_OVERWRITE":         "bool",
		"MWOPS_LOG_FILEBEAT_VERSION":  "string",
	}
	got := make(map[string]Option, len(tpl.Options))
	for _, opt := range tpl.Options {
		got[opt.Key] = opt
		if opt.Target != TargetEnv {
			t.Fatalf("参数 %s 应通过环境变量落地（TargetEnv），实际 %s", opt.Key, opt.Target)
		}
		if opt.Label == "" {
			t.Fatalf("参数 %s 缺少中文标签（前端表单要显示）", opt.Key)
		}
	}
	for key, kind := range wantOptions {
		opt, ok := got[key]
		if !ok {
			t.Fatalf("日志模板缺少参数 %s（渲染器读了它，使用者却没地方填）", key)
		}
		if opt.Kind != kind {
			t.Fatalf("参数 %s 的 Kind 应为 %s，实际 %s（决定前端控件）", key, kind, opt.Kind)
		}
	}
	// 默认值必须与渲染器的兜底一致，否则"表单显示的值"和"实际落地的值"会对不上。
	for key, want := range map[string]string{
		"MWOPS_LOG_ENVIRONMENT": "dev",
		"MWOPS_LOG_LEVEL":       LogLevelError,
		"MWOPS_LOG_MULTILINE":   "true",
		// 默认安装方式刻意是 package（不是 auto）：auto 在目标机有 Docker 时会走容器模式，
		// 而容器模式的坑最多（容器运行用户 / 宿主数据目录属主 / 镜像约定的配置路径 /
		// Docker 把缺失的绑定源创建成目录），真实环境里连着暴露了四轮（INC-024 / INC-026）。
		// 需要容器化采集时由使用者显式选 auto/docker。
		"MWOPS_LOG_INSTALL_MODE":     "package",
		"MWOPS_LOG_FILEBEAT_VERSION": "8.16.0",
		// 覆盖 Filebeat 是**破坏性**开关（会重新下载并覆盖目标机上已有的 Filebeat），
		// 因此默认必须是 false：把缺省写成"打开"等于让每次部署都去重装别人机器上的软件。
		"MWOPS_LOG_OVERWRITE": "false",
	} {
		if got[key].Default != want {
			t.Fatalf("参数 %s 的默认值应为 %q，实际 %q", key, want, got[key].Default)
		}
	}

	// Notes 必须写清三个最容易踩的坑（这三条都是"平台看起来正常但收不到日志"的典型）。
	notes := strings.Join(tpl.Notes, "\n")
	for _, want := range []string{
		"advertised",       // ① Kafka 对外地址配错：连上后立刻断开
		"127.0.0.1:9092",   //    典型报错原文
		"InvalidTimestamp", // ② 目标机时钟偏移（NTP）被 Kafka 拒收
		"幂等",               // ③ 已安装的不重复安装
	} {
		if !strings.Contains(notes, want) {
			t.Fatalf("模板 Notes 应说明 %q 这个坑：\n%s", want, notes)
		}
	}
}

// TestTemplatesExposeCategory 锁定分类字段的对外行为。
//
// 既有 8 个模板必须兜底成 monitor（它们的字段值一个字都不能改），
// 日志模板必须是 log —— 前端据此决定展示哪组表单与哪条落地链路。
func TestTemplatesExposeCategory(t *testing.T) {
	seenLog := false
	for _, tpl := range Templates() {
		if tpl.Category == "" {
			t.Fatalf("Templates() 返回的 %s 缺少 category（前端只能自己猜）", tpl.Type)
		}
		switch tpl.Type {
		case TypeLog:
			seenLog = true
			if tpl.Category != CategoryLog {
				t.Fatalf("日志模板应为 %s，实际 %s", CategoryLog, tpl.Category)
			}
		default:
			if tpl.Category != CategoryMonitor {
				t.Fatalf("%s 应兜底为 %s（既有模板不得因新增分类而改变语义），实际 %s",
					tpl.Type, CategoryMonitor, tpl.Category)
			}
		}
	}
	if !seenLog {
		t.Fatal("模板列表里应有日志集成（log）")
	}
	// 自定义/未知类型也走兜底：分类只由 Type 决定，不依赖调用方填字段。
	if got := (Template{Type: TypeRedis}).CategoryOf(); got != CategoryMonitor {
		t.Fatalf("Redis 应归入 %s，实际 %s", CategoryMonitor, got)
	}
	if got := (Template{Type: TypeLog}).CategoryOf(); got != CategoryLog {
		t.Fatalf("log 应归入 %s，实际 %s", CategoryLog, got)
	}
}
