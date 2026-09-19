package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/engine"
	"middleware-ops/internal/model"
	"middleware-ops/internal/utils"
)

// 本文件钉住「仓库凭据」的四条规矩（INC-031）：
//  1. 入库的 repo_url 不含凭据（怎么填都不含）；
//  2. 凭据加密保存，页面/接口不回显（结构体上 json:"-" + 本文件的断言）；
//  3. 留空 = 不修改、非空 = 覆盖、clear_credential = 清除；
//  4. 旧数据（URL 内嵌凭据）读到就迁移，且迁移失败**不能把凭据弄丢**。

func testCipher(t *testing.T) *utils.Cipher {
	t.Helper()
	c, err := utils.NewCipher("unit-test-master-key")
	if err != nil {
		t.Fatalf("构造测试用 Cipher 失败: %v", err)
	}
	return c
}

// TestApplyCodeRepoInputSanitizesAndEncrypts 钉住"地址永远干净、凭据永远加密"。
func TestApplyCodeRepoInputSanitizesAndEncrypts(t *testing.T) {
	cipher := testCipher(t)

	// ① 使用者把带令牌的地址整段粘进来 → 令牌被拆出来加密。
	item := &model.CodeRepo{}
	changed, err := applyCodeRepoInput(cipher, item, CodeRepoInput{
		ServiceName: "order-api",
		RepoURL:     "https://oauth2:glpat-secret@gitlab.internal/g/r.git",
		Branch:      "main",
	})
	if err != nil {
		t.Fatalf("applyCodeRepoInput 失败: %v", err)
	}
	if !changed {
		t.Fatal("带了令牌应视为凭据发生变化")
	}
	if item.RepoURL != "https://gitlab.internal/g/r.git" {
		t.Fatalf("入库地址必须干净，实际 %q", item.RepoURL)
	}
	if strings.Contains(item.CredentialEncrypted, "glpat-secret") {
		t.Fatalf("凭据不能明文入库，实际 %q", item.CredentialEncrypted)
	}
	if item.CredentialEncrypted == "" {
		t.Fatal("凭据应被加密保存（否则私有仓库拉不下来）")
	}
	if got := decryptRepoCredential(cipher, item); got != "oauth2:glpat-secret" {
		t.Fatalf("解密结果 = %q", got)
	}

	// ② 凭据留空（页面不回显，使用者没动这个框）→ 保持原样。
	before := item.CredentialEncrypted
	if changed, err = applyCodeRepoInput(cipher, item, CodeRepoInput{
		ServiceName: "order-api", RepoURL: "https://gitlab.internal/g/r.git", Branch: "main",
	}); err != nil {
		t.Fatalf("applyCodeRepoInput 失败: %v", err)
	}
	if changed || item.CredentialEncrypted != before {
		t.Fatalf("留空应等于不修改：changed=%v enc=%q", changed, item.CredentialEncrypted)
	}

	// ③ 显式换新令牌 → 覆盖。
	if changed, err = applyCodeRepoInput(cipher, item, CodeRepoInput{
		ServiceName: "order-api", RepoURL: "https://gitlab.internal/g/r.git",
		Branch: "main", Credential: "oauth2:new-token",
	}); err != nil {
		t.Fatalf("applyCodeRepoInput 失败: %v", err)
	}
	if !changed {
		t.Fatal("换令牌应视为凭据发生变化")
	}
	if got := decryptRepoCredential(cipher, item); got != "oauth2:new-token" {
		t.Fatalf("换令牌后解密结果 = %q", got)
	}

	// ④ 显式清除。
	if changed, err = applyCodeRepoInput(cipher, item, CodeRepoInput{
		ServiceName: "order-api", RepoURL: "https://gitlab.internal/g/r.git",
		Branch: "main", ClearCredential: true,
	}); err != nil {
		t.Fatalf("applyCodeRepoInput 失败: %v", err)
	}
	if !changed || item.CredentialEncrypted != "" {
		t.Fatalf("清除后应无凭据：changed=%v enc=%q", changed, item.CredentialEncrypted)
	}
	if got := decryptRepoCredential(cipher, item); got != "" {
		t.Fatalf("清除后解密结果应为空，实际 %q", got)
	}

	// ⑤ 查询参数形式的凭据同样被搬走。
	queryItem := &model.CodeRepo{}
	if _, err := applyCodeRepoInput(cipher, queryItem, CodeRepoInput{
		ServiceName: "svc", RepoURL: "https://git.internal/g/r.git?token=query-secret",
	}); err != nil {
		t.Fatalf("applyCodeRepoInput 失败: %v", err)
	}
	if strings.Contains(queryItem.RepoURL, "query-secret") {
		t.Fatalf("查询参数里的令牌也必须搬走，实际 %q", queryItem.RepoURL)
	}
	if got := decryptRepoCredential(cipher, queryItem); got != "?token=query-secret" {
		t.Fatalf("查询参数凭据 = %q", got)
	}
}

