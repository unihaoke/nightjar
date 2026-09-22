package service

import (
	"testing"
	"time"

	"middleware-ops/internal/config"
)

// 本文件钉住「AI 代码分析」纳入平台设置后的两条关键语义：
// 密钥三态（不改 / 覆盖 / 清除）与"平台设置 → 内存配置"的落位（含时长解析）。

func baselineAnalysis() aiAnalysisPayload {
	return aiAnalysisPayload{
		Enabled: true, BaseURL: "http://ai.internal:8000",
		APIKey: "k-old", CallbackToken: "t-old",
		SubmitPath: defaultSubmitPath, QueryPath: defaultQueryPath,
		Timeout: "15s", TaskTimeout: "30m", PollInterval: "60s", PollBatch: 20,
	}
}

// TestMergeAIAnalysisSecretTriState 密钥必须是三态：不动 / 覆盖 / 清除。
//
// 界面不回显明文，若"空串"被当成清空，管理员只改个地址就会把密钥弄丢——
// 这是设置页最容易出的一类静默故障。
func TestMergeAIAnalysisSecretTriState(t *testing.T) {
	old := baselineAnalysis()

	// ① 没动密钥：保持原值。
	got := mergeAIAnalysis(old, AIAnalysisInput{Enabled: true, BaseURL: "http://ai2", SubmitPath: "", QueryPath: ""})
	if got.APIKey != "k-old" || got.CallbackToken != "t-old" {
		t.Fatalf("未填密钥时应保持原值，实际 key=%q token=%q", got.APIKey, got.CallbackToken)
	}
	if got.BaseURL != "http://ai2" {
		t.Fatalf("地址应被覆盖，实际 %q", got.BaseURL)
	}
	// 路径留空要保底，不能把客户端拼地址的字段清空。
	if got.SubmitPath != defaultSubmitPath || got.QueryPath != defaultQueryPath {
		t.Fatalf("路径留空应回落默认值，实际 %q / %q", got.SubmitPath, got.QueryPath)
	}

	// ② 覆盖密钥。
	got = mergeAIAnalysis(old, AIAnalysisInput{Enabled: true, BaseURL: "http://ai.internal:8000",
		APIKey: "k-new", CallbackToken: "t-new"})
	if got.APIKey != "k-new" || got.CallbackToken != "t-new" {
		t.Fatalf("填写后应覆盖，实际 key=%q token=%q", got.APIKey, got.CallbackToken)
	}

	// ③ 显式清除（只清指定的那一把）。
	got = mergeAIAnalysis(old, AIAnalysisInput{Enabled: true, BaseURL: "http://ai.internal:8000", ClearAPIKey: true})
	if got.APIKey != "" {
		t.Fatalf("clear 后应清空 api_key，实际 %q", got.APIKey)
	}
	if got.CallbackToken != "t-old" {
		t.Fatalf("只清 api_key 时不应动 callback_token，实际 %q", got.CallbackToken)
	}
}

// TestApplyCodeAnalysisToConfig 平台设置必须真正落到内存配置里（否则保存不生效）。
func TestApplyCodeAnalysisToConfig(t *testing.T) {
	cfg := &config.Config{}
	payload := defaultAISettingsPayload()
	payload.CodeAnalysis = aiAnalysisPayload{
		Enabled: true, BaseURL: "http://ai.internal:8000", APIKey: "k",
		SubmitPath: "/v1/tasks", QueryPath: "/v1/tasks/{task_id}",
		CallbackURL: "http://platform", CallbackToken: "t",
		Timeout: "20s", TaskTimeout: "45m", PollInterval: "90s", PollBatch: 5,
		CallMode: defaultCallModeSync, SyncTimeout: "5m", NotifyOnSubmit: true,
	}
	payload.applyCodeAnalysisTo(cfg)

	got := cfg.AIAnalysis
	if !got.Enabled || got.BaseURL != "http://ai.internal:8000" || got.APIKey != "k" {
		t.Fatalf("基础字段未生效：%+v", got)
	}
	if got.TaskTimeout != 45*time.Minute || got.PollInterval != 90*time.Second || got.Timeout != 20*time.Second {
		t.Fatalf("时长解析错误：%+v", got)
	}
	if got.PollBatch != 5 || got.CallMode != defaultCallModeSync || got.SyncTimeout != 5*time.Minute || !got.NotifyOnSubmit {
		t.Fatalf("开关/批量未生效：%+v", got)
	}
	if got.CallbackURL != "http://platform" || got.CallbackToken != "t" {
		t.Fatalf("回调配置未生效：%+v", got)
	}
}

// TestApplyCodeAnalysisInvalidDurationKeepsOld 非法时长保留旧值：
// 静默归零会让超时立刻触发（0 超时 = 每次都超时），比沿用旧值危险得多。
func TestApplyCodeAnalysisInvalidDurationKeepsOld(t *testing.T) {
	cfg := &config.Config{}
	cfg.AIAnalysis.TaskTimeout = 30 * time.Minute

	payload := defaultAISettingsPayload()
	payload.CodeAnalysis.TaskTimeout = "不是时长"
	payload.applyCodeAnalysisTo(cfg)

	if cfg.AIAnalysis.TaskTimeout != 30*time.Minute {
		t.Fatalf("非法时长应保留原值，实际 %v", cfg.AIAnalysis.TaskTimeout)
	}
}

// TestDefaultCodeAnalysisDisabled 默认关闭：没配地址就不该假装能分析。
func TestDefaultCodeAnalysisDisabled(t *testing.T) {
	cfg := &config.Config{}
	defaultAISettingsPayload().applyCodeAnalysisTo(cfg)
	if cfg.AIAnalysis.Enabled {
		t.Fatal("AI 代码分析默认应为关闭")
	}
	if cfg.AIAnalysis.SubmitPath == "" || cfg.AIAnalysis.QueryPath == "" {
		t.Fatal("默认应带可用的对接路径，否则界面首次打开就是空的")
	}
}
