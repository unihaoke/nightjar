package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件用 HTTP 桩验证 Engine API 客户端的请求构造与幂等语义。
//
// 为什么不接真实 Docker：CI 与开发机不一定有守护进程，而集成中心最怕的
// 「创建了容器但网络没挂上」「重复应用时容器无限堆积」这两类问题，
// 都能在请求层面固化住。

// engineStub 是一个可观测的 Engine API 桩。
type engineStub struct {
	exists      bool
	createdBody map[string]any
	calls       []string
	networks    []string
}

func (s *engineStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		switch {
		case r.URL.Path == "/_ping":
			w.WriteHeader(http.StatusOK)
			return
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			if !s.exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "existing", "Name": "/mwops-exporter-jd-redis",
				"State":  map[string]any{"Status": "running", "Running": true},
				"Config": map[string]any{"Image": "oliver006/redis_exporter:v1.66.0"},
			})
			return
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/containers/"):
			s.exists = false
			w.WriteHeader(http.StatusNoContent)
			return
		case r.Method == http.MethodPost && r.URL.Path == "/containers/create":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &s.createdBody)
			s.exists = true
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "new-container-id"})
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
			w.WriteHeader(http.StatusNoContent)
			return
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/connect"):
			s.networks = append(s.networks, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/networks/"), "/connect"))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
	}
}

func newStubClient(t *testing.T, stub *engineStub) *Client {
	t.Helper()
	server := httptest.NewServer(stub.handler())
	t.Cleanup(server.Close)
	client, err := New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("构造客户端失败：%v", err)
	}
	return client
}

// TestEnsureCreatesContainerWithNetworkAndEnv 锁定创建请求的关键字段。
func TestEnsureCreatesContainerWithNetworkAndEnv(t *testing.T) {
	stub := &engineStub{}
	client := newStubClient(t, stub)

	action, id, err := client.Ensure(context.Background(), ContainerSpec{
		Name: "mwops-exporter-jd-redis", Image: "oliver006/redis_exporter:v1.66.0",
		Env:      []string{"REDIS_ADDR=redis://jd-redis:6379", "REDIS_PASSWORD=secret"},
		Cmd:      []string{"--check-keys=db0=session:*"},
		Networks: []string{"mwops", "jd-nightjar"},
	})
	if err != nil {
		t.Fatalf("Ensure 失败：%v", err)
	}
	if action != "created" || id != "new-container-id" {
		t.Fatalf("动作或容器 ID 不符：action=%s id=%s", action, id)
	}
	hostConfig, ok := stub.createdBody["HostConfig"].(map[string]any)
	if !ok {
		t.Fatalf("创建请求缺少 HostConfig：%v", stub.createdBody)
	}
	// 第一个网络作为 NetworkMode，其余通过 /networks/{id}/connect 追加。
	if hostConfig["NetworkMode"] != "mwops" {
		t.Fatalf("NetworkMode 应为 mwops，实际 %v", hostConfig["NetworkMode"])
	}
	if len(stub.networks) != 1 || stub.networks[0] != "jd-nightjar" {
		t.Fatalf("应只对第二个网络发起 connect，实际 %v", stub.networks)
	}
	env, _ := stub.createdBody["Env"].([]any)
	if len(env) != 2 {
		t.Fatalf("Env 未透传：%v", stub.createdBody["Env"])
	}
	labels, _ := stub.createdBody["Labels"].(map[string]any)
	if labels["mwops.managed"] != "true" {
		t.Fatalf("平台管理的容器必须带 mwops.managed 标签（便于清理），实际 %v", labels)
	}
}

// TestEnsureRecreatesExistingContainer 锁定「重新应用 = 重建容器」的语义，
// 避免环境变量改了但容器没变，或反复应用堆出多个同名容器。
func TestEnsureRecreatesExistingContainer(t *testing.T) {
	stub := &engineStub{exists: true}
	client := newStubClient(t, stub)

	action, _, err := client.Ensure(context.Background(), ContainerSpec{
		Name: "mwops-exporter-jd-redis", Image: "oliver006/redis_exporter:v1.66.0",
		Networks: []string{"mwops"},
	})
	if err != nil {
		t.Fatalf("Ensure 失败：%v", err)
	}
	if action != "recreated" {
		t.Fatalf("容器已存在时应重建，实际动作 %s", action)
	}
	joined := strings.Join(stub.calls, " | ")
	if !strings.Contains(joined, "DELETE /containers/mwops-exporter-jd-redis") {
		t.Fatalf("重建前应先删除旧容器，实际调用：%s", joined)
	}
	if strings.Contains(joined, "/connect") {
		t.Fatalf("只有一个网络时不应发起 connect，实际调用：%s", joined)
	}
}

// TestInspectMissingContainerReturnsNil 锁定「容器不存在」不当作错误。
func TestInspectMissingContainerReturnsNil(t *testing.T) {
	stub := &engineStub{}
	client := newStubClient(t, stub)
	state, err := client.Inspect(context.Background(), "mwops-exporter-none")
	if err != nil {
		t.Fatalf("容器不存在不应报错：%v", err)
	}
	if state != nil {
		t.Fatalf("容器不存在应返回 nil，实际 %+v", state)
	}
}

// TestListManagedFiltersByLabel 锁定清理能力依赖的标签过滤条件。
func TestListManagedFiltersByLabel(t *testing.T) {
	var rawQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"Id": "1", "Names": []string{"/mwops-exporter-jd-redis"}, "Image": "img", "State": "running", "Status": "Up 2 minutes"},
			{"Id": "2", "Names": []string{"/mwops-exporter-jd-mysql"}, "Image": "img", "State": "exited", "Status": "Exited (0) 1 minute ago"},
		})
	}))
	defer server.Close()
	client, err := New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("构造客户端失败：%v", err)
	}
	items, err := client.ListManaged(context.Background())
	if err != nil {
		t.Fatalf("列举失败：%v", err)
	}
	// 结果按容器名排序（便于前端稳定展示），因此按名字断言而不是按下标。
	if len(items) != 2 {
		t.Fatalf("应返回 2 个受管容器，实际 %+v", items)
	}
	byName := map[string]bool{}
	for _, item := range items {
		byName[item.Name] = item.Running
	}
	if running, ok := byName["mwops-exporter-jd-redis"]; !ok || !running {
		t.Fatalf("redis exporter 应为 running：%+v", items)
	}
	if running, ok := byName["mwops-exporter-jd-mysql"]; !ok || running {
		t.Fatalf("mysql exporter 应为 exited：%+v", items)
	}
	if !strings.Contains(rawQuery, "mwops.managed%3Dtrue") {
		t.Fatalf("必须按 mwops.managed=true 过滤，实际 query=%s", rawQuery)
	}
}
