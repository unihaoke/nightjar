package repo

import (
	"os/exec"
	"strings"
	"testing"
)

// TestProbeGitBinary 覆盖启动自检的主路径：git 在 PATH 中时应返回版本号。
func TestProbeGitBinary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("本机未安装 git，跳过自检用例")
	}
	out, err := ProbeGitBinary("")
	if err != nil {
		t.Fatalf("git 可用时自检不应失败: %v", err)
	}
	if !strings.Contains(out, "git version") {
		t.Fatalf("自检输出不像 git --version: %q", out)
	}
}

// TestProbeGitBinaryMissing 覆盖「镜像里没装 git」这一真实故障：
// 自检必须报错并给出可操作的修法，而不是返回空成功。
func TestProbeGitBinaryMissing(t *testing.T) {
	out, err := ProbeGitBinary("definitely-not-a-git-binary")
	if err == nil {
		t.Fatalf("可执行文件不存在时自检应当报错，实际输出: %q", out)
	}
	if !strings.Contains(err.Error(), "definitely-not-a-git-binary") {
		t.Fatalf("错误信息应指出是哪个可执行文件不可用: %v", err)
	}
	if !strings.Contains(err.Error(), "allow_outbound") {
		t.Fatalf("错误信息应给出关闭能力的修法（本次失败要让运维看懂）: %v", err)
	}
}
