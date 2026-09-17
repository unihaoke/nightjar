package integration

import (
	"strings"
	"testing"
)

// 本文件锁定「远程安装」产物的安全与可用性约定。
//
// 这是平台唯一会**在别的机器上执行命令**的能力，因此必须固化：
//  1. playbook 里绝不出现口令（口令走 0600 的 vars 文件）；
//  2. 展示用产物绝不出现 SSH 口令；
//  3. 抓取目标必须是 host:port（远程没有容器名可解析）。

func remoteTestInstance(t *testing.T) Instance {
	t.Helper()
	tpl, ok := TemplateOf(TypeRedis)
	if !ok {
		t.Fatal("Redis 模板应存在")
	}
	address, err := ParseAddress("10.0.0.11:6379", tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	return Instance{
		Name: "redis-prod-01", MWType: TypeRedis, Address: address,
		Username: "", Password: "s3cr3t-pass", Environment: "dev",
	}
}

func remoteTestOptions() RemoteOptions {
	return RemoteOptions{
		Host: "10.0.0.21", SSHUser: "ops", SSHPort: 22, SSHPassword: "ssh-secret",
		ExporterPort: 9121, InstallMode: "docker", DockerNetwork: "host", Become: true,
	}
}

func TestRemotePlaybookNeverContainsSecrets(t *testing.T) {
	tpl, _ := TemplateOf(TypeRedis)
	art, err := RenderRemoteInstall(tpl, remoteTestInstance(t), remoteTestOptions())
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	for _, secret := range []string{"s3cr3t-pass", "ssh-secret"} {
		if strings.Contains(art.Playbook, secret) {
			t.Fatalf("playbook 不得包含凭据 %q：\n%s", secret, art.Playbook)
		}
		if strings.Contains(art.MaskedVarsFile, secret) {
			t.Fatalf("展示用 vars 文件不得包含口令：%s", art.MaskedVarsFile)
		}
		if strings.Contains(art.MaskedInventory, secret) {
			t.Fatalf("展示用 inventory 不得包含 SSH 口令：%s", art.MaskedInventory)
		}
	}
	// 真实产物（落盘、0600）才包含凭据，供平台执行。
	if !strings.Contains(art.VarsFile, "s3cr3t-pass") {
		t.Fatalf("vars 文件应包含口令（用于目标机 env 文件）：%s", art.VarsFile)
	}
	if !strings.Contains(art.Inventory, "ansible_password=ssh-secret") {
		t.Fatalf("inventory 应包含 SSH 口令：%s", art.Inventory)
	}
	// 展示版必须是占位符。
	if !strings.Contains(art.MaskedInventory, "${SSH_PASSWORD}") {
		t.Fatalf("展示用 inventory 应使用占位符：%s", art.MaskedInventory)
	}
}

func TestRemoteTargetUsesHostPortAndNetworkMode(t *testing.T) {
	tpl, _ := TemplateOf(TypeRedis)
	art, err := RenderRemoteInstall(tpl, remoteTestInstance(t), remoteTestOptions())
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	// 远程抓取目标：主机 + Exporter 端口（不是容器名）。
	if art.Target != "10.0.0.21:9121" {
		t.Fatalf("抓取目标应为 10.0.0.21:9121，实际 %s", art.Target)
	}
	if art.ContainerName != "mwops-exporter-redis-prod-01" {
		t.Fatalf("容器名不符：%s", art.ContainerName)
	}
	// host 网络下不该出现端口映射（会与 host 网络冲突）。
	if strings.Contains(art.Playbook, "-p {{ exporter_port }}") {
		t.Fatalf("host 网络下不应生成 -p 映射：\n%s", art.Playbook)
	}
	// 等待就绪的端口断言。
	if !strings.Contains(art.Playbook, "port: {{ exporter_port }}") {
		t.Fatalf("playbook 应等待 Exporter 端口就绪：\n%s", art.Playbook)
	}
	// 命令里不得出现凭据。
	if strings.Contains(art.RunCommand, "ssh-secret") || strings.Contains(art.RunCommand, "s3cr3t") {
		t.Fatalf("执行命令不得包含凭据：%s", art.RunCommand)
	}
}

// TestRemoteEnvFileEscapesQuotes 锁定：含单引号/空格的口令必须被安全转义，
// 否则写进目标机的 env 文件会破坏内容甚至形成命令注入。
func TestRemoteEnvFileEscapesQuotes(t *testing.T) {
	tpl, _ := TemplateOf(TypeRedis)
	in := remoteTestInstance(t)
	in.Password = "p'w d\"x"
	art, err := RenderRemoteInstall(tpl, in, remoteTestOptions())
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if !strings.Contains(art.VarsFile, `'\''`) {
		t.Fatalf("单引号应被转义：%s", art.VarsFile)
	}
	// 转义后不能出现"裸的"单引号结尾导致内容逃逸：整行必须仍是以成对引号包裹的赋值。
	for _, line := range strings.Split(art.VarsFile, "\n") {
		if strings.HasPrefix(line, "  REDIS_PASSWORD=") && !strings.HasPrefix(line, "  REDIS_PASSWORD='") {
			t.Fatalf("口令赋值必须整体加引号：%s", line)
		}
	}
}

// TestNodeTemplateIsHostMode 锁定主机监控模板的关键约定。
//
// node_exporter 采集的是**宿主机本身**，因此必须：共享宿主网络/PID、只读挂载宿主根目录、
// 提供二进制安装信息（Release），并且不需要任何认证账号。
func TestNodeTemplateIsHostMode(t *testing.T) {
	tpl, ok := TemplateOf(TypeNode)
	if !ok {
		t.Fatal("主机监控（node）模板应存在")
	}
	if !tpl.HostNetwork || !tpl.HostPID {
		t.Fatalf("node_exporter 必须共享宿主网络与 PID：%+v", tpl)
	}
	if len(tpl.HostMounts) == 0 {
		t.Fatal("node_exporter 必须只读挂载宿主根目录，否则读到的是容器自身指标")
	}
	if tpl.NeedsAuth || tpl.MonitorUser != "" {
		t.Fatal("主机监控不需要认证账号")
	}
	if tpl.Release == nil || tpl.Release.Binary == "" {
		t.Fatal("主机监控应提供二进制安装信息（Release）")
	}
	if tpl.ExporterPort != 9100 {
		t.Fatalf("node_exporter 默认端口应为 9100，实际 %d", tpl.ExporterPort)
	}
	// 所有组件都应有二进制安装信息，否则「binary 安装方式」对它是不可用能力。
	for _, item := range Templates() {
		if item.Release == nil {
			t.Fatalf("%s 缺少 Release 元数据（二进制安装模式不可用）", item.Type)
		}
		if ReleaseURL(item, "amd64") == "" {
			t.Fatalf("%s 无法生成下载地址", item.Type)
		}
	}
}

// TestBinaryInstallPlaybook 锁定二进制 + 原生 systemd 的产物要点。
func TestBinaryInstallPlaybook(t *testing.T) {
	tpl, _ := TemplateOf(TypeNode)
	address, _ := ParseAddress("10.0.0.31:9100", tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	opts := remoteTestOptions()
	opts.InstallMode = InstallModeBinary
	opts.ExporterPort = 9100
	art, err := RenderRemoteInstall(tpl, Instance{
		Name: "node-31", MWType: TypeNode, Address: address, Environment: "dev",
	}, opts)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	for _, want := range []string{
		"uname -m",                     // 需要识别架构选包
		"ansible.builtin.get_url",      // 下载官方 release
		"ansible.builtin.unarchive",    // 解压
		"EnvironmentFile=",             // 口令走 env 文件（0600）
		"/etc/systemd/system/",         // 原生 systemd 单元
		"node_exporter-1.8.2.linux-",   // 默认下载地址规则
	} {
		if !strings.Contains(art.Playbook, want) {
			t.Fatalf("二进制安装 playbook 应包含 %q：\n%s", want, art.Playbook)
		}
	}
	// 原生运行时不得再挂 /host：--path.rootfs 必须被改写为 /
	if strings.Contains(art.Playbook, "--path.rootfs=/host") {
		t.Fatalf("二进制模式不应保留 --path.rootfs=/host：\n%s", art.Playbook)
	}
	if !strings.Contains(art.Playbook, "--path.rootfs=/") {
		t.Fatalf("二进制模式应改用 --path.rootfs=/：\n%s", art.Playbook)
	}
	// 安装方式归一化：历史值 systemd 应等同 docker-systemd，而不是二进制。
	opts.InstallMode = "systemd"
	legacy, err := RenderRemoteInstall(tpl, Instance{
		Name: "node-31", MWType: TypeNode, Address: address, Environment: "dev",
	}, opts)
	if err != nil {
		t.Fatalf("历史 systemd 值应被兼容：%v", err)
	}
	if !strings.Contains(legacy.Playbook, "docker run") {
		t.Fatalf("systemd 应归一化为 docker-systemd（容器交给 systemd 托管）：\n%s", legacy.Playbook)
	}
}

// TestBinaryInstallWithoutReleaseFails 锁定：缺少 Release 时必须明确报错，不能静默降级。
func TestBinaryInstallWithoutReleaseFails(t *testing.T) {
	tpl := Template{Type: "custom", Name: "自定义组件", Component: "custom_exporter",
		Image: "example/custom:1.0", ExporterPort: 9999, DefaultPort: 9999, MetricsPath: "/metrics"}
	opts := remoteTestOptions()
	opts.InstallMode = InstallModeBinary
	if _, err := RenderRemoteInstall(tpl, Instance{
		Name: "custom-01", MWType: "custom", Environment: "dev",
		Address: mustAddress(t, "10.0.0.9:9999", 9999),
	}, opts); err == nil {
		t.Fatal("缺少 Release 时应报错并提示改用 docker 模式")
	} else if !strings.Contains(err.Error(), "二进制安装信息") {
		t.Fatalf("错误信息应点明缺少二进制安装信息，实际：%v", err)
	}
}

// mustAddress 是测试用的地址解析助手。
func mustAddress(t *testing.T, raw string, port int) Address {
	t.Helper()
	address, err := ParseAddress(raw, port, "", "")
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	return address
}

// TestRemoteDockerHostModeFlags 锁定：宿主模式组件在**远程 docker 模式**下也必须带上
// --pid=host 与宿主根只读挂载，否则远程读到的也是容器自身指标（有数据但全是错的）。
func TestRemoteDockerHostModeFlags(t *testing.T) {
	tpl, _ := TemplateOf(TypeNode)
	address, _ := ParseAddress("10.0.0.31:9100", tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	opts := remoteTestOptions()
	opts.InstallMode = InstallModeDocker
	opts.ExporterPort = 9100
	art, err := RenderRemoteInstall(tpl, Instance{
		Name: "node-31", MWType: TypeNode, Address: address, Environment: "dev",
	}, opts)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	for _, want := range []string{"--pid=host", "/:/host:ro,rslave", "--network host"} {
		if !strings.Contains(art.Playbook, want) {
			t.Fatalf("宿主模式远程 docker 安装应包含 %q：\n%s", want, art.Playbook)
		}
	}
}

func TestRemoteBridgeModeAddsPortMapping(t *testing.T) {
	tpl, _ := TemplateOf(TypeMySQL)
	address, _ := ParseAddress("10.0.0.12:3306", tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	opts := remoteTestOptions()
	opts.DockerNetwork = "bridge"
	opts.InstallMode = "systemd"
	art, err := RenderRemoteInstall(tpl, Instance{
		Name: "mysql-01", MWType: TypeMySQL, Address: address,
		Username: "mwops_exporter", Password: "pw", Environment: "dev",
	}, opts)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if !strings.Contains(art.Playbook, "-p {{ exporter_port }}:9104") {
		t.Fatalf("bridge 网络应生成端口映射：\n%s", art.Playbook)
	}
	if !strings.Contains(art.Playbook, "systemd") {
		t.Fatalf("systemd 模式应生成单元文件任务：\n%s", art.Playbook)
	}
}