// TestEncryptRepoCredentialRefusesWithoutCipher 钉住"没有加密能力就拒绝保存"。
//
// 退化成明文是绝不能接受的选项：使用者看不出差别，泄露风险却是真实的。
func TestEncryptRepoCredentialRefusesWithoutCipher(t *testing.T) {
	if _, err := encryptRepoCredential(nil, "oauth2:secret"); err == nil {
		t.Fatal("没有 cipher 时必须报错，而不是明文保存")
	}
	if enc, err := encryptRepoCredential(nil, "   "); err != nil || enc != "" {
		t.Fatalf("空凭据应直接返回空：enc=%q err=%v", enc, err)
	}
}

// TestPrepareRepoCredentialMigratesLegacy 钉住旧数据迁移是幂等且不丢凭据的。
func TestPrepareRepoCredentialMigratesLegacy(t *testing.T) {
	cipher := testCipher(t)
	item := &model.CodeRepo{
		ServiceName: "legacy-svc",
		RepoURL:     "https://oauth2:legacy-token@gitlab.internal/g/r.git",
	}
	changed, err := prepareRepoCredential(cipher, item)
	if err != nil {
		t.Fatalf("迁移失败: %v", err)
	}
	if !changed {
		t.Fatal("内嵌凭据的旧记录应被判为需要迁移")
	}
	if item.RepoURL != "https://gitlab.internal/g/r.git" {
		t.Fatalf("迁移后地址应干净，实际 %q", item.RepoURL)
	}
	if got := decryptRepoCredential(cipher, item); got != "oauth2:legacy-token" {
		t.Fatalf("迁移后应仍能用原令牌，实际 %q", got)
	}

	// 幂等：第二次调用不应再改动。
	if changed, err = prepareRepoCredential(cipher, item); err != nil || changed {
		t.Fatalf("迁移应幂等：changed=%v err=%v", changed, err)
	}

	// 没有加密能力时：**不能**把 URL 改干净（那等于把凭据弄丢），必须报错并保持原样。
	raw := &model.CodeRepo{ServiceName: "svc", RepoURL: "https://oauth2:tok@gitlab.internal/g/r.git"}
	if _, err := prepareRepoCredential(nil, raw); err == nil {
		t.Fatal("无 cipher 时应报错")
	}
	if !strings.Contains(raw.RepoURL, "tok@") {
		t.Fatalf("迁移失败时不得改动原地址（否则凭据丢失），实际 %q", raw.RepoURL)
	}
	if got := decryptRepoCredential(nil, raw); got != "oauth2:tok" {
		t.Fatalf("无 cipher 时仍应能从旧 URL 读出凭据用于拉取，实际 %q", got)
	}
}

// TestRepoCredentialJSONNotExposed 钉住密文字段不会被序列化出去。
//
// 这条测试的价值在于"防呆"：模型结构体会被直接返回给前端，
// 哪天有人顺手把 json:"-" 改成别的标签，接口就会开始回显密文。
func TestRepoCredentialJSONNotExposed(t *testing.T) {
	item := model.CodeRepo{
		ServiceName: "svc", RepoURL: "https://git.internal/g/r.git",
		CredentialEncrypted: "v1:AAAA-secret-ciphertext",
	}
	raw := mustJSON(t, item)
	if strings.Contains(raw, "secret-ciphertext") || strings.Contains(raw, "credential_encrypted") {
		t.Fatalf("密文不该出现在接口响应里：%s", raw)
	}
}

