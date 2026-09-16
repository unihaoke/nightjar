// Package integration —— 日志接入：从被管容器的 docker 配置反查日志位置。
//
// 设计前提（用户明确要求）：
//
//	**位置必须能从 docker 配置里读到；读不到就不允许配置**，绝不猜一个路径。
//	因为猜错的后果是"采集容器起来了但永远没有日志"，比直接报错更难排查。
//
// 反查依据按可信度从高到低：
//  1. 容器环境变量指向的日志目录（LOG_PATH / LOG_DIR / LOG_HOME / LOGS_DIR / LOGGING_FILE_PATH），
//     且该目录确实被某个卷或宿主目录覆盖（否则采集容器挂不到它）；
//  2. 挂载点的容器内路径或卷名/宿主路径里含 "log"（不区分大小写）；
//  3. 都命中不到 → 返回 ok=false，调用方必须拒绝保存。
package integration

import (
	"path"
	"sort"
	"strings"
)

// MountInfo 是一个挂载点（与 internal/docker 解耦，便于单测）。
type MountInfo struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// logPathEnvs 是各语言/框架常见的日志目录环境变量名。
//
// jd（Spring Boot + logback）用的是 LOG_PATH；其余为通用兜底。
var logPathEnvs = []string{
	"LOG_PATH", "LOG_DIR", "LOG_HOME", "LOGS_DIR", "LOG_FOLDER", "LOGGING_FILE_PATH",
}

// LogSource 是"从哪里采日志"的完整答案。
type LogSource struct {
	// Container 为被管容器名（从 docker 读到的真实名字）。
	Container string `json:"container"`
	// Networks 为该容器所在网络——采集容器要接到同一张上（跨 compose 也能收到日志？不需要，
	// 但保持一致便于排查；真正必需的是它要能访问平台后端）。
	Networks []string `json:"networks"`
	// Dir 为容器内日志目录（如 /app/data/logs）。
	Dir string `json:"dir"`
	// MountTarget 为采集容器内的挂载路径（平台固定用 /logs）。
	MountTarget string `json:"mount_target"`
	// MountSource 为要挂载的来源：命名卷名（volume）或宿主路径（bind）。
	MountSource string `json:"mount_source"`
	// MountKind 取值 volume / bind。
	MountKind string `json:"mount_kind"`
	// MountSpec 是可直接交给 docker 的挂载串（"<source>:/logs:ro"）。
	MountSpec string `json:"mount_spec"`
	// Glob 为采集用的文件通配（在采集容器内看，如 /logs/*.log）。
	Glob string `json:"glob"`
	// Evidence 说明"凭什么认为这里是日志目录"，供使用者核对。
	Evidence []string `json:"evidence"`
}

// logMountTarget 是采集容器内固定的挂载点。
const logMountTarget = "/logs"

