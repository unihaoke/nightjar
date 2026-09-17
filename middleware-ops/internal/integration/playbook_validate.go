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
// 变更渲染模板时请同步 +1，并在 docs/POSTMORTEM.md 里记录原因。
const PlaybookRendererVersion = "mwops-playbook v2"

// yamlLinePattern 从 yaml.v3 的错误文本里抠出行号（形如 "yaml: line 33: ..."）。
var yamlLinePattern = regexp.MustCompile(`line (\d+)`)

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
