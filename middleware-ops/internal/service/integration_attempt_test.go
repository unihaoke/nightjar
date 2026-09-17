package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// 本文件锁定「下一步该点哪个按钮」的判定，以及重新应用/账号操作共用的 SSH 凭据契约。
//
// 背景（真实反馈）：
//  1. 远程集成点「重新应用」必然失败——它不带 SSH 凭据就发起安装，留下一条
//     "远程安装需要 SSH 凭据"的待处理项；使用者填好私钥保存后，保存接口返回的 last_error
//     仍是上一次的旧错误（新尝试在后台跑），界面于是先弹旧错误，像是修复没生效。
//  2. 待处理项横幅一律把人引到账号弹窗，让人以为「重新应用」与「重试建号」重复。

func TestClassifyNextAction(t *testing.T) {
	cases := []struct {
		name      string
		lastError string
		want      NextAction
	}{
		{
			"远程安装缺凭据 → 点重新应用（远程会先问凭据）",
			"重建 Exporter失败：远程安装需要 SSH 凭据（用户名 + 口令或私钥）",
			NextActionReapply,
		},
		{
			"安装过程失败 → 点重新应用",
			"重建 Exporter失败：Ansible 执行失败：exit status 2（unknown flag）",
			NextActionReapply,
		},
		{
			"容器未就绪/端口不通 → 点重新应用",
			"Exporter 未跑通：平台探测 203.195.191.75:9121 失败：connection refused",
			NextActionReapply,
		},
		{
			"账号口令不一致 → 去重试建号（即使句子里同时出现 Exporter）",
			"Exporter 未跑通（up=0）：认证失败（NOAUTH），请核对监控账号口令",
			NextActionRetryAccount,
		},
		{
			"账号还没建 → 去重试建号",
			"该实例的只读监控账号尚未由平台创建：到「监控账号」点「重试建号」",
			NextActionRetryAccount,
		},
		{
			"权限不足 → 去重试建号",
			"建号失败：SQLSTATE 42000 access denied for user 'root'",
			NextActionRetryAccount,
		},
		{
			"原因不明 → 引导看诊断",
			"未采集到指标样本",
			NextActionInvestigate,
		},
		{"没有失败 → 不提示动作", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyNextAction(c.lastError); got != c.want {
				t.Fatalf("classifyNextAction(%q) = %q, want %q", c.lastError, got, c.want)
			}
		})
	}
}

func TestNextActionOfProvidesLabel(t *testing.T) {
	value, label := nextActionOf("远程安装需要 SSH 凭据")
	if value != string(NextActionReapply) {
		t.Fatalf("应引导到重新应用，实际 %q", value)
	}
	if !strings.Contains(label, "重新应用") {
		t.Fatalf("按钮文案应说明动作，实际 %q", label)
	}
	// 空错误不产生按钮，避免界面上出现一个没有指向的按钮。
	if value, label := nextActionOf(""); value != "" || label != "" {
		t.Fatalf("空错误不应给出动作：%q/%q", value, label)
	}
}

// TestSSHCredsInputContract 锁定与前端约定的字段名与"口令/私钥二选一"语义。
//
// 字段名一旦改动，前端（sshPayload）就会静默失效：后端收到空凭据 → 远程操作全部报缺凭据。
func TestSSHCredsInputContract(t *testing.T) {
	var in SSHCredsInput
	raw := `{"ssh_user":"root","ssh_port":2222,"ssh_key":"-----BEGIN OPENSSH PRIVATE KEY-----"}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if in.SSHUser != "root" || in.SSHPort != 2222 || !strings.Contains(in.SSHKey, "BEGIN OPENSSH") {
		t.Fatalf("字段名与前端约定不一致：%+v", in)
	}
	if !in.provided() {
		t.Fatal("只给私钥也应算提供了凭据（私钥认证不需要口令）")
	}
	creds := in.remoteCreds("10.0.0.9")
	if creds.User != "root" || creds.Port != 2222 || creds.Key == "" || creds.Host != "10.0.0.9" {
		t.Fatalf("凭据转换不正确：%+v", creds)
	}

	// 口令方式。
	var pw SSHCredsInput
	_ = json.Unmarshal([]byte(`{"ssh_user":"ops","ssh_password":"p"}`), &pw)
	if !pw.provided() || pw.remoteCreds("h").Password != "p" {
		t.Fatalf("口令方式应可用：%+v", pw)
	}

	// 缺用户名 / 两种认证都没给 → 不算提供。
	if (SSHCredsInput{SSHPassword: "p"}).provided() {
		t.Fatal("没有用户名不算提供凭据")
	}
	if (SSHCredsInput{SSHUser: "ops"}).provided() {
		t.Fatal("只有用户名（没有口令也没有私钥）不算提供凭据")
	}
}
