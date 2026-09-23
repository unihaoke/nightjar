package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
)

// 本文件用一个假的 AI 服务跑通"提交 → 查询"这两步真实 HTTP 交互。
//
// 为什么值得测：这两个方法是平台与外部服务之间唯一的出网点，
// 路径拼错、鉴权头漏了、任务号没回传都会表现为"告警永远没有结论"，
// 而那种现象在页面上和"AI 服务慢"长得一模一样——只能靠测试提前钉住。

func TestSubmitAndQueryAgainstFakeService(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotBody   map[string]any
		queryPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"submitted"}`))
		case http.MethodGet:
			queryPath = r.URL.Path
			_, _ = w.Write([]byte(`{"task_id":"t-1","status":"succeeded","answer":"根因是连接池耗尽"}`))
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	client := NewAIAnalysisClient(config.AIAnalysisConfig{
		Enabled:    true,
		BaseURL:    srv.URL,
		APIKey:     "secret-token",
		SubmitPath: "/v1/analyses",
		QueryPath:  "/v1/analyses/{task_id}",
	}, zap.NewNop())

	// ① 提交：路径、鉴权、正文、任务号都要对。
	res, err := client.Submit(context.Background(), SubmitInput{
		TaskID:      "mwo-1-100",
		Service:     "jd-logs",
		Question:    "服务：jd-logs\n错误信息：NPE",
		CallbackURL: "http://platform/api/ai/analysis/callback",
		EventID:     1,
	})
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if gotPath != "/v1/analyses" {
		t.Fatalf("提交路径错误：%q", gotPath)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("鉴权头错误：%q", gotAuth)
	}
	if gotBody["task_id"] != "mwo-1-100" || gotBody["service"] != "jd-logs" ||
		gotBody["callback_url"] != "http://platform/api/ai/analysis/callback" {
		t.Fatalf("提交正文缺少关键字段：%v", gotBody)
	}
	if res.TaskID != "t-1" || res.Status != modelStatusSubmitted {
		t.Fatalf("提交结果异常：%+v", res)
	}

	// ② 查询：{task_id} 必须被替换，结论要能取回来。
	// generic 协议没有 runId，第二个参数传空串。
	q, err := client.Query(context.Background(), "t-1", "")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if queryPath != "/v1/analyses/t-1" {
		t.Fatalf("查询路径未替换任务号：%q", queryPath)
	}
	if q.Status != modelStatusSucceeded || q.Answer != "根因是连接池耗尽" {
		t.Fatalf("查询结果异常：%+v", q)
	}
}

// TestSubmitRefusesWhenNotConfigured 没配地址就不该发起任何请求。
func TestSubmitRefusesWhenNotConfigured(t *testing.T) {
	client := NewAIAnalysisClient(config.AIAnalysisConfig{Enabled: false}, zap.NewNop())
	if client.Configured() {
		t.Fatal("未启用时不该判定为可用")
	}
	if _, err := client.Submit(context.Background(), SubmitInput{TaskID: "x"}); err == nil {
		t.Fatal("未配置时提交应当报错")
	}
}

// TestJoinURL 拼路径的几种形态（尾部斜杠、缺前导斜杠、空路径）。
func TestJoinURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"http://ai:8000/", "/v1/analyses", "http://ai:8000/v1/analyses"},
		{"http://ai:8000", "v1/analyses", "http://ai:8000/v1/analyses"},
		{"http://ai:8000/", "", "http://ai:8000"},
	}
	for _, c := range cases {
		if got := joinURL(c.base, c.path); got != c.want {
			t.Fatalf("joinURL(%q,%q) = %q，期望 %q", c.base, c.path, got, c.want)
		}
	}
}
