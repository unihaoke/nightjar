package service

import (
	"strings"
	"testing"
)

// 本文件锁定「ansible 输出的失败摘要」（INC-009）。
//
// 真实故障：平台只截取输出的**前 600 个字符**，而 ansible 是从前往后打印的，
// 失败一定在尾部——界面上于是只剩一串成功任务的 ok/changed，真正的原因一个字都没有。

func TestFailureExcerptKeepsFailingTask(t *testing.T) {
	output := `PLAY [安装并启动 Exporter（redis_exporter）] ***

TASK [准备 Exporter 配置目录] ***
changed: [203.195.191.75]

TASK [校验目标机器已安装 docker] ***
ok: [203.195.191.75]

TASK [写入 Exporter 环境变量（含口令，权限 0600）] ***
changed: [203.195.191.75]

TASK [拉取官方镜像] ***
FAILED - RETRYING: 拉取官方镜像 (3 retries left).
FAILED - RETRYING: 拉取官方镜像 (2 retries left).
fatal: [203.195.191.75]: FAILED! => {"changed": false, "cmd": "docker pull oliver006/redis_exporter:v1.66.0", "msg": "Error response from daemon: Get \"https://registry-1.docker.io/v2/\": net/http: request canceled while waiting for connection", "stderr": "…", "stdout": ""}

PLAY RECAP ***
203.195.191.75 : ok=3 changed=2 unreachable=0 failed=1 skipped=0 rescued=0 ignored=0`

	got := ansibleFailureExcerpt(output, 900)
	for _, want := range []string{"TASK [拉取官方镜像]", "FAILED - RETRYING", "Error response from daemon", "成功的任务已省略"} {
		if !strings.Contains(got, want) {
			t.Fatalf("摘要应包含 %q：\n%s", want, got)
		}
	}
	// 失败点之前的成功任务不该占位置（那正是原来的问题）。
	if strings.Contains(got, "准备 Exporter 配置目录") {
		t.Fatalf("摘要应从前一个任务开始，不该包含更早的任务：\n%s", got)
	}
	if strings.Contains(got, "PLAY RECAP") {
		t.Fatalf("摘要应在 PLAY RECAP 前结束：\n%s", got)
	}
}

func TestFailureExcerptMultilineMessage(t *testing.T) {
	output := `TASK [缺少客户端与 docker 时明确失败（不擅自改目标机软件包）] ***
fatal: [10.0.0.9]: FAILED! => {"changed": false, "msg": "目标主机上既没有 mysql 客户端，也没有 docker\n请二选一：① 安装客户端；② 取消勾选代建账号"}

PLAY RECAP ***
10.0.0.9 : ok=1 changed=0 unreachable=0 failed=1`

	got := ansibleFailureExcerpt(output, 900)
	if !strings.Contains(got, "请二选一") {
		t.Fatalf("多行 msg 的后续行也应带上：\n%s", got)
	}
}

func TestFailureExcerptWithoutMarkerKeepsHeadAndTail(t *testing.T) {
	var b strings.Builder
	b.WriteString("第一行很重要\n")
	for i := 0; i < 300; i++ {
		b.WriteString("中间填充行 middle filler line\n")
	}
	b.WriteString("最后一行才是结论\n")

	got := ansibleFailureExcerpt(b.String(), 400)
	if !strings.Contains(got, "第一行很重要") || !strings.Contains(got, "最后一行才是结论") {
		t.Fatalf("没有失败标记时应头尾都给：\n%s", got)
	}
	if len(got) > 500 {
		t.Fatalf("摘要不应超过限制：%d", len(got))
	}
}

func TestFailureExcerptShortOutputUnchanged(t *testing.T) {
	short := "TASK [x] ***\nfatal: [h]: FAILED! => {\"msg\": \"boom\"}"
	if got := ansibleFailureExcerpt(short, 900); got != short {
		t.Fatalf("短输出应原样返回：%q", got)
	}
}

func TestFailureExcerptCensoredTask(t *testing.T) {
	output := `TASK [写入 Exporter 环境变量（含口令，权限 0600）] ***
fatal: [10.0.0.9]: FAILED! => {"censored": "the output has been hidden due to the fact that 'no_log: true' was specified for this result", "changed": false}

PLAY RECAP ***
10.0.0.9 : ok=2 changed=1 unreachable=0 failed=1`

	got := ansibleFailureExcerpt(output, 900)
	if !strings.Contains(got, "写入 Exporter 环境变量") || !strings.Contains(got, "censored") {
		t.Fatalf("被遮蔽的失败也要指出是哪个任务：\n%s", got)
	}
	// 与 censoredHint 配合：摘要说清"哪个任务"，提示说清"下一步看什么"。
	if hint := censoredHint(output); hint == "" {
		t.Fatal("censored 输出应同时给出排查指引")
	}
}
