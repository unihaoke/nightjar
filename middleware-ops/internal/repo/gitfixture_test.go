package repo

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// 本文件是「本地远端」测试夹具（不依赖网络）。
//
// 做法：git init --bare 建裸库，用一个临时工作副本提交文件后 push 进去，
// 再以该地址为 RepoURL 调用 Ensure。
//
// 传输方式（重要）：优先使用裸库的本地路径（符合需求约定：路径或 file:// URL）。
// 但受限沙箱（本机开发环境即是）里 git for Windows 的本地/file 传输会启动 msys2
// sh.exe 去执行 upload-pack/receive-pack，而沙箱禁止该进程创建共享内存与命名管道
// （"couldn't create signal pipe, Win32 error 5" / CreateFileMapping Win32 error 5），
// 传输必然失败——这与被测试代码无关。HTTP 传输由 git-remote-http.exe 直接发起、
// 不经过 sh.exe，因此在探测到本地传输不可用时自动改用 127.0.0.1 上的 smart-HTTP
// （git http-backend）跑**真实 git**：仍然是真实 clone/fetch/push，断言逻辑不变。

// gitTest 在 dir 下执行 git 命令并返回合并输出。
//
// 固定身份/关闭签名/固定换行/允许 file 协议：让结果与机器上的全局 git 配置无关
// （需求要求显式传 -c user.name/-c user.email，不依赖全局配置）。
func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{
		"-c", "user.name=repo-test",
		"-c", "user.email=repo-test@example.com",
		"-c", "commit.gpgsign=false",
		"-c", "core.autocrlf=false",
		"-c", "protocol.file.allow=always",
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v 失败: %v\n%s", args, err, out)
	}
	return string(out)
}

var (
	localTransportOnce sync.Once
	localTransportOK   bool
)

// localTransportWorks 探测「本地路径/file:// 传输」是否可用（进程内只探测一次）。
func localTransportWorks(t *testing.T, bare string) bool {
	t.Helper()
	localTransportOnce.Do(func() {
		probe := filepath.Join(t.TempDir(), "probe-clone")
		cmd := exec.Command("git", "clone", "--bare", "--quiet", bare, probe)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("本地路径 git 传输不可用（沙箱限制），回退到回环 smart-HTTP：%v\n%s", err, out)
			return
		}
		localTransportOK = true
	})
	return localTransportOK
}

// testRemote 是一个「本地远端」：bare 裸库 + work 工作副本 + 供 git 使用的 url。
type testRemote struct {
	bare string
	work string
	url  string
}

// newTestRemote 建裸库、初始提交并推到远端。
//
// 默认分支用 symbolic-ref 显式指定 main：git 2.27（本机版本）不支持 `git init -b`
// 与 init.defaultBranch，不这么做 clone 空库会报 "remote HEAD refers to nonexistent ref"。
func newTestRemote(t *testing.T) *testRemote {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")

	gitTest(t, base, "init", "--bare", "--quiet", bare)
	gitTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	// HTTP 传输下要允许匿名 push（测试夹具需要往「远端」加提交）。
	gitTest(t, bare, "config", "http.receivepack", "true")

	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("创建工作副本失败: %v", err)
	}
	gitTest(t, work, "init", "--quiet")
	gitTest(t, work, "symbolic-ref", "HEAD", "refs/heads/main")

	r := &testRemote{bare: bare, work: work, url: bare}
	if !localTransportWorks(t, bare) {
		srv := newGitHTTPBackend(t, base)
		r.url = srv.URL + "/remote.git"
		t.Logf("使用回环 smart-HTTP 作为远端：%s", r.url)
	}

	r.write(t, "app/main.go", "package main\n\nfunc main() {}\n")
	r.commit(t, "init")
	gitTest(t, work, "remote", "add", "origin", r.url)
	gitTest(t, work, "push", "--quiet", "-u", "origin", "main")
	return r
}

func (r *testRemote) write(t *testing.T, rel, content string) {
	t.Helper()
	p := filepath.Join(r.work, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件 %s 失败: %v", p, err)
	}
}

func (r *testRemote) commit(t *testing.T, msg string) {
	t.Helper()
	gitTest(t, r.work, "add", "-A")
	gitTest(t, r.work, "commit", "--quiet", "-m", msg)
}

// push 把工作副本的新提交推到远端（模拟「远端有了新提交」）。
func (r *testRemote) push(t *testing.T) {
	t.Helper()
	gitTest(t, r.work, "push", "--quiet", "origin", "main")
}

func (r *testRemote) head(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(gitTest(t, r.work, "rev-parse", "HEAD"))
}

// newGitHTTPBackend 用 net/http + `git http-backend` 在回环上暴露 root 下的裸库。
//
// 这是受限沙箱的传输回退方案：git-remote-http 由 git 直接 exec（不经过 sh.exe），
// 服务端 `git http-backend` 也是直接 exec git-upload-pack/git-receive-pack，
// 全程不需要 msys2 的 fork。
func newGitHTTPBackend(t *testing.T, root string) *httptest.Server {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		env := []string{
			"GIT_PROJECT_ROOT=" + root,
			"GIT_HTTP_EXPORT_ALL=1",
			"REQUEST_METHOD=" + r.Method,
			"PATH_INFO=" + r.URL.Path,
			"QUERY_STRING=" + r.URL.RawQuery,
			"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
			"REMOTE_ADDR=127.0.0.1",
			"SERVER_PROTOCOL=HTTP/1.1",
			"GATEWAY_INTERFACE=CGI/1.1",
		}
		if r.ContentLength >= 0 {
			env = append(env, fmt.Sprintf("CONTENT_LENGTH=%d", r.ContentLength))
		}

		cmd := exec.Command("git", "http-backend")
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdin = r.Body
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			http.Error(w, "git http-backend 失败: "+err.Error()+"\n"+stderr.String(), http.StatusInternalServerError)
			return
		}

		head, body := splitCGI(stdout.Bytes())
		status := http.StatusOK
		for _, line := range strings.Split(head, "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" {
				continue
			}
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			switch strings.ToLower(key) {
			case "status":
				code, _, _ := strings.Cut(value, " ")
				if n, err := strconv.Atoi(code); err == nil {
					status = n
				}
			case "content-length":
				// 交给 net/http 自己算，避免与 CGI 的长度不一致导致截断。
			default:
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// splitCGI 切分 CGI 输出：头部与 body 之间由空行分隔（CRLF 或 LF）。
func splitCGI(out []byte) (string, []byte) {
	idx := -1
	sepLen := 0
	for _, sep := range [][]byte{[]byte("\r\n\r\n"), []byte("\n\n")} {
		if i := bytes.Index(out, sep); i >= 0 && (idx < 0 || i < idx) {
			idx, sepLen = i, len(sep)
		}
	}
	if idx < 0 {
		return string(out), nil
	}
	return string(out[:idx]), out[idx+sepLen:]
}
