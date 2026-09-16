package integration

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Artifacts 是一次集成最终要落地的产物。
//
// 每一份产物都对应一种落地方式，平台会把它们全部展示给使用者：
//   - FileSD     ：写入 Prometheus 的 file_sd 目标文件，**无需改动抓取配置**（推荐）；
//   - ScrapeJob  ：显式 scrape_configs 片段，file_sd 不便接入时的替代方案；
//   - Compose    ：Exporter 的 docker compose 服务片段，平台不代为部署时使用；
//   - DeployCmd  ：等价的 docker run 命令，便于一次性手工验证。
type Artifacts struct {
	JobName     string            `json:"job_name"`
	Targets     []string          `json:"targets"`
	Labels      map[string]string `json:"labels"`
	FileSD      string            `json:"file_sd"`
	ScrapeJob   string            `json:"scrape_job"`
	Compose     string            `json:"compose"`
	DeployCmd   string            `json:"deploy_cmd"`
	Selector    string            `json:"selector"`
	VerifySteps []string          `json:"verify_steps"`
}

// FileSDEntry 是 file_sd 文件里的一条目标记录。
type FileSDEntry struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

// RenderFileSD 把多条目标渲染为完整的 file_sd JSON 文档。
//
// Prometheus 的 file_sd_configs 会周期性重读该文件，因此新增/修改集成**不需要**
// 重启或 reload Prometheus，这也是本平台选择 file_sd 作为主通道的原因。
func RenderFileSD(entries []FileSDEntry) (string, error) {
	// 固定顺序输出，保证同样的输入生成逐字节相同的文件（便于审计与 diff）。
	sorted := append([]FileSDEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return labelKey(sorted[i].Labels) < labelKey(sorted[j].Labels)
	})
	if sorted == nil {
		sorted = []FileSDEntry{}
	}
	payload, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		return "", fmt.Errorf("渲染 file_sd 失败: %w", err)
	}
	return string(payload) + "\n", nil
}

// labelKey 生成排序键。
func labelKey(labels map[string]string) string {
	return labels["instance_name"] + "|" + labels["mw_type"]
}

// LabelsFor 构造写入 Prometheus 的标签集合。
//
// instance_name 是平台定位实例的唯一依据（internal/monitor 的 buildSelector），
// 因此必须与集成名称完全一致；自定义标签最后覆盖，但保留标签已在服务层拦下。
func LabelsFor(in Instance) map[string]string {
	labels := map[string]string{
		"instance_name": in.Name,
		"mw_type":       in.MWType,
	}
	if in.Environment != "" {
		labels["env"] = in.Environment
	}
	if in.GroupName != "" {
		labels["group"] = in.GroupName
	}
	for key, value := range in.Labels {
		labels[key] = value
	}
	return labels
}

// EntryFor 构造该集成的 file_sd 记录。
func EntryFor(in Instance) FileSDEntry {
	return FileSDEntry{Targets: []string{in.Address.HostPort()}, Labels: LabelsFor(in)}
}

// Render 渲染一次集成的全部产物。
//
// jobName 为 Prometheus 里的 file_sd 抓取任务名（默认 middleware-integration），
// sdFilePath 为 Prometheus 容器内看到的 file_sd 文件路径，
// network 为 Exporter 容器需要加入的 docker 网络（需与 Prometheus 同网络）。
func Render(tpl Template, in Instance, jobName, sdFilePath, network string) (Artifacts, error) {
	if err := tpl.Validate(in); err != nil {
		return Artifacts{}, err
	}
	if strings.TrimSpace(jobName) == "" {
		jobName = "middleware-integration"
	}
	env := tpl.RenderEnv(in)
	args := tpl.RenderArgs(in)

	entry := EntryFor(in)
	fileSD, err := RenderFileSD([]FileSDEntry{entry})
	if err != nil {
		return Artifacts{}, err
	}

	return Artifacts{
		JobName:     jobName,
		Targets:     entry.Targets,
		Labels:      entry.Labels,
		FileSD:      fileSD,
		ScrapeJob:   renderScrapeJob(tpl, in, jobName),
		Compose:     renderCompose(tpl, in, env, args, network),
		DeployCmd:   renderDeployCmd(tpl, in, env, args, network),
		Selector:    SelectorFor(in, jobName),
		VerifySteps: verifySteps(tpl, in, jobName, sdFilePath),
	}, nil
}

// SelectorFor 返回平台查询该集成指标时使用的 PromQL 选择器。
func SelectorFor(in Instance, jobName string) string {
	if strings.TrimSpace(jobName) == "" {
		jobName = "middleware-integration"
	}
	return fmt.Sprintf(`job="%s",instance_name="%s"`, jobName, in.Name)
}

// renderScrapeJob 渲染显式 scrape_configs 片段（file_sd 的替代方案）。
func renderScrapeJob(tpl Template, in Instance, jobName string) string {
	labels := LabelsFor(in)
	var b strings.Builder
	b.WriteString("  # 集成：" + in.Name + "（" + tpl.Name + "）\n")
	b.WriteString("  # 与 file_sd 方案二选一：同时配置会产生重复序列。\n")
	b.WriteString("  - job_name: " + yamlScalar(jobName) + "\n")
	if tpl.MetricsPath != "" && tpl.MetricsPath != "/metrics" {
		b.WriteString("    metrics_path: " + yamlScalar(tpl.MetricsPath) + "\n")
	}
	b.WriteString("    static_configs:\n")
	b.WriteString("      - targets: [" + yamlScalar(in.Address.HostPort()) + "]\n")
	b.WriteString("        labels:\n")
	for _, key := range sortedKeys(labels) {
		b.WriteString("          " + key + ": " + yamlScalar(labels[key]) + "\n")
	}
	return b.String()
}