// DiscoverLogSource 从容器详情推断日志位置。
//
// env 形如 ["LOG_PATH=/app/data/logs", "TZ=Asia/Shanghai"]。
// 返回 ok=false 时，调用方必须拒绝配置并给出"未从 docker 配置发现日志目录"的原因。
func DiscoverLogSource(container string, env []string, mounts []MountInfo, networks []string) (LogSource, bool) {
	envMap := make(map[string]string, len(env))
	for _, item := range env {
		if idx := strings.Index(item, "="); idx > 0 {
			envMap[strings.ToUpper(strings.TrimSpace(item[:idx]))] = strings.TrimSpace(item[idx+1:])
		}
	}

	source := LogSource{Container: container, Networks: networks, MountTarget: logMountTarget}

	// ① 环境变量指明的日志目录：必须被某个挂载覆盖，否则采集容器挂不到它。
	for _, key := range logPathEnvs {
		dir := strings.TrimSpace(envMap[key])
		if dir == "" {
			continue
		}
		if mount, ok := mountCovering(mounts, dir); ok {
			fillMount(&source, mount, dir)
			source.Evidence = []string{"容器环境变量 " + key + "=" + dir, "该目录由 " + describeMount(mount) + " 提供"}
			return source, true
		}
		source.Evidence = append(source.Evidence, "环境变量 "+key+"="+dir+" 未被任何卷/宿主目录覆盖，无法采集")
	}

	// ② 路径或卷名里含 log（不区分大小写）。
	candidates := make([]MountInfo, 0, len(mounts))
	for _, mount := range mounts {
		if looksLikeLogMount(mount) {
			candidates = append(candidates, mount)
		}
	}
	if len(candidates) > 0 {
		// 多个候选时取路径最深/名字最像日志的那个，保证结果稳定。
		sort.SliceStable(candidates, func(i, j int) bool {
			return logScore(candidates[i]) > logScore(candidates[j])
		})
		chosen := candidates[0]
		fillMount(&source, chosen, chosen.Destination)
		source.Evidence = append(source.Evidence, "挂载点匹配到日志特征："+describeMount(chosen))
		if len(candidates) > 1 {
			for _, other := range candidates[1:] {
				source.Evidence = append(source.Evidence, "（另有候选挂载："+describeMount(other)+"，如需改用它请在页面手填）")
			}
		}
		return source, true
	}
	return source, false
}

// fillMount 把挂载点信息填进 LogSource。
func fillMount(source *LogSource, mount MountInfo, dir string) {
	source.Dir = dir
	source.MountKind = mount.Type
	if mount.Type == "bind" {
		source.MountSource = mount.Source
	} else {
		source.MountSource = mount.Name
	}
	source.MountSpec = source.MountSource + ":" + logMountTarget + ":ro"
	source.Glob = path.Join(logMountTarget, "*.log")
}

// mountCovering 找出覆盖指定容器内路径的挂载点。
//
// "覆盖"= 挂载点就是该目录，或该目录位于挂载点之下（如挂载 /app/data、日志写 /app/data/logs）。
func mountCovering(mounts []MountInfo, dir string) (MountInfo, bool) {
	cleaned := path.Clean(dir)
	best := MountInfo{}
	found := false
	for _, mount := range mounts {
		target := path.Clean(mount.Destination)
		if target == "" || target == "." {
			continue
		}
		if cleaned == target || strings.HasPrefix(cleaned, target+"/") {
			// 取最长的匹配（最贴近日志目录的那一层）
			if !found || len(target) > len(path.Clean(best.Destination)) {
				best = mount
				found = true
			}
		}
	}
	return best, found
}

// looksLikeLogMount 判断挂载点是否像日志目录。
func looksLikeLogMount(mount MountInfo) bool {
	return strings.Contains(strings.ToLower(mount.Destination), "log") ||
		strings.Contains(strings.ToLower(mount.Name), "log") ||
		strings.Contains(strings.ToLower(mount.Source), "log")
}

// logScore 给候选挂载打分，用于在多个候选中稳定排序。
func logScore(mount MountInfo) int {
	score := 0
	dest := strings.ToLower(mount.Destination)
	if strings.HasSuffix(dest, "/logs") || strings.HasSuffix(dest, "/log") {
		score += 10
	}
	if strings.Contains(dest, "log") {
		score += 5
	}
	if strings.Contains(strings.ToLower(mount.Name), "log") {
		score += 3
	}
	if strings.Contains(strings.ToLower(mount.Source), "log") {
		score += 2
	}
	score += len(strings.Split(strings.Trim(dest, "/"), "/"))
	return score
}

// describeMount 输出挂载点的人可读描述。
func describeMount(mount MountInfo) string {
	switch mount.Type {
	case "bind":
		return "宿主目录 " + mount.Source + " → 容器内 " + mount.Destination
	case "volume":
		return "命名卷 " + mount.Name + " → 容器内 " + mount.Destination
	default:
		return mount.Destination
	}
}