// fakeEngine 是只提供"是否外发"这一个事实的假引擎（合规判定只需要它）。
type fakeEngine struct {
	external bool
	calls    int
}

func (f *fakeEngine) Name() string { return "fake" }
func (f *fakeEngine) Chat(context.Context, engine.ChatRequest) (*engine.ChatResponse, error) {
	f.calls++
	return &engine.ChatResponse{Content: "{}"}, nil
}
func (f *fakeEngine) ChatStream(context.Context, engine.ChatRequest) (engine.Stream, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeEngine) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
func (f *fakeEngine) Available() bool                                      { return true }
func (f *fakeEngine) Status() engine.Status                                { return engine.Status{Name: "fake", External: f.external} }

// TestOutboundGate 钉住**闸门的接线**（INC-030 的核心）：
// "引擎会外发"与"未许可"必须同时成立才阻断，且阻断时不给引擎任何调用机会。
//
// 这条测试是补上来的：早先只测了 `thirdPartyAllowedFor` 这个纯函数，
// 把调用点的 `engineExternal(...)` 去掉后测试照样全绿——而那个"去掉"正是原始缺陷本身。
func TestOutboundGate(t *testing.T) {
	allowedRepo := &model.CodeRepo{ServiceName: "svc", AllowThirdParty: true}
	deniedRepo := &model.CodeRepo{ServiceName: "svc", AllowThirdParty: false}

	cases := []struct {
		name        string
		engineExt   bool
		repo        *model.CodeRepo
		forceLocal  bool
		whitelisted bool
		wantAllowed bool
		wantBlocked bool
	}{
		{"第三方引擎 + 未勾选 → 阻断", true, deniedRepo, false, true, false, true},
		{"第三方引擎 + 勾选但不在白名单 → 阻断", true, allowedRepo, false, false, false, true},
		{"第三方引擎 + force_local → 阻断", true, allowedRepo, true, true, false, true},
		{"第三方引擎 + 无仓库映射 → 阻断", true, nil, false, true, false, true},
		{"第三方引擎 + 许可齐全 → 放行", true, allowedRepo, false, true, true, false},
		// 内网/规则引擎：内容不出网，两个开关都不该拦住分析。
		{"自建引擎 + 未勾选 → 放行（不出网）", false, deniedRepo, false, true, false, false},
		{"自建引擎 + 无仓库映射 → 放行（不出网）", false, nil, false, true, false, false},
	}
	for _, c := range cases {
		eng := &fakeEngine{external: c.engineExt}
		svc := &CodeAnalysisService{engine: eng, log: zap.NewNop()}
		allowed, blocked, _, reason := svc.outboundGate(c.repo, c.forceLocal, c.whitelisted, "svc")
		if allowed != c.wantAllowed || blocked != c.wantBlocked {
			t.Fatalf("%s：allowed=%v blocked=%v，期望 allowed=%v blocked=%v",
				c.name, allowed, blocked, c.wantAllowed, c.wantBlocked)
		}
		if c.wantBlocked && strings.TrimSpace(reason) == "" {
			t.Fatalf("%s：阻断时必须给出原因", c.name)
		}
		if !c.wantBlocked && reason != "" {
			t.Fatalf("%s：放行时不该有阻断原因，实际 %q", c.name, reason)
		}
	}
}

// TestThirdPartyAllowedFor 钉住"允许发给第三方 AI"的三个条件（INC-030 的判定表）。
func TestThirdPartyAllowedFor(t *testing.T) {
	allowed := &model.CodeRepo{ServiceName: "svc", AllowThirdParty: true}
	denied := &model.CodeRepo{ServiceName: "svc", AllowThirdParty: false}

	cases := []struct {
		name        string
		repo        *model.CodeRepo
		forceLocal  bool
		whitelisted bool
		wantAllowed bool
		wantWarn    string
	}{
		{"勾选 + 在白名单 → 允许", allowed, false, true, true, ""},
		{"勾选但不在白名单 → 不允许，并给出降级说明", allowed, false, false, false, "不在出网白名单"},
		{"勾选 + force_local → 不允许（本次显式要求只本地）", allowed, true, true, false, ""},
		{"没勾选 → 不允许", denied, false, true, false, ""},
		{"没有仓库映射 → 不允许", nil, false, true, false, ""},
	}
	for _, c := range cases {
		got, warn := thirdPartyAllowedFor(c.repo, c.forceLocal, c.whitelisted, "svc")
		if got != c.wantAllowed {
			t.Fatalf("%s：allowed = %v，期望 %v", c.name, got, c.wantAllowed)
		}
		if c.wantWarn != "" && !strings.Contains(warn, c.wantWarn) {
			t.Fatalf("%s：告警应提到 %q，实际 %q", c.name, c.wantWarn, warn)
		}
	}
}

