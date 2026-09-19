package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
)

// 本文件钉住「按文件名全仓遍历」这条缺陷（INC-032）不再复现。
//
// 为什么值得逐条钉：给出一条**来自 vendor/构建产物/测试目录**的同名文件的第 120 行，
// 比诚实地说"没定位到"更有害——结论看起来有依据，人工复核反而更容易被带偏。

// writeTree 在 root 下铺一棵源码树，内容里带上 marker 便于断言"读到的到底是哪一个"。
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("建目录失败 %s: %v", rel, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("写文件失败 %s: %v", rel, err)
		}
	}
}

// newLocateService 构造一个只装了"文件清单、脱敏器与日志"的最小分析服务。
func newLocateService(files []string, filesErr error) *CodeAnalysisService {
	cfg := &config.Config{}
	svc := &CodeAnalysisService{
		cfg: cfg, log: zap.NewNop(), redactor: NewRedactor(&cfg.Security),
		trackedCache: map[string]trackedFilesEntry{},
	}
	svc.SetRepoFetcher(&stubRepoFetcher{hasCode: true, files: files, filesErr: filesErr})
	return svc
}

const javaStack = `java.lang.NullPointerException: boom
	at com.acme.order.OrderService.process(OrderService.java:42)
	at com.acme.web.Api.handle(Api.java:10)`

// TestLocateCodePrefersSourceOverVendorTargetAndTests 钉住打分规则。
func TestLocateCodePrefersSourceOverVendorTargetAndTests(t *testing.T) {
	root := t.TempDir()
	body := func(marker string) string {
		return "package com.acme.order;\n\npublic class OrderService {\n" +
			"  // " + marker + "\n  public void process() {}\n}\n"
	}
	// 一次写一个文件（而不是一个多键 map）：键长不一的多键 map 要求整列对齐，
	// 手写极易差一个空格，而这种差异与测试意图无关。
	writeTree(t, root, map[string]string{"src/main/java/com/acme/order/OrderService.java": body("SOURCE")})
	writeTree(t, root, map[string]string{"target/classes/com/acme/order/OrderService.java": body("BUILD")})
	writeTree(t, root, map[string]string{"vendor/dep/com/acme/order/OrderService.java": body("VENDOR")})
	writeTree(t, root, map[string]string{"src/test/java/com/acme/order/OrderService.java": body("TEST")})
	// 候选顺序**刻意把错的那几个排在前面**：这正是不打分时的真实情形
	//（字典序下 target/ 常常排在 src/ 之前），也保证"取第一个候选"这种退化会被测出来。
	svc := newLocateService([]string{
		"target/classes/com/acme/order/OrderService.java",
		"vendor/dep/com/acme/order/OrderService.java",
		"src/test/java/com/acme/order/OrderService.java",
		"src/main/java/com/acme/order/OrderService.java",
	}, nil)

	snippet, file, line, _ := svc.locateCode(context.Background(),
		&model.CodeRepo{ServiceName: "svc", LocalPath: root}, javaStack)
	if file != "src/main/java/com/acme/order/OrderService.java" {
		t.Fatalf("应命中源码目录，实际 %q（snippet=%q）", file, snippet)
	}
	if !strings.Contains(snippet, "SOURCE") {
		t.Fatalf("读到的不是源码那份，snippet=%q", snippet)
	}
	if line <= 0 {
		t.Fatalf("行号应为正数，实际 %d", line)
	}
	// 返回的是仓库内相对路径，不是宿主绝对路径（绝对路径会泄露缓存目录布局）。
	if strings.Contains(file, root) || filepath.IsAbs(file) {
		t.Fatalf("定位结果不该是绝对路径：%q", file)
	}
}

// TestLocateCodeStillUsesVendorWhenSourceMissing 钉住"降权不等于排除"。
//
// 源码确实不在仓库里时（例如只有编译产物、或该文件属于依赖），
// 给一个候选仍比"什么都找不到"有用——但必须只有它一个候选时才这样。
func TestLocateCodeStillUsesVendorWhenSourceMissing(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"vendor/dep/com/acme/order/OrderService.java": "public class OrderService { void process() {} }\n",
	})
	svc := newLocateService([]string{"vendor/dep/com/acme/order/OrderService.java"}, nil)
	_, file, _, _ := svc.locateCode(context.Background(), &model.CodeRepo{LocalPath: root}, javaStack)
	if file != "vendor/dep/com/acme/order/OrderService.java" {
		t.Fatalf("唯一候选在 vendor 下时仍应返回它，实际 %q", file)
	}
}