// renderCompose 渲染 Exporter 的 compose 服务片段（口令以变量占位，不落明文）。
func renderCompose(tpl Template, in Instance, env map[string]string, args []string, network string) string {
	var b strings.Builder
	container := ContainerName(in.Name)
	b.WriteString("  # 集成：" + in.Name + "（" + tpl.Name + "）—— 由平台「集成中心」生成\n")
	b.WriteString("  # 合并进 docker-compose.yml 后执行：docker compose up -d " + container + "\n")
	b.WriteString("  " + container + ":\n")
	b.WriteString("    image: " + tpl.Image + "\n")
	b.WriteString("    container_name: " + container + "\n")
	b.WriteString("    restart: unless-stopped\n")
	if len(env) > 0 {
		b.WriteString("    environment:\n")
		for _, key := range sortedKeys(env) {
			b.WriteString("      " + key + ": " + yamlScalar(maskSecret(env[key], in.Password)) + "\n")
		}
	}
	if len(args) > 0 {
		b.WriteString("    command:\n")
		for _, arg := range args {
			b.WriteString("      - " + yamlScalar(maskSecret(arg, in.Password)) + "\n")
		}
	}
	// 多网络：compose 片段里逐条列出（第一个是监控面，其余是数据面）。
	networks := splitNetworks(network)
	if len(networks) > 0 {
		b.WriteString("    networks:\n")
		for _, item := range networks {
			b.WriteString("      - " + item + "\n")
		}
	}
	return b.String()
}

// splitNetworks 解析逗号分隔的网络名。
func splitNetworks(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// renderDeployCmd 渲染等价的 docker run 命令。
func renderDeployCmd(tpl Template, in Instance, env map[string]string, args []string, network string) string {
	container := ContainerName(in.Name)
	parts := []string{
		"docker run -d",
		"--name " + container,
		"--restart unless-stopped",
	}
	for _, item := range splitNetworks(network) {
		parts = append(parts, "--network "+item)
	}
	for _, key := range sortedKeys(env) {
		parts = append(parts, "-e "+key+"="+shellQuote(maskSecret(env[key], in.Password)))
	}
	parts = append(parts, tpl.Image)
	for _, arg := range args {
		parts = append(parts, maskSecret(arg, in.Password))
	}
	return strings.Join(parts, " ") + "\n"
}

// verifySteps 生成手工核对步骤（从 Exporter 到平台逐层收敛）。
func verifySteps(tpl Template, in Instance, jobName, sdFilePath string) []string {
	target := in.Address.HostPort()
	steps := []string{
		fmt.Sprintf("确认 Exporter 容器在运行：docker ps --filter name=%s", ContainerName(in.Name)),
		fmt.Sprintf("直连 Exporter 拉取原始指标：curl -s http://<exporter-host>:%d%s | head", tpl.ExporterPort, metricsPath(tpl)),
		fmt.Sprintf("Prometheus 目标页确认该 target 为 up：抓取任务 %s（file_sd：%s，refresh_interval 到期后自动生效）", jobName, sdFilePath),
		fmt.Sprintf("Prometheus 侧直接验证标签：%s", SelectorFor(in, jobName)),
		"平台「中间件纳管 → 实例详情 → 接入自检」应显示 matched 大于 0，且来源为 Prometheus",
	}
	if tpl.ExporterPort > 0 {
		steps = append(steps, fmt.Sprintf("注意：被管实例地址 %s 与 Exporter 端口 %d 是两个概念——Prometheus 抓的是 Exporter，不是中间件本身", target, tpl.ExporterPort))
	}
	return steps
}

// metricsPath 返回抓取路径。
func metricsPath(tpl Template) string {
	if tpl.MetricsPath == "" {
		return "/metrics"
	}
	return tpl.MetricsPath
}

// maskSecret 把明文口令替换成变量占位，避免生成的配置里出现明文密钥。
func maskSecret(value, password string) string {
	if password == "" {
		return value
	}
	masked := strings.ReplaceAll(value, password, "${MONITOR_PASSWORD}")
	// URL 中的口令已被替换，但 DSN 里可能还有 user:pass 形式，这里再兜一层。
	return masked
}

// yamlScalar 按需给 YAML 标量加引号。
func yamlScalar(value string) string {
	if value == "" {
		return `""`
	}
	safe := true
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '.', r == '_', r == '/', r == ':', r == '@', r == '=', r == ',', r == '*':
		default:
			safe = false
		}
	}
	// ": " 会让 YAML 把它当成嵌套映射，必须加引号；以 - / * 开头会被当成序列或别名。
	if strings.Contains(value, ": ") || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "*") {
		safe = false
	}
	if safe {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// shellQuote 为 shell 命令加单引号。
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// sortedKeys 返回排序后的 map 键。
func sortedKeys(items map[string]string) []string {
	keys := make([]string, 0, len(items))
	for key := range items {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