// TestOutboundBlockReasonIsActionable 钉住阻断原因的"可操作性"。
//
// 使用者看到的不能只是"被拒绝"：他得知道去哪个页面勾什么、或者去改哪个配置。
func TestOutboundBlockReasonIsActionable(t *testing.T) {
	svc := &CodeAnalysisService{log: zap.NewNop()}

	reason := svc.outboundBlockReason("order-api", &model.CodeRepo{ServiceName: "order-api"}, false)
	for _, want := range []string{"order-api", "允许第三方 AI", "代码仓库", "self_hosted", "本地定位"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("阻断原因应包含 %q，实际：%s", want, reason)
		}
	}

	// 勾选了但不在白名单：原因应指向白名单，而不是让人去重复勾选。
	reason2 := svc.outboundBlockReason("order-api", &model.CodeRepo{ServiceName: "order-api", AllowThirdParty: true}, false)
	if !strings.Contains(reason2, "outbound_whitelist") {
		t.Fatalf("应指向出网白名单，实际：%s", reason2)
	}

	// force_local：原因要说清是本次请求自己的要求。
	reason3 := svc.outboundBlockReason("order-api", &model.CodeRepo{ServiceName: "order-api", AllowThirdParty: true}, true)
	if !strings.Contains(reason3, "force_local") {
		t.Fatalf("应提到 force_local，实际：%s", reason3)
	}

	// 没有仓库映射时也要给出正确指引。
	reason4 := svc.outboundBlockReason("order-api", nil, false)
	if !strings.Contains(reason4, "没有配置代码仓库映射") {
		t.Fatalf("应提到缺仓库映射，实际：%s", reason4)
	}
}

// TestRepoLastPullPrefersNewest 钉住"重启后不重复 pull"的判定基础（INC-031 的第 2 条）。
//
// 只认进程内记忆的话，每次容器重建都会把所有仓库重新 fetch 一遍；
// 只认 DB 的话，同一进程里刚拉过的仓库又会被再拉一次。
func TestRepoLastPullPrefersNewest(t *testing.T) {
	var memory sync.Map
	now := time.Now().UTC()
	older := now.Add(-10 * time.Minute)

	// 只有 DB 记录（刚重启，内存是空的）→ 用 DB 的时间。
	item := model.CodeRepo{ServiceName: "svc", LastPullAt: &older}
	if got := repoLastPull(&memory, item); !got.Equal(older) {
		t.Fatalf("应回落到 DB 的 last_pull_at，实际 %v", got)
	}

	// 内存里更近（本次进程刚拉过）→ 用内存的。
	memory.Store("svc", now)
	if got := repoLastPull(&memory, item); !got.Equal(now) {
		t.Fatalf("应取较新的内存时间，实际 %v", got)
	}

	// DB 更近（别人/上一次拉过）→ 用 DB 的。
	fresher := now.Add(time.Minute)
	item.LastPullAt = &fresher
	if got := repoLastPull(&memory, item); !got.Equal(fresher) {
		t.Fatalf("应取较新的 DB 时间，实际 %v", got)
	}

	// 都没有 → 零值（调用方据此判定"必须去拉"）。
	if got := repoLastPull(&memory, model.CodeRepo{ServiceName: "other"}); !got.IsZero() {
		t.Fatalf("没有任何记录时应返回零值，实际 %v", got)
	}
	// 传 nil 也不能 panic（防御性：调用方可能还没初始化 map）。
	if got := repoLastPull(nil, item); !got.Equal(fresher) {
		t.Fatalf("memory 为 nil 时应只用 DB 时间，实际 %v", got)
	}
}

// mustJSON 把值序列化成 JSON 字符串（用于断言"密文不会外泄到接口响应"）。
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(raw)
}
