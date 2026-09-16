package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件锁定「平台自己去找目标容器与网络」的行为。
//
// 为什么重要：被管项目（如 jd）不应该为了被监控而建互联网络、加别名、改 compose。
// 使用者在平台里只填一个名字，平台必须能自己解析出：哪个容器、在哪张网络上、
// 以及一个"在那个网络上一定能解析"的主机名。

// targetStub 提供容器列表与容器详情两类响应。
type targetStub struct {
	containers []map[string]any
}

func (s *targetStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/containers/json":
			list := make([]map[string]any, 0, len(s.containers))
			for _, c := range s.containers {
				list = append(list, map[string]any{"Id": c["Id"]})
			}
			_ = json.NewEncoder(w).Encode(list)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/containers/") &&
			strings.HasSuffix(r.URL.Path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
			for _, c := range s.containers {
				if c["Id"] == id {
					_ = json.NewEncoder(w).Encode(c)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotImplemented)
		}
	}
}

func newTargetClient(t *testing.T, containers ...map[string]any) *Client {
	t.Helper()
	stub := &targetStub{containers: containers}
	server := httptest.NewServer(stub.handler())
	t.Cleanup(server.Close)
	client, err := New("tcp://" + strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("构造客户端失败：%v", err)
	}
	return client
}

// container 构造一个容器详情响应（字段与 Engine API 对齐）。
func container(name, service string, aliases, networks []string) map[string]any {
	nets := map[string]any{}
	for _, n := range networks {
		nets[n] = map[string]any{"Aliases": aliases}
	}
	return map[string]any{
		"Id":    "id-" + name,
		"Name":  "/" + name,
		"State": map[string]any{"Running": true},
		"Config": map[string]any{
			"Labels": map[string]any{"com.docker.compose.service": service},
		},
		"NetworkSettings": map[string]any{"Networks": nets},
	}
}

// TestResolveTargetByContainerName 覆盖最常见写法：直接填容器名。
func TestResolveTargetByContainerName(t *testing.T) {
	client := newTargetClient(t,
		container("interview-redis", "redis", []string{"interview-redis", "redis"}, []string{"jd_jd-data"}),
	)
	res, err := client.ResolveTarget(context.Background(), "interview-redis")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if res.Container != "interview-redis" || res.MatchedBy != "container_name" {
		t.Fatalf("应命中容器名，实际 container=%q matchedBy=%q", res.Container, res.MatchedBy)
	}
	if len(res.Networks) != 1 || res.Networks[0] != "jd_jd-data" {
		t.Fatalf("应返回真实网络名 jd_jd-data，实际 %v", res.Networks)
	}
	if res.Host != "interview-redis" {
		t.Fatalf("应回写容器名作为可解析主机名，实际 %q", res.Host)
	}
	if !res.Running {
		t.Fatal("容器在运行时应标记 Running")
	}
}

// TestResolveTargetByComposeServiceRewritesHost 锁定：填 compose 服务名也能找到，
// 且返回的 Host 换成容器名（服务名别名不一定存在于所有网络上）。
func TestResolveTargetByComposeServiceRewritesHost(t *testing.T) {
	client := newTargetClient(t,
		container("interview-mysql", "mysql", []string{"interview-mysql", "mysql"}, []string{"jd_jd-data"}),
	)
	res, err := client.ResolveTarget(context.Background(), "mysql")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if res.MatchedBy != "compose_service" || res.Container != "interview-mysql" {
		t.Fatalf("应命中 compose 服务名，实际 container=%q matchedBy=%q", res.Container, res.MatchedBy)
	}
	if res.Host != "interview-mysql" {
		t.Fatalf("Host 应换成容器名，实际 %q", res.Host)
	}
}

// TestResolveTargetUnknownReturnsCandidates 锁定：找不到目标时给出候选名，
// 让报错能直接告诉用户"你大概想填哪个"，而不是只说找不到。
func TestResolveTargetUnknownReturnsCandidates(t *testing.T) {
	client := newTargetClient(t,
		container("interview-redis", "redis", []string{"interview-redis", "redis"}, []string{"jd_jd-data"}),
	)
	res, err := client.ResolveTarget(context.Background(), "jd-redis")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if res.Container != "" {
		t.Fatalf("不存在别名 jd-redis（那是旧架构的人工别名），不应命中，实际 %q", res.Container)
	}
	if len(res.Candidates) == 0 {
		t.Fatal("应返回候选容器名以便提示用户")
	}
	found := false
	for _, c := range res.Candidates {
		if c == "interview-redis" {
			found = true
		}
	}
	if !found {
		t.Fatalf("候选里应包含 interview-redis，实际 %v", res.Candidates)
	}
}

// TestResolveTargetUnionsNetworks 锁定：同一名字命中多个容器时取网络并集（去重排序），
// 这样 Exporter 不会因为"只接了其中一张网"而连不上目标。
func TestResolveTargetUnionsNetworks(t *testing.T) {
	client := newTargetClient(t,
		container("redis-a", "redis", []string{"redis"}, []string{"net-b"}),
		container("redis-b", "redis", []string{"redis"}, []string{"net-a", "net-b"}),
	)
	res, err := client.ResolveTarget(context.Background(), "redis")
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if len(res.Networks) != 2 || res.Networks[0] != "net-a" || res.Networks[1] != "net-b" {
		t.Fatalf("应返回去重排序后的网络并集 [net-a net-b]，实际 %v", res.Networks)
	}
}
