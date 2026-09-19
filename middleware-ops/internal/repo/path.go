package repo

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// 本文件实现本地路径安全（硬性要求 1）。
//
// 威胁模型：Service 名可能来自用户输入（服务注册、日志事件里的 service 字段），
// 一旦直接拼进 filepath.Join，`../../etc/cron.d/x` 这类值就能把 clone 目标
// 写到缓存根目录之外——既可能覆盖平台文件，也可能让后续代码读取越权。
// 因此这里有两道防线：净化目录名 + RootDir 归属校验（含符号链接解析）。

// dirNameUnsafe 匹配不允许出现在缓存子目录名里的字符（含 / 与 \，它们会被替换掉）。
var dirNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitizeServiceName 把服务名净化为「单层、安全」的目录名。
//
// 规则：
//   - 只允许 [A-Za-z0-9._-]，其余字符（含路径分隔符）统一替换为 "-"；
//   - 去掉首尾的 "." 与 "-"（避免 "."、".."、不可见形态，也避免 Windows 上
//     目录名结尾为点被系统静默裁掉导致路径与实际目录不一致）；
//   - 净化结果为 ""、"."、".." 时直接拒绝：它们是相对路径语义，
//     进入 filepath.Join 就会跳出 RootDir。
//
// 合法名字（order-service、api.v2_1）净化前后不变。
func sanitizeServiceName(service string) (string, error) {
	name := strings.TrimSpace(service)
	if name == "" {
		return "", fmt.Errorf("%w：Service 为空，无法推导缓存子目录名（可改用 Request.LocalPath 显式指定）", ErrInvalidService)
	}
	name = dirNameUnsafe.ReplaceAllString(name, "-")
	name = strings.Trim(name, ".-")
	switch name {
	case "", ".", "..":
		return "", fmt.Errorf("%w：Service %q 净化后为 %q，属于空名或保留目录名（拒绝越过缓存根目录）", ErrInvalidService, service, name)
	}
	return name, nil
}

// withinDir 判断 target 是否严格落在 root 之内（不含 root 自身）。
//
// 用 filepath.Rel 而不是字符串前缀比较，原因：字符串前缀会把
// /app/data/repos-evil 误判成 /app/data/repos 的子目录（前缀相同但不在目录内）。
// 在 Windows 上 Rel 按大小写不敏感比较盘符与路径分量，符合系统语义。
func withinDir(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." || rel == "" {
		return false // root 自身不算「之内」：不能把缓存根目录当某个服务的代码目录
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}

// resolveExisting 对「已存在的最长前缀」做符号链接解析，再拼回剩余部分。
//
// 必要性：LocalPath 可能指向 RootDir 内的一个软链接，而它实际指向 RootDir 之外
// （例如 root/svc -> /etc）。只做字符串归属校验会放行，实际写入却在外面。
// 首次 clone 时目标路径还不存在，所以只能解析已存在的部分。
func resolveExisting(path string) string {
	if path == "" {
		return path
	}
	p := filepath.Clean(path)
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			if suffix == "" {
				return resolved
			}
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return path // 到达根仍不可解析（权限等），退回原值由调用方判断
		}
		suffix = filepath.Join(filepath.Base(p), suffix)
		p = parent
	}
}

// probePath 返回目标路径的 (是否存在, 是否是 git 仓库)。
//
// 存在的判定用 os.Stat（跟随软链接）：同名文件、空目录、软链接都算「已存在」，
// 用于触发「拒绝覆盖」而不是「直接 clone」。git 仓库的判定是 .git 存在，
// 它是目录（普通仓库）或文件（worktree/submodule）都算。
func probePath(dir string) (bool, bool, error) {
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("repo: 检查本地目录 %s 失败: %w", dir, err)
	}
	if !info.IsDir() {
		return true, false, nil // 同名文件：按「已存在且不是仓库」处理，绝不覆盖
	}
	_, statErr := os.Stat(filepath.Join(dir, ".git"))
	return true, statErr == nil, nil
}

// isGitDir 判断目录是否是 git 仓库（.git 可能是目录，也可能是 worktree 的文件）。
func isGitDir(dir string) bool {
	_, isRepo, err := probePath(dir)
	return err == nil && isRepo
}
