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
	// sudo 需要密码时（登录普通用户），缺 become 口令会报 "Missing sudo password"。
	if !strings.Contains(art.Inventory, "ansible_become_password=ssh-secret") {
		t.Fatalf("inventory 应为 sudo 提供 become 口令：%s", art.Inventory)
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
	// 等待就绪的端口断言（必须带引号，否则 YAML 会把它当 flow mapping；
	// yamlScalar 产出单引号形式）。
	if !strings.Contains(art.Playbook, "port: '{{ exporter_port }}'") {
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

// TestRemoteAccountSQLPlaybook 锁定「在目标机执行账号 SQL」的产物要点。
//
// 这条路径的意义：远程集成不需要平台侧 docker.sock 也能代建只读账号。
// 因此必须固化：口令不进 playbook、不进命令行；客户端探测 + docker 回退 + 明确失败三态齐全。
func TestRemoteAccountSQLPlaybook(t *testing.T) {
	statements := []string{
		"CREATE USER IF NOT EXISTS 'mwops_exporter'@'%' IDENTIFIED WITH mysql_native_password BY 'pw'",
		"GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.* TO 'mwops_exporter'@'%'",
	}
	art, err := RenderAccountSQL(AccountSQLRequest{
		Name: "order-mysql", MWType: TypeMySQL, DBHost: "127.0.0.1", DBPort: 3306,
		ExecUser: "root", ExecPassword: "adm1n-pass", Statements: statements,
	})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	for _, want := range []string{
		"command -v mysql",         // 先探测目标机自带客户端
		"command -v docker",        // 缺失时探测 docker 作为回退
		"docker run --rm --network host",
		"MYSQL_PWD",                // 口令走环境变量
		"ansible.builtin.fail",     // 两者都没有时明确失败（不擅自装包）
		"127.0.0.1",
	} {
		if !strings.Contains(art.Playbook, want) {
			t.Fatalf("账号 SQL playbook 应包含 %q：\n%s", want, art.Playbook)
		}
	}
	// 口令绝不出现：playbook 与展示用 vars 都是占位
	if strings.Contains(art.Playbook, "adm1n-pass") {
		t.Fatalf("playbook 不得包含管理员口令：\n%s", art.Playbook)
	}
	if strings.Contains(art.MaskedVarsFile, "adm1n-pass") {
		t.Fatalf("展示用 vars 不得包含口令：%s", art.MaskedVarsFile)
	}
	if !strings.Contains(art.VarsFile, "adm1n-pass") {
		t.Fatalf("真实 vars 文件应含口令（0600、用完即删）：%s", art.VarsFile)
	}
	if strings.Contains(strings.Join(strings.Split(art.Playbook, "\n"), " "), "-p adm1n-pass") {
		t.Fatal("口令不得出现在命令行参数里")
	}
	// PostgreSQL 走 psql + PGPASSWORD，且不能再出现 mysql 专属开关
	pgArt, err := RenderAccountSQL(AccountSQLRequest{
		Name: "order-pg", MWType: TypePG, DBHost: "10.0.0.9", DBPort: 5432,
		ExecUser: "postgres", ExecPassword: "pg-pass", Statements: []string{"SELECT 1"},
	})
	if err != nil {
		t.Fatalf("PG 渲染失败：%v", err)
	}
	if !strings.Contains(pgArt.Playbook, "psql") || !strings.Contains(pgArt.Playbook, "PGPASSWORD") {
		t.Fatalf("PG 应使用 psql + PGPASSWORD：\n%s", pgArt.Playbook)
	}
	if strings.Contains(pgArt.Playbook, "--protocol=TCP") {
		t.Fatalf("psql 不应带 mysql 的 --protocol：\n%s", pgArt.Playbook)
	}
	// 不支持的组件类型必须明确报错
	if _, err := RenderAccountSQL(AccountSQLRequest{
		Name: "r", MWType: TypeRedis, DBHost: "127.0.0.1", Statements: []string{"SELECT 1"},
	}); err == nil {
		t.Fatal("Redis 不需要账号，调用远程账号 SQL 应报错")
	}
}

// TestAnonymousInstancesCanBeSaved 锁定：**无认证实例必须能保存**。
//
// 真实反馈：曾经加过一道"账号与口令都空则拒绝保存"的门槛，结果无 requirepass 的 Redis
// 既填不出账号也填不出口令，使用者被彻底卡住。正确做法是放行 + 靠 Exporter 日志自证
//（NOAUTH → 平台翻译成"需要认证/口令不一致"写回备注）。
func TestAnonymousInstancesCanBeSaved(t *testing.T) {
	for _, mwType := range []string{TypeRedis, TypeKafka, TypeNginx, TypeES} {
		tpl, ok := TemplateOf(mwType)
		if !ok {
			t.Fatalf("模板 %s 应存在", mwType)
		}
		address := mustAddress(t, "10.0.0.11:6379", tpl.DefaultPort)
		if err := tpl.Validate(Instance{
			Name: "anon-" + mwType, MWType: mwType, Address: address, Environment: "dev",
		}); err != nil {
			t.Fatalf("%s 无认证实例必须允许保存（账号与口令都可空），实际：%v", mwType, err)
		}
	}
	// 但"平台要代建只读账号"的组件仍必须给出账号名，否则建号 SQL 无从下手。
	mysqlTpl, _ := TemplateOf(TypeMySQL)
	if err := mysqlTpl.Validate(Instance{
		Name: "mysql-no-user", MWType: TypeMySQL,
		Address: mustAddress(t, "10.0.0.12:3306", 3306), Environment: "dev",
	}); err == nil {
		t.Fatal("MySQL 未填监控账号名时应报错（平台要按该名字建号）")
	}
}

// TestRenderedPlaybooksQuoteJinjaValues 锁定：YAML 里以 {{ }} 开头的值必须加引号。
//
// 真实故障：生成的 playbook 里写了 `port: {{ exporter_port }}`，YAML 把行首的 `{{`
// 当成 flow mapping 解析，ansible 直接报
//   found unacceptable key (unhashable type: 'AnsibleMapping')
// 整个远程安装（含建号）全部失败。这里对所有渲染产物做一遍扫描，防止再犯。
func TestRenderedPlaybooksQuoteJinjaValues(t *testing.T) {
	for _, mwType := range []string{TypeRedis, TypeMySQL, TypeNode} {
		tpl, _ := TemplateOf(mwType)
		address := mustAddress(t, "10.0.0.31:9100", tpl.DefaultPort)
		for _, mode := range []string{InstallModeDocker, InstallModeDockerSystemd, InstallModeBinary} {
			opts := remoteTestOptions()
			opts.InstallMode = mode
			art, err := RenderRemoteInstall(tpl, Instance{
				Name: mwType + "-01", MWType: mwType, Address: address,
				Username: "mwops_exporter", Password: "pw", Environment: "dev",
			}, opts)
			if err != nil {
				t.Fatalf("%s/%s 渲染失败：%v", mwType, mode, err)
			}
			assertJinjaValuesQuoted(t, mwType+"/"+mode, art.Playbook)
		}
	}
	accountArt, err := RenderAccountSQL(AccountSQLRequest{
		Name: "acct", MWType: TypeMySQL, DBHost: "127.0.0.1", DBPort: 3306,
		ExecUser: "root", ExecPassword: "pw",
		Statements: []string{"SELECT 1"},
	})
	if err != nil {
		t.Fatalf("账号 playbook 渲染失败：%v", err)
	}
	assertJinjaValuesQuoted(t, "account-sql", accountArt.Playbook)
}

// assertJinjaValuesQuoted 检查每一行里"冒号后的值"若以 {{ 开头则必须带引号。
//
// 只检查裸标量位置：shell/command 的值里出现 {{ 是合法的（如 `docker pull {{ img }}`），
// 因为 {{ 不在标量起始位置、不会被 YAML 当成 flow mapping。
func assertJinjaValuesQuoted(t *testing.T, label, playbook string) {
	t.Helper()
	for i, line := range strings.Split(playbook, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		idx := strings.Index(trimmed, ": ")
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(trimmed[idx+2:])
		if strings.HasPrefix(value, "{{") && !strings.HasPrefix(value, "\"") {
			t.Fatalf("%s 第 %d 行：以 {{ 开头的 YAML 值必须加引号，否则 ansible 报 unhashable type：%s",
				label, i+1, trimmed)
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

// remoteInstanceFor 按组件类型构造一个合法的远程实例（各模板对账号的要求不同）。
func remoteInstanceFor(t *testing.T, mwType string) Instance {
	t.Helper()
	tpl, ok := TemplateOf(mwType)
	if !ok {
		t.Fatalf("模板缺失：%s", mwType)
	}
	address, err := ParseAddress("10.0.0.31:9100", tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
	if err != nil {
		t.Fatalf("地址解析失败：%v", err)
	}
	return Instance{
		Name: mwType + "-01", MWType: mwType, Address: address,
		Username: "mwops_exporter", Password: "pw", Environment: "dev",
	}
}

// TestRenderedPlaybooksHaveNoGoTemplate 锁定：产物里不得出现未转义的 Go 模板语法。
//
// 真实故障 INC-006：模板里写了 `docker version --format '{{.Server.Version}}'`——
// YAML 合法、ansible 能解析，但模板渲染阶段把 `{{.` 当 Jinja 表达式，
// 直接报 `template error while templating string: unexpected '.'`，
// 「校验 Docker 可用」这一步就挂了，后面的建号与安装一步没跑。
func TestRenderedPlaybooksHaveNoGoTemplate(t *testing.T) {
	for _, mwType := range []string{TypeRedis, TypeMySQL, TypeNode} {
		tpl, _ := TemplateOf(mwType)
		for _, mode := range []string{InstallModeDocker, InstallModeDockerSystemd, InstallModeBinary} {
			opts := remoteTestOptions()
			opts.InstallMode = mode
			art, err := RenderRemoteInstall(tpl, remoteInstanceFor(t, mwType), opts)
			if err != nil {
				t.Fatalf("%s/%s 渲染失败：%v", mwType, mode, err)
			}
			for i, line := range strings.Split(art.Playbook, "\n") {
				if strings.Contains(line, "{{.") || strings.Contains(line, "{{ .") {
					t.Fatalf("%s/%s 第 %d 行含 Go 模板语法（会被 Ansible 当 Jinja 渲染而报错）：%s",
						mwType, mode, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestSystemdUnitUsesAbsoluteExecPath 锁定 systemd 单元的 Exec* 必须是绝对路径。
//
// systemd 拒绝加载 ExecStart=docker run …（"Executable path is not absolute"），
// 因此模板改用 `command -v docker` 注册到的实际路径 `{{ exporter_docker_bin.stdout | trim }}`。
func TestSystemdUnitUsesAbsoluteExecPath(t *testing.T) {
	tpl, _ := TemplateOf(TypeRedis)
	opts := remoteTestOptions()
	opts.InstallMode = InstallModeDockerSystemd
	art, err := RenderRemoteInstall(tpl, remoteTestInstance(t), opts)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if !strings.Contains(art.Playbook, "command -v docker") {
		t.Fatalf("应先解析 docker 绝对路径：\n%s", art.Playbook)
	}
	execs := 0
	for _, line := range strings.Split(art.Playbook, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "Exec") {
			continue
		}
		execs++
		value := trimmed[strings.Index(trimmed, "=")+1:]
		value = strings.TrimPrefix(value, "-") // ExecStartPre=-/path 表示忽略失败
		first := strings.SplitN(strings.TrimSpace(value), " ", 2)[0]
		if !strings.HasPrefix(first, "/") && !strings.HasPrefix(first, "{{") {
			t.Fatalf("Exec* 首个 token 必须是绝对路径或解析出的变量，实际是 %q：\n%s", first, art.Playbook)
		}
	}
	if execs < 3 {
		t.Fatalf("应生成 ExecStartPre/ExecStart/ExecStop 三条，实际 %d：\n%s", execs, art.Playbook)
	}
}

// TestPlaybookHasNoMultilineQuotedScalar 锁定：产物里不得用引号包住多行值。
//
// YAML 会把多行引号标量里的换行**折叠成空格**，于是
// `content: 'url: …\nversion: …'` 写进目标机就变成挤在一行（PyYAML 实测确认），
// 审计信息直接失真。多行内容必须用块标量（`content: |`）。
func TestPlaybookHasNoMultilineQuotedScalar(t *testing.T) {
	for _, mode := range []string{InstallModeDocker, InstallModeDockerSystemd, InstallModeBinary} {
		tpl, _ := TemplateOf(TypeNode)
		opts := remoteTestOptions()
		opts.InstallMode = mode
		art, err := RenderRemoteInstall(tpl, remoteInstanceFor(t, TypeNode), opts)
		if err != nil {
			t.Fatalf("%s 渲染失败：%v", mode, err)
		}
		for i, line := range strings.Split(art.Playbook, "\n") {
			idx := strings.Index(line, ": ")
			if idx < 0 {
				continue
			}
			value := strings.TrimSpace(line[idx+2:])
			if value == "" || (value[0] != '\'' && value[0] != '"') {
				continue
			}
			if !strings.Contains(value[1:], string(value[0])) {
				t.Fatalf("%s 第 %d 行：引号标量未在同一行闭合，YAML 会把换行折叠成空格：%s",
					mode, i+1, strings.TrimSpace(line))
			}
		}
	}
}
// TestCommandModuleAvoidsShellFeatures 锁定：ansible.builtin.command 只用于真正的可执行文件。
//
// ansible.builtin.command **不经 shell** 执行（直接 execvp），因此
//   - shell 内建（`command -v`、`cd`、`source`）会以 "No such file or directory: b'command'" 失败；
//   - 管道/重定向/`;`/`&&` 会被当成普通参数传给程序（`>` 变成字面量）。
// 需要这些能力必须用 ansible.builtin.shell。
func TestCommandModuleAvoidsShellFeatures(t *testing.T) {
	const prefix = "ansible.builtin.command:"
	shellBuiltins := []string{"command ", "cd ", "source ", "export ", ". ", "eval ", "exec ", "command -v"}
	metachars := []string{"|", ">", "<", ";", "&&", "||", "$("}
	for _, mwType := range []string{TypeRedis, TypeMySQL, TypeNode} {
		tpl, _ := TemplateOf(mwType)
		for _, mode := range []string{InstallModeDocker, InstallModeDockerSystemd, InstallModeBinary} {
			opts := remoteTestOptions()
			opts.InstallMode = mode
			art, err := RenderRemoteInstall(tpl, remoteInstanceFor(t, mwType), opts)
			if err != nil {
				t.Fatalf("%s/%s 渲染失败：%v", mwType, mode, err)
			}
			for i, line := range strings.Split(art.Playbook, "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, prefix) {
					continue
				}
				value := strings.TrimSpace(trimmed[len(prefix):])
				for _, builtin := range shellBuiltins {
					if strings.HasPrefix(value, builtin) {
						t.Fatalf("%s/%s 第 %d 行：command 模块不能用 shell 内建（%q），请改用 shell 模块：%s",
							mwType, mode, i+1, builtin, trimmed)
					}
				}
				for _, meta := range metachars {
					if strings.Contains(value, meta) {
						t.Fatalf("%s/%s 第 %d 行：command 模块不解析 %q（不经 shell），请改用 shell 模块：%s",
							mwType, mode, i+1, meta, trimmed)
					}
				}
			}
		}
	}

	// 客户端探测依赖 shell 内建，必须是 shell 模块。
	accountArt, err := RenderAccountSQL(AccountSQLRequest{
		Name: "acct", MWType: TypeMySQL, DBHost: "127.0.0.1", DBPort: 3306,
		ExecUser: "root", ExecPassword: "pw", Statements: []string{"SELECT 1"},
	})
	if err != nil {
		t.Fatalf("账号 playbook 渲染失败：%v", err)
	}
	if !strings.Contains(accountArt.Playbook, "ansible.builtin.shell: command -v mysql") {
		t.Fatalf("探测客户端必须用 shell 模块：\n%s", accountArt.Playbook)
	}
	if strings.Contains(accountArt.Playbook, "ansible.builtin.command: command -v") {
		t.Fatalf("command 模块不能用 shell 内建 command -v：\n%s", accountArt.Playbook)
	}
}

// TestValidatePlaybookYAML 锁定运行时自校验（INC-005 的第二道防线）。
//
// 回归测试只能守住"当前模板"；渲染器还要在执行前自己解析一遍，
// 这样即使将来又写出非法 YAML，使用者在界面上看到的是
// 「第 N 行 …不是合法 YAML」而不是 ansible 那句 unhashable type。
func TestValidatePlaybookYAML(t *testing.T) {
	broken := "- name: 安装\n  hosts: exporter_target\n  tasks:\n" +
		"    - name: 等待 Exporter 端口就绪\n" +
		"      ansible.builtin.wait_for:\n" +
		"        host: 127.0.0.1\n" +
		"        port: {{ exporter_port }}\n"
	err := validatePlaybookYAML("远程安装", broken)
	if err == nil {
		t.Fatal("裸 {{ 值必须被拦下，否则 ansible 只会在执行阶段报 unhashable type")
	}
	if !strings.Contains(err.Error(), "第 7 行") {
		t.Fatalf("错误信息应给出行号：%v", err)
	}
	if !strings.Contains(err.Error(), "port: {{ exporter_port }}") {
		t.Fatalf("错误信息应给出原文：%v", err)
	}
	if !strings.Contains(err.Error(), "{{ 开头的值必须加引号") {
		t.Fatalf("错误信息应说明修法：%v", err)
	}

	// 单引号修正后必须通过。
	fixed := strings.Replace(broken, "port: {{ exporter_port }}", "port: '{{ exporter_port }}'", 1)
	if err := validatePlaybookYAML("远程安装", fixed); err != nil {
		t.Fatalf("加引号后应通过：%v", err)
	}

	// 行中出现 {{ 是合法的（shell 命令里很常见），不得误报。
	inline := "- name: 安装\n  hosts: all\n  tasks:\n    - name: 拉取镜像\n" +
		"      ansible.builtin.command: docker pull {{ exporter_image }}\n"
	if err := validatePlaybookYAML("远程安装", inline); err != nil {
		t.Fatalf("行中的 {{ 不应被误判：%v", err)
	}

	// 其它结构性错误交给真正的 YAML 解析器兜住。
	if err := validatePlaybookYAML("远程安装", "key: [unclosed\n"); err == nil {
		t.Fatal("非法 YAML 结构应被解析器拦下")
	}

	// Go 模板语法必须被拦下（INC-006）：YAML 合法，但会被 Ansible 当 Jinja 渲染而报错。
	goTpl := "- name: 安装\n  hosts: all\n  tasks:\n    - name: 校验 docker\n" +
		"      ansible.builtin.command: docker version --format '{{.Server.Version}}'\n"
	goErr := validatePlaybookYAML("远程安装", goTpl)
	if goErr == nil {
		t.Fatal("Go 模板语法 {{.X}} 必须被拦下，否则 Ansible 报 unexpected '.'")
	}
	if !strings.Contains(goErr.Error(), "第 5 行") || !strings.Contains(goErr.Error(), "Jinja") {
		t.Fatalf("错误信息应给出行号并说明会被当 Jinja 渲染：%v", goErr)
	}
	// 用 {% raw %} 包裹是合法写法，不得误报。
	raw := strings.Replace(goTpl, "'{{.Server.Version}}'", "'{% raw %}{{.Server.Version}}{% endraw %}'", 1)
	if err := validatePlaybookYAML("远程安装", raw); err != nil {
		t.Fatalf("{%% raw %%} 包裹后应通过：%v", err)
	}
}

// TestPlaybookRendererVersionStamped 锁定产物第 3 行的渲染器版本戳。
//
// 远程安装报 YAML 语法错时，先看这一行即可区分
// 「平台模板有缺陷」与「后端镜像未重建、仍在跑旧渲染器」。
func TestPlaybookRendererVersionStamped(t *testing.T) {
	tpl, _ := TemplateOf(TypeRedis)
	art, err := RenderRemoteInstall(tpl, remoteTestInstance(t), remoteTestOptions())
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if !strings.Contains(art.Playbook, "# 渲染器: "+PlaybookRendererVersion) {
		t.Fatalf("安装 playbook 应带渲染器版本戳：\n%s", art.Playbook)
	}
	if got := strings.SplitN(art.Playbook, "\n", 4); len(got) < 3 || !strings.Contains(got[2], PlaybookRendererVersion) {
		t.Fatalf("版本戳应在第 3 行（便于一眼确认镜像新旧）：\n%s", art.Playbook)
	}

	accountArt, err := RenderAccountSQL(AccountSQLRequest{
		Name: "redis-prod-01", MWType: TypeMySQL, DBHost: "127.0.0.1", DBPort: 3306,
		ExecUser: "root", ExecPassword: "pw", Statements: []string{"SELECT 1"},
	})
	if err != nil {
		t.Fatalf("账号 playbook 渲染失败：%v", err)
	}
	if !strings.Contains(accountArt.Playbook, "# 渲染器: "+PlaybookRendererVersion) {
		t.Fatalf("账号 playbook 应带渲染器版本戳：\n%s", accountArt.Playbook)
	}
}