// TestLocateCodeMatchesPathHintForDynamicLanguages 钉住 Python/Go 的路径线索匹配。
//
// 动态语言的堆栈给的是**运行时**路径（容器内 /app/...），与仓库布局往往不同，
// 因此匹配要逐级放宽到后缀；而只按文件名找会让 legacy/ 下的同名文件抢先命中。
func TestLocateCodeMatchesPathHintForDynamicLanguages(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"src/foo/bar.py": "def handler():\n    raise ValueError('SRC')\n"})
	writeTree(t, root, map[string]string{"legacy/deep/foo/bar.py": "def handler():\n    raise ValueError('LEGACY')\n"})
	// 两个候选都能对上 `foo/bar.py` 这一级（同分），此时取**路径更浅**的那个；
	// 候选顺序同样刻意把错的那个排前面，确保"取第一个"会被测出来。
	svc := newLocateService([]string{"legacy/deep/foo/bar.py", "src/foo/bar.py"}, nil)
	snippet, file, _, _ := svc.locateCode(context.Background(), &model.CodeRepo{LocalPath: root},
		"Traceback (most recent call last):\n  File \"/app/foo/bar.py\", line 2, in handler\nValueError: boom")
	if file != "src/foo/bar.py" {
		t.Fatalf("同分时应取路径最浅的候选，实际 %q", file)
	}
	if !strings.Contains(snippet, "SRC") {
		t.Fatalf("读到的不是源码那份，snippet=%q", snippet)
	}

	// Go：完整模块路径线索应压过 vendor 下的同名文件（vendor 排在前面）。
	goRoot := t.TempDir()
	writeTree(t, goRoot, map[string]string{"internal/service/codeanalysis.go": "package service\n\n// SRC-MARK\nfunc locate() {}\n"})
	writeTree(t, goRoot, map[string]string{"vendor/x/service/codeanalysis.go": "package service\n\n// VENDOR-MARK\nfunc locate() {}\n"})
	svc2 := newLocateService([]string{"vendor/x/service/codeanalysis.go", "internal/service/codeanalysis.go"}, nil)
	snippet2, file2, _, _ := svc2.locateCode(context.Background(), &model.CodeRepo{LocalPath: goRoot},
		"goroutine 1 [running]:\nmiddleware-ops/internal/service/codeanalysis.go:120 +0x1f")
	if file2 != "internal/service/codeanalysis.go" {
		t.Fatalf("Go 堆栈应命中模块内路径，实际 %q", file2)
	}
	if !strings.Contains(snippet2, "SRC-MARK") {
		t.Fatalf("读到的不是模块内的那份，snippet=%q", snippet2)
	}
}

// TestLocateCodeFallsBackToWalkWhenIndexUnavailable 钉住兜底路径：
// git 索引读不到时（非 git 目录、git 不可用）仍能定位，且不会走进依赖目录。
func TestLocateCodeFallsBackToWalkWhenIndexUnavailable(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"node_modules/dep/OrderService.java": "public class OrderService { void process() { /*NODE*/ } }\n"})
	writeTree(t, root, map[string]string{"app/OrderService.java": "public class OrderService { void process() { /*APP*/ } }\n"})
	svc := newLocateService(nil, errNoFetcher)
	snippet, file, _, _ := svc.locateCode(context.Background(), &model.CodeRepo{LocalPath: root}, javaStack)
	if file != "app/OrderService.java" {
		t.Fatalf("兜底遍历应跳过依赖目录，实际 %q", file)
	}
	if !strings.Contains(snippet, "APP") {
		t.Fatalf("snippet 内容异常：%q", snippet)
	}
}

// TestLocateCodeSkipsNonRegularCandidate 钉住"候选必须是普通文件"这条守卫。
//
// 覆盖度说明（如实写）：这条守卫真正防的是**符号链接指向仓库外**
// （`Foo.java -> /etc/passwd` 能把任意文件内容读出来）。而"候选是目录"那一段
// 即使守卫失效也不会读出内容（`os.ReadFile` 读目录本身就会失败），
// 它只是一条"不许崩、不许把目录当文件"的兜底断言。
// 因此：**在没有建软链权限的环境（本机 Windows）上，这条守卫的失效测不出来**，
// 需要在 Linux/容器里跑才算真覆盖——不要因为这里全绿就以为它被验证过了。
func TestLocateCodeSkipsNonRegularCandidate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "OrderService.java"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	svc := newLocateService([]string{"OrderService.java"}, nil)
	snippet, file, _, _ := svc.locateCode(context.Background(), &model.CodeRepo{LocalPath: root}, javaStack)
	if file != "" || snippet != "" {
		t.Fatalf("目录不该被当成源码文件读取：file=%q snippet=%q", file, snippet)
	}

	// 软链指向仓库外：必须跳过。
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("TOP-SECRET-CONTENT\n"), 0o600); err != nil {
		t.Fatalf("写外部文件失败: %v", err)
	}
	linkRoot := t.TempDir()
	link := filepath.Join(linkRoot, "OrderService.java")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("当前环境不允许创建符号链接，软链守卫**未实测**（需在 Linux/容器里验证）：%v", err)
	}
	svc2 := newLocateService([]string{"OrderService.java"}, nil)
	snippet2, file2, _, _ := svc2.locateCode(context.Background(), &model.CodeRepo{LocalPath: linkRoot}, javaStack)
	if file2 != "" || strings.Contains(snippet2, "TOP-SECRET") {
		t.Fatalf("软链不该被读取：file=%q snippet=%q", file2, snippet2)
	}
}

// TestHintSuffixesAndScoring 直接钉住打分函数的边界。
func TestHintSuffixesAndScoring(t *testing.T) {
	got := hintSuffixes("app/foo/bar.py")
	want := []string{"app/foo/bar.py", "foo/bar.py", "bar.py"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("hintSuffixes = %v，期望 %v", got, want)
	}
	if hintSuffixes("/") != nil {
		t.Fatal("空线索应返回 nil")
	}

	index := &locateIndexInfo{fileName: "OrderService.java", pathHint: "com/acme/order/OrderService.java"}
	src := scoreCandidate("src/main/java/com/acme/order/OrderService.java", index)
	vendor := scoreCandidate("vendor/x/com/acme/order/OrderService.java", index)
	target := scoreCandidate("target/classes/com/acme/order/OrderService.java", index)
	if !(src > vendor && src > target) {
		t.Fatalf("源码分数应高于依赖与产物：src=%d vendor=%d target=%d", src, vendor, target)
	}
	if vendor <= target {
		// 两者都被扣了分：只要源码最高即可，但这里顺带确认扣分确实生效（都低于源码）。
		if vendor >= src || target >= src {
			t.Fatalf("扣分未生效：src=%d vendor=%d target=%d", src, vendor, target)
		}
	}
}
