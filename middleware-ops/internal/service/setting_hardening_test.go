package service

import (
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
)

// 本文件锁定「平台自管设置」上线时新引入的两个不变量（此前配置只在启动时读一次，不存在这两类问题）：
//
//  1. 通知配置**保存即生效**：设置服务必须把新配置推给通知服务，否则界面显示"已保存"，
//     告警却仍然按启动时的旧 webhook 发——这类"看起来成功了"的失效最难排查；
//  2. 运行期改配置不能与发送链路抢内存：通知服务持有的是自己那份原子快照，且切片要防御性拷贝
//     （否则调用方一改切片，快照跟着变，等于没换）。
//
// 另外锁定审计脱敏：审计长期保存且可检索，凭据绝不能进库。

// TestRedactCredentials 校验 base_url 里的 userinfo 一定会被打码，且路径里的 @ 不被误伤。
func TestRedactCredentials(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通地址不变", "https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"空串", "", ""},
		{"用户名口令打码", "https://user:pass@gateway.internal/v1", "https://***:***@gateway.internal/v1"},
		{"只有 token 也打码", "https://sk-abc123@host/v1", "https://***:***@host/v1"},
		{"无端口无路径", "http://admin:secret@10.0.0.1", "http://***:***@10.0.0.1"},
		{"路径里的 @ 不动", "https://host/v1/@me", "https://host/v1/@me"},
		{"没有 scheme 时整段打码（无法区分用户名与协议名）", "user:pass@host", "***:***@host"},
		{"带端口的凭据只保留主机", "https://u:p@host:8443/v1/chat", "https://***:***@host:8443/v1/chat"},
	}
	for _, c := range cases {
		got := redactCredentials(c.in)
		if got != c.want {
			t.Fatalf("%s: redactCredentials(%q) = %q，期望 %q", c.name, c.in, got, c.want)
		}
		for _, leak := range []string{"user:pass", "sk-abc123", "admin:secret", "u:p@"} {
			if strings.Contains(got, leak) {
				t.Fatalf("%s: 脱敏结果仍含凭据片段 %q：%q", c.name, leak, got)
			}
		}
	}
}

// TestNotifierConfigSnapshotIsolatedAndLive 校验通知服务的配置快照既能"立即生效"，
// 又不会被外部结构体的后续修改悄悄带跑偏。
func TestNotifierConfigSnapshotIsolatedAndLive(t *testing.T) {
	mem := config.NotifyConfig{
		Enabled: true,
		Feishu:  config.WebhookChannelConfig{Enabled: true, Webhook: "https://hook/old"},
	}
	notifier := NewNotifierService(&mem, "http://platform", nil, nil, zap.NewNop())

	if err := notifier.ReadyForTest("feishu"); err != nil {
		t.Fatalf("初始配置应可发送：%v", err)
	}
	// 通知服务持有的是副本：外部（config.Config 里的字段）被改掉不应影响它。
	mem.Feishu.Webhook = ""
	if err := notifier.ReadyForTest("feishu"); err != nil {
		t.Fatalf("通知服务不应跟随外部结构体的后续修改：%v", err)
	}

	// 保存后必须立即生效：webhook 被清空就该报"未配置"，而不是继续用旧值。
	notifier.UpdateConfig(config.NotifyConfig{Enabled: true, Feishu: config.WebhookChannelConfig{Enabled: true}})
	if err := notifier.ReadyForTest("feishu"); err == nil {
		t.Fatal("UpdateConfig 之后 webhook 为空，ReadyForTest 应当报错（说明保存没生效）")
	}

	// 切片必须防御性拷贝：调用方改自己的切片不能改写已生效的快照。
	to := []string{"ops@example.com"}
	notifier.UpdateConfig(config.NotifyConfig{
		Enabled: true,
		Email:   config.EmailChannelConfig{Enabled: true, Host: "smtp.internal", To: to},
	})
	to[0] = "attacker@example.com"
	if got := notifier.current().Email.To[0]; got != "ops@example.com" {
		t.Fatalf("快照应拷贝切片，实际取到 %q", got)
	}
}

// TestApplyNotifyPayloadReachesNotifier 是"保存后没生效"这类问题的回归守卫：
// 设置服务保存时必须把配置推给通知服务（只改内存 cfg 是不够的）。
func TestApplyNotifyPayloadReachesNotifier(t *testing.T) {
	notifier := NewNotifierService(&config.NotifyConfig{}, "http://platform", nil, nil, zap.NewNop())
	svc := &SettingService{cfg: &config.Config{}, notify: notifier}

	svc.applyNotifyPayload(notifySettingsPayload{
		Enabled: true,
		Feishu:  notifyChannelPayload{Enabled: true, Webhook: "https://hook/new"},
	})

	if err := notifier.ReadyForTest("feishu"); err != nil {
		t.Fatalf("保存后发送链路应立即生效：%v", err)
	}
	if got := svc.cfg.Notify.Feishu.Webhook; got != "https://hook/new" {
		t.Fatalf("内存配置也应同步，实际 %q", got)
	}
}

// TestNotifierConfigConcurrentSwap 在 -race 下校验"运行期换配置"与"发送链路读配置"不打架：
// 就地逐字段改写会让读方拿到半个旧配置（string/切片拷贝不是原子的），本用例用并发读写压出来。
func TestNotifierConfigConcurrentSwap(t *testing.T) {
	notifier := NewNotifierService(&config.NotifyConfig{Enabled: true}, "http://platform", nil, nil, zap.NewNop())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			notifier.UpdateConfig(config.NotifyConfig{
				Enabled: true,
				Feishu: config.WebhookChannelConfig{
					Enabled: true, Webhook: "https://hook/a", Secret: "secret-a",
					Mentions: []string{"a@example.com"},
				},
			})
			notifier.UpdateConfig(config.NotifyConfig{
				Enabled: true,
				WeCom:   config.WebhookChannelConfig{Enabled: true, Webhook: "https://hook/b", Secret: "secret-b"},
			})
		}
	}()

	for i := 0; i < 200; i++ {
		_ = notifier.ChannelStatus()
		_ = notifier.ReadyForTest("feishu")
		_ = notifier.current()
	}
	wg.Wait()
}
