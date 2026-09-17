package integration

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PlaybookRendererVersion 是 playbook 渲染器的版本号，会写进产物第 3 行注释。
//
// 用途：远程安装失败时，先看目标文件里的这一行就能判断
// 「平台镜像里跑的是哪一版渲染器」——排查 INC-005 时正是靠它区分
// 「模板写错」与「后端镜像没重建、仍在跑旧代码」两种情况。
//
// 版本历史：
//   - v2：YAML 引号修复（INC-005）+ 渲染后自校验 + 版本戳；
//   - v3：去掉 docker 的 Go 模板格式串（INC-006）、systemd 单元改用 docker 绝对路径、
//     INSTALLED_FROM 改块标量；
//   - v4：公共前置任务「准备 Exporter 配置目录」（INC-008），env 文件改为不做 shell 引号转义。
//
// 变更渲染模板时请同步 +1，并在 docs/POSTMORTEM.md 里记录原因。
const PlaybookRendererVersion = "mwops-playbook v4"

// yamlLinePattern 从 yaml.v3 的错误文本里抠出行号（形如 "yaml: line 33: ..."）。
var yamlLinePattern = regexp.MustCompile(`line (\d+)`)

var (
	// rawBlockPattern 匹配 {% raw %}…{% endraw %}（含 Jinja 的空白控制写法 {%- raw -%}）。
	rawBlockPattern = regexp.MustCompile(`(?s)\{%-?\s*raw\s*-?%\}.*?\{%-?\s*endraw\s*-?%\}`)
	// goTemplatePattern 匹配以点号开头的 Go 模板取值：{{.Field}} / {{ .Field }}。
	// 不匹配 `{{-`（那是 Jinja 的空白控制，合法）。
	goTemplatePattern = regexp.MustCompile(`\{\{\s*\.`)
)

// validatePlaybookYAML 在把 playbook 交给 ansible-playbook 之前先自己校验一遍。
//
// 背景（真实故障 INC-005）：模板里生成了 `port: {{ exporter_port }}`。
// YAML 会把标量位置的行首 `{{` 当成 flow mapping，PyYAML 直接拒绝，
// ansible 只在执行阶段抛
//
//	ERROR! ... found unacceptable key (unhashable type: 'AnsibleMapping')
//
// 使用者拿到的是一句与根因毫无关系的报错，且整个「建号 + 拉起 Exporter」流程全废。
// 这里提前校验，把「第几行、原文是什么、为什么错」直接返回给界面。
func validatePlaybookYAML(kind, playbook string) error {
	if line, text, ok := firstUnquotedJinjaValue(playbook); ok {
		return fmt.Errorf("平台生成的 %s playbook 第 %d 行不是合法 YAML："+
			"以 {{ 开头的值必须加引号（这属于平台模板缺陷，请提交工单）：\n  %d | %s",
			kind, line, line, text)
	}
	if line, text, ok := firstGoTemplateValue(playbook); ok {
		return fmt.Errorf("平台生成的 %s playbook 第 %d 行含 Go 模板语法（%s）："+
			"playbook 里的 {{ }} 会先被 Ansible 当 Jinja 表达式渲染，行首的点号不是合法表达式，"+
			"执行时会报 template error while templating string: unexpected '.'。"+
			"请去掉该 --format 格式串，或用 {%% raw %%}…{%% endraw %%} 包裹（这属于平台模板缺陷，请提交工单）",
			kind, line, text)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(playbook), &doc); err != nil {
		msg := err.Error()
		if line := yamlErrorLine(msg); line > 0 {
			return fmt.Errorf("平台生成的 %s playbook 第 %d 行不是合法 YAML（这属于平台模板缺陷，请提交工单）：%s\n  %d | %s",
				kind, line, firstLine(msg), line, sourceLine(playbook, line))
		}
		return fmt.Errorf("平台生成的 %s playbook 不是合法 YAML（这属于平台模板缺陷，请提交工单）：%s", kind, firstLine(msg))
	}
	return nil
}

// firstUnquotedJinjaValue 找出「冒号后的值以裸 {{ 开头」的行。
//
// 只检查标量起始位置：shell/command 的值里出现 {{ 是合法的
// （如 `docker pull {{ exporter_image }}`），YAML 不会把行中的 {{ 当成 flow mapping。
func firstUnquotedJinjaValue(playbook string) (int, string, bool) {
	for i, line := range strings.Split(playbook, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(trimmed, ": ")
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(trimmed[idx+2:])
		if strings.HasPrefix(value, "{{") {
			return i + 1, trimmed, true
		}
	}
	return 0, "", false
}

// firstGoTemplateValue 找出未用 {raw} 包裹的 Go 模板语法 `{{.X}}`。
//
// 真实故障 INC-006：模板里写了 `docker version --format '{{.Server.Version}}'`。
// 该行 YAML 完全合法，ansible 也解析通过，但**渲染模板**阶段会把 `{{.Server.Version}}`
// 当 Jinja 表达式求值，行首的点号直接报
//
//	template error while templating string: unexpected '.'
//
// 于是「校验 Docker 可用」这一步就失败，后面的建号与安装全部没跑。
func firstGoTemplateValue(playbook string) (int, string, bool) {
	// {% raw %} 块内的内容 ansible 不做模板渲染，是唯一合法的写法：
	// 先把整块替换成等量换行（保持行号），再扫描剩余部分。
	masked := rawBlockPattern.ReplaceAllStringFunc(playbook, func(block string) string {
		return strings.Repeat("\n", strings.Count(block, "\n"))
	})
	lines := strings.Split(masked, "\n")
	original := strings.Split(playbook, "\n")
	for i, line := range lines {
		if goTemplatePattern.MatchString(line) {
			return i + 1, strings.TrimSpace(original[i]), true
		}
	}
	return 0, "", false
}

// yamlErrorLine 返回 yaml.v3 错误文本里的行号，取不到时返回 0。
func yamlErrorLine(msg string) int {
	m := yamlLinePattern.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0
	}
	line, convErr := strconv.Atoi(m[1])
	if convErr != nil {
		return 0
	}
	return line
}

// sourceLine 返回第 line 行原文（越界返回空串）。
func sourceLine(text string, line int) string {
	lines := strings.Split(text, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	return strings.TrimRight(lines[line-1], "\r")
}

// firstLine 取多行错误的首行，避免把 yaml.v3 的整段提示灌进界面。
func firstLine(text string) string {
	if idx := strings.IndexByte(text, '\n'); idx >= 0 {
		return text[:idx]
	}
	return text
}
