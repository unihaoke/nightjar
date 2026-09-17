package service

import (
	"strings"
	"testing"
)

// 锁定 no_log 屏蔽失败原因时的排查提示（INC-008）。
//
// 含密任务必须 no_log（否则口令随 module args 回显），代价是失败结果被整段替换成
// censored —— 使用者只看到一行没有信息量的报错。平台侧补一句"下一步看什么"。

func TestCensoredHintOnlyForCensoredOutput(t *testing.T) {
	censored := `TASK [写入 Exporter 环境变量（含口令，权限 0600）] ***
fatal: [10.0.0.9]: FAILED! => {"censored": "the output has been hidden due to the fact that 'no_log: true' was specified for this result", "changed": false}`
	hint := censoredHint(censored)
	if hint == "" {
		t.Fatal("输出被 no_log 屏蔽时必须给出排查指引，否则使用者只能看到 censored")
	}
	for _, want := range []string{"no_log", "ls -ld", "df -h"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("提示应包含 %q，实际：%s", want, hint)
		}
	}
	// 不得把口令带进提示。
	if strings.Contains(hint, "password") || strings.Contains(hint, "pw") {
		t.Fatalf("提示不得涉及口令：%s", hint)
	}

	// 可读的失败不需要这段提示（避免噪音）。
	readable := `fatal: [10.0.0.9]: FAILED! => {"msg": "Destination directory /opt/mwops-exporter does not exist"}`
	if got := censoredHint(readable); got != "" {
		t.Fatalf("可读失败不应追加 censored 提示：%s", got)
	}
}
