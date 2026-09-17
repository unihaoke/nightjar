package service

import (
	"os/exec"
	"strings"
	"testing"
)

// 本文件锁定「平台侧是否具备口令登录目标机的能力」这条前置检查（INC-007）。
//
// 真实故障：ansible 的 ssh 连接插件把口令交给 OpenSSH，而 OpenSSH 不接受命令行口令，
// 必须由 sshpass 代答。镜像里缺 sshpass 时，ansible 只在远端执行阶段抛
//
//	to use the 'ssh' connection type with passwords or pkcs11_provider,
//	you must install the sshpass program
//
// 与使用者填写的内容毫无关系，也看不出问题在平台侧。这里把它前置成可操作的提示。

func TestCheckSSHPassWithoutSSHPass(t *testing.T) {
	restore := lookPath
	defer func() { lookPath = restore }()
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }

	svc := &IntegrationService{}
	err := svc.checkSSHPass(RemoteCreds{User: "root", Password: "pw"})
	if err == nil {
		t.Fatal("平台缺 sshpass 且使用口令认证时必须拦下，否则只会在目标机上抛 ansible 的原话")
	}
	// 三个要素：说清缺什么、给出两条出路、且不泄露口令。
	for _, want := range []string{"sshpass", "docker compose build backend", "私钥"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误信息应包含 %q，实际：%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "pw") {
		t.Fatalf("错误信息不得回显口令：%v", err)
	}
}

func TestCheckSSHPassSkipsKeyAuth(t *testing.T) {
	restore := lookPath
	defer func() { lookPath = restore }()
	called := false
	lookPath = func(string) (string, error) { called = true; return "", exec.ErrNotFound }

	svc := &IntegrationService{}
	// 私钥认证不经过 sshpass：缺 sshpass 也必须放行。
	if err := svc.checkSSHPass(RemoteCreds{User: "root", Key: "-----BEGIN OPENSSH PRIVATE KEY-----"}); err != nil {
		t.Fatalf("私钥认证不应要求 sshpass：%v", err)
	}
	if called {
		t.Fatal("私钥认证时不该去查 sshpass（徒增依赖判断）")
	}
}

func TestCheckSSHPassWithSSHPass(t *testing.T) {
	restore := lookPath
	defer func() { lookPath = restore }()
	lookPath = func(string) (string, error) { return "/usr/bin/sshpass", nil }

	svc := &IntegrationService{}
	if err := svc.checkSSHPass(RemoteCreds{User: "root", Password: "pw"}); err != nil {
		t.Fatalf("平台有 sshpass 时应放行：%v", err)
	}
}
