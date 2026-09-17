package integration

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 本文件渲染「远程服务器安装 Exporter」的 Ansible 产物。
//
// 设计约定（公共平台，不绑定任何具体项目）：
//   - 平台**只渲染内置模板**的 playbook：使用者不能上传任意 playbook，避免变成远程命令执行入口；
//   - SSH 凭据只出现在 0600 的临时 inventory；Exporter 口令只出现在 0600 的 vars 文件，
//     两者都**不进命令行**（否则 ps 可见），执行完立即删除；
//   - 展示给使用者的产物一律用 ${SSH_PASSWORD} / ${MONITOR_PASSWORD} 占位；
//   - Prometheus 抓取目标是 `目标主机:Exporter端口`（远程没有容器名可解析）。

// RemoteOptions 是远程安装的平台侧参数（来自 config.Integration.Ansible）。
type RemoteOptions struct {
	Host          string
	SSHUser       string
	SSHPort       int
	ExporterPort  int
	InstallMode   string // docker（默认，官方镜像跑容器）| systemd（容器交给 systemd 托管）
	InstallDir    string
	DockerNetwork string
	Become        bool
	// SSHKeyFile 为可选的私钥路径（调用方写入 0600 临时文件）；为空则用口令认证。
	SSHKeyFile string
	// SSHPassword 仅用于渲染 0600 的临时 inventory；
	// 展示用产物必须传空串（renderInventory 会用 ${SSH_PASSWORD} 占位）。
	SSHPassword string
	ExtraArgs   []string
}

// RemoteArtifacts 是一次远程安装的全部产物。
//
// Playbook / Inventory / VarsFile 是**落盘内容**（后两者含凭据），
// 展示给使用者时必须用 Masked* 字段。
type RemoteArtifacts struct {
	Playbook  string
	Inventory string
	VarsFile  string

	MaskedPlaybook  string
	MaskedInventory string
	MaskedVarsFile  string

	// RunCommand 是平台实际执行的命令（不含任何凭据）。
	RunCommand string
	// Target 是交给 Prometheus 抓取的地址 host:port。
	Target string
	// ContainerName / UnitName 为远程对象名（便于卸载与排查）。
	ContainerName string
	UnitName      string
}

// RenderRemoteInstall 渲染远程安装产物。
//
// in.Password 只出现在 VarsFile（0600）中；MaskedVarsFile 用于界面展示。
func RenderRemoteInstall(tpl Template, in Instance, opts RemoteOptions) (RemoteArtifacts, error) {
	if err := tpl.Validate(in); err != nil {
		return RemoteArtifacts{}, err
	}
	host := strings.TrimSpace(opts.Host)
	if host == "" {
		return RemoteArtifacts{}, fmt.Errorf("远程安装需要填写目标服务器地址")
	}
	if strings.TrimSpace(opts.SSHUser) == "" {
		return RemoteArtifacts{}, fmt.Errorf("远程安装需要填写 SSH 用户名")
	}
	port := opts.ExporterPort
	if port <= 0 {
		port = tpl.ExporterPort
	}
	mode := normalizeInstallMode(opts.InstallMode)
	// 二进制模式需要官方 release 信息；缺失时明确报错并给出替代方案（不是静默降级）。
	if mode == InstallModeBinary {
		if tpl.Release == nil || tpl.Release.Repo == "" || tpl.Release.Version == "" {
			return RemoteArtifacts{}, fmt.Errorf(
				"%s 未提供二进制安装信息（Release）：请改用「docker」安装方式，"+
					"或先在组件模板里补 Release{Repo,Version,Binary}", tpl.Name)
		}
	}
	installDir := strings.TrimSpace(opts.InstallDir)
	if installDir == "" {
		installDir = "/opt/mwops-exporter"
	}
	network := strings.TrimSpace(opts.DockerNetwork)
	if network == "" {
		network = "host"
	}

	container := ContainerName(in.Name)
	unit := container + ".service"
	env := tpl.RenderEnv(in)
	args := tpl.RenderArgs(in)
	varsFile := renderVarsFile(env)
	maskedVars := renderVarsFile(maskEnv(env, in.Password))
	playbook := renderRemotePlaybook(remotePlaybookInput{
		Name: in.Name, Component: tpl.Component, Image: tpl.Image,
		Container: container, Unit: unit, EnvFile: installDir + "/" + container + ".env",
		// Port 是**宿主**端口（Prometheus 抓这个）；ContainerPort 是 Exporter 在容器内监听的端口
		// （来自模板，如 mysqld_exporter=9104）。两者在 host 网络下相同，在 bridge 下需要 -p 映射。
		Port: port, ContainerPort: tpl.ExporterPort, Network: network, InstallDir: installDir, Mode: mode,
		Become: opts.Become, Args: args,
		Release:     tpl.Release,
		BinaryPath:  installDir + "/" + releaseBinary(tpl),
		DownloadURL: ReleaseURL(tpl, ""),
		TopDir:      ReleaseTopDir(tpl, ""),
		HostPID:     tpl.HostPID,
		HostMounts:  tpl.HostMounts,
	})
	// 交给 ansible 之前先自校验：宁可在这里报「平台模板缺陷」，
	// 也不要让使用者看到 ansible 那句 unhashable type（见 playbook_validate.go）。
	if err := validatePlaybookYAML("远程安装", playbook); err != nil {
		return RemoteArtifacts{}, err
	}
	return RemoteArtifacts{
		Playbook: playbook, Inventory: renderInventory(host, opts, opts.SSHPassword), VarsFile: varsFile,
		MaskedPlaybook: playbook, MaskedInventory: renderInventory(host, opts, ""), MaskedVarsFile: maskedVars,
		RunCommand: renderAnsibleCommand(RemotePlaybookPath(in.Name), RemoteInventoryPath(in.Name), opts),
		Target:     JoinHostPort(host, port), ContainerName: container, UnitName: unit,
	}, nil
}

// 远程安装方式。
const (
	// InstallModeDocker：官方镜像跑容器（默认，目标机需有 Docker）。
	InstallModeDocker = "docker"
	// InstallModeDockerSystemd：容器交给 systemd 托管（开机自启、统一运维）。
	InstallModeDockerSystemd = "docker-systemd"
	// InstallModeBinary：下载官方 release 二进制 + 原生 systemd 服务（目标机无需 Docker）。
	InstallModeBinary = "binary"
)

// normalizeInstallMode 归一化安装方式（兼容历史值 systemd → docker-systemd）。
func normalizeInstallMode(value string) string {
	switch strings.TrimSpace(value) {
	case InstallModeBinary:
		return InstallModeBinary
	case InstallModeDockerSystemd, "systemd", "docker+systemd":
		return InstallModeDockerSystemd
	default:
		return InstallModeDocker
	}
}

// NormalizeInstallMode 对外暴露归一化规则（供服务层校验与测试复用）。
func NormalizeInstallMode(value string) string { return normalizeInstallMode(value) }

// remotePlaybookInput 是 playbook 模板的渲染入参。
type remotePlaybookInput struct {
	Name       string
	Component  string
	Image      string
	Container  string
	Unit       string
	EnvFile    string
	// Port 为宿主端口（Prometheus 抓取 / wait_for 用）。
	Port int
	// ContainerPort 为 Exporter 容器内监听端口（模板定义，如 9104）。
	ContainerPort int
	Network    string
	InstallDir string
	Mode       string
	Become     bool
	Args       []string
	// Release 为二进制安装所需的官方发布信息（binary 模式必填）。
	Release     *ReleaseSpec
	BinaryPath  string
	DownloadURL string
	TopDir      string
	// HostPID / HostMounts 为宿主模式组件（node_exporter 等）在 **docker 模式**下的必需参数：
	// 远程机器上同样要共享宿主 PID 并只读挂载宿主根，否则读到的是容器自身指标。
	HostPID    bool
	HostMounts []string
}

// renderRemotePlaybook 渲染内置 playbook 模板。
//
// 口令**不写进 playbook**：由 `-e @vars.json`（0600）传入 exporter_env_content，
// 这样即使 playbook 被复制到工单/聊天/仓库里也不含密。
func renderRemotePlaybook(in remotePlaybookInput) string {
	var b strings.Builder
	b.WriteString("# 由平台「集成中心」生成：远程安装 " + in.Component + "（集成 " + in.Name + "）\n")
	b.WriteString("# 请勿手工修改：平台按此模板执行，改动会在下次「重新应用」时被覆盖。\n")
	// 版本戳：远程安装报 YAML 语法错时，先看这一行判断平台跑的是哪一版渲染器
	// （旧镜像会显示更低的版本号，见 docs/POSTMORTEM.md INC-005）。
	b.WriteString("# 渲染器: " + PlaybookRendererVersion + "\n")
	b.WriteString("- name: 安装并启动 Exporter（" + in.Component + "）\n")
	b.WriteString("  hosts: exporter_target\n")
	if in.Become {
		b.WriteString("  become: true\n")
	}
	b.WriteString("  gather_facts: false\n")
	b.WriteString("  vars:\n")
	b.WriteString("    exporter_container: " + yamlScalar(in.Container) + "\n")
	b.WriteString("    exporter_image: " + yamlScalar(in.Image) + "\n")
	b.WriteString("    exporter_env_file: " + yamlScalar(in.EnvFile) + "\n")
	b.WriteString("    exporter_port: " + strconv.Itoa(in.Port) + "\n")
	b.WriteString("  tasks:\n")
	if in.Mode == InstallModeBinary {
		writeBinaryTasks(&b, in)
	} else {
		writeDockerTasks(&b, in)
	}
	b.WriteString("    - name: 等待 Exporter 端口就绪\n")
	b.WriteString("      ansible.builtin.wait_for:\n")
	b.WriteString("        host: 127.0.0.1\n")
	// 必须加引号：YAML 里以 {{ 开头的裸标量会被当成 flow mapping 解析，
	// ansible 会报 "found unacceptable key (unhashable type: 'AnsibleMapping')"。
	b.WriteString("        port: " + yamlScalar("{{ exporter_port }}") + "\n")
	b.WriteString("        timeout: 60\n")
	return b.String()
}

// writeDockerTasks 渲染「官方镜像跑容器」的任务；docker-systemd 模式额外生成 systemd 单元。
func writeDockerTasks(b *strings.Builder, in remotePlaybookInput) {
	b.WriteString("    - name: 校验目标机器上的 Docker 可用\n")
	b.WriteString("      ansible.builtin.command: docker version --format '{{.Server.Version}}'\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("    - name: 写入 Exporter 环境变量（含口令，权限 0600）\n")
	b.WriteString("      ansible.builtin.copy:\n")
	b.WriteString("        dest: \"{{ exporter_env_file }}\"\n")
	b.WriteString("        mode: '0600'\n")
	b.WriteString("        content: \"{{ exporter_env_content }}\"\n")
	b.WriteString("      no_log: true\n")
	b.WriteString("    - name: 拉取官方镜像\n")
	b.WriteString("      ansible.builtin.command: docker pull {{ exporter_image }}\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("    - name: 重建并启动 Exporter 容器\n")
	b.WriteString("      ansible.builtin.shell: |\n")
	b.WriteString("        docker rm -f {{ exporter_container }} >/dev/null 2>&1 || true\n")
	b.WriteString("        " + dockerRunLine(in) + "\n")
	b.WriteString("      no_log: true\n")
	if in.Mode != InstallModeDockerSystemd {
		return
	}
	b.WriteString("    - name: 用 systemd 托管容器（开机自启、统一运维）\n")
	b.WriteString("      ansible.builtin.copy:\n")
	b.WriteString("        dest: \"/etc/systemd/system/" + in.Unit + "\"\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("        content: |\n")
	for _, line := range systemdUnitLines(in) {
		b.WriteString("          " + line + "\n")
	}
	b.WriteString("      no_log: true\n")
	b.WriteString("    - name: 启动并设置开机自启\n")
	b.WriteString("      ansible.builtin.systemd:\n")
	b.WriteString("        name: " + yamlScalar(in.Unit) + "\n")
	b.WriteString("        enabled: true\n")
	b.WriteString("        state: restarted\n")
	b.WriteString("        daemon_reload: true\n")
}

// writeBinaryTasks 渲染「下载官方 release 二进制 + 原生 systemd 服务」的任务。
//
// 适用目标机没有 Docker、或不允许跑容器的场景；对应文档《Exporter 一键部署方案》2.3。
// 与容器模式的差异：
//   - 需要按 CPU 架构选包（uname -m → amd64/arm64）；
//   - 口令仍走 0600 的 env 文件，由 systemd 的 EnvironmentFile 注入；
//   - --path.rootfs 由 /host 改为 /（原生运行时的根就是宿主根）。
func writeBinaryTasks(b *strings.Builder, in remotePlaybookInput) {
	url := strings.ReplaceAll(in.DownloadURL, "{arch}", "{{ exporter_arch }}")
	version := ""
	if in.Release != nil {
		version = in.Release.Version
	}
	b.WriteString("    - name: 识别目标机 CPU 架构\n")
	b.WriteString("      ansible.builtin.command: uname -m\n")
	b.WriteString("      register: exporter_uname\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("    - name: 归一化架构名（amd64 / arm64）\n")
	b.WriteString("      ansible.builtin.set_fact:\n")
	b.WriteString("        exporter_arch: >-\n")
	b.WriteString("          {{ 'amd64' if exporter_uname.stdout | trim in ['x86_64','amd64'] " +
		"else ('arm64' if exporter_uname.stdout | trim in ['aarch64','arm64'] else 'amd64') }}\n")
	b.WriteString("    - name: 创建安装目录\n")
	b.WriteString("      ansible.builtin.file:\n")
	b.WriteString("        path: " + yamlScalar(in.InstallDir) + "\n")
	b.WriteString("        state: directory\n")
	b.WriteString("        mode: '0755'\n")
	b.WriteString("    - name: 下载官方二进制包\n")
	b.WriteString("      ansible.builtin.get_url:\n")
	b.WriteString("        url: " + yamlScalar(url) + "\n")
	b.WriteString("        dest: " + yamlScalar("/tmp/"+in.Container+".tar.gz") + "\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("    - name: 解压安装（幂等：已存在则跳过）\n")
	b.WriteString("      ansible.builtin.unarchive:\n")
	b.WriteString("        src: " + yamlScalar("/tmp/"+in.Container+".tar.gz") + "\n")
	b.WriteString("        dest: " + yamlScalar(in.InstallDir) + "\n")
	b.WriteString("        remote_src: true\n")
	b.WriteString("        extra_opts: [--strip-components=1]\n")
	b.WriteString("        creates: " + yamlScalar(in.BinaryPath) + "\n")
	b.WriteString("    - name: 写入 Exporter 环境变量（含口令，权限 0600）\n")
	b.WriteString("      ansible.builtin.copy:\n")
	b.WriteString("        dest: \"{{ exporter_env_file }}\"\n")
	b.WriteString("        mode: '0600'\n")
	b.WriteString("        content: \"{{ exporter_env_content }}\"\n")
	b.WriteString("      no_log: true\n")
	b.WriteString("    - name: 创建 systemd 服务单元\n")
	b.WriteString("      ansible.builtin.copy:\n")
	b.WriteString("        dest: \"/etc/systemd/system/" + in.Unit + "\"\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("        content: |\n")
	for _, line := range binaryUnitLines(in) {
		b.WriteString("          " + line + "\n")
	}
	b.WriteString("    - name: 启动并设置开机自启\n")
	b.WriteString("      ansible.builtin.systemd:\n")
	b.WriteString("        name: " + yamlScalar(in.Unit) + "\n")
	b.WriteString("        enabled: true\n")
	b.WriteString("        state: restarted\n")
	b.WriteString("        daemon_reload: true\n")
	b.WriteString("    - name: 记录安装来源（便于审计与排障）\n")
	b.WriteString("      ansible.builtin.copy:\n")
	b.WriteString("        dest: " + yamlScalar(in.InstallDir+"/INSTALLED_FROM") + "\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("        content: " + yamlScalar("url: "+url+"\nversion: "+version+"\n") + "\n")
}

// dockerRunLine 生成 docker run 命令行（host 网络下端口即 Exporter 自身端口，无需 -p）。
func dockerRunLine(in remotePlaybookInput) string {
	line := "docker run -d --name {{ exporter_container }} --restart unless-stopped --network " + networkOrDefault(in.Network)
	line += hostModeDockerFlags(in)
	if networkOrDefault(in.Network) != "host" {
		line += " -p {{ exporter_port }}:" + strconv.Itoa(in.ContainerPort)
	} else if listenArg := webListenArg(in); listenArg != "" {
		// host 网络下没有端口映射，端口不一致时只能让 Exporter 自己改监听地址。
		line += " " + listenArg
	}
	line += " --env-file {{ exporter_env_file }} {{ exporter_image }}"
	for _, arg := range in.Args {
		line += " " + shellArg(arg)
	}
	return line
}

// hostModeDockerFlags 生成宿主模式组件在 docker 模式下必需的参数。
//
// 为什么必须单独处理：node_exporter 采集的是**宿主机**指标，
// 容器里若不共享 PID 命名空间、不把宿主 / 只读挂到 /host，读到的全是容器自身的数字
//（CPU/内存/磁盘全错，而且看起来"有数据"，最难发现）。
func hostModeDockerFlags(in remotePlaybookInput) string {
	var b strings.Builder
	if in.HostPID {
		b.WriteString(" --pid=host")
	}
	for _, mount := range in.HostMounts {
		b.WriteString(" -v " + shellArg(mount))
	}
	return b.String()
}

// webListenArg 在"宿主端口 != 组件默认端口"时返回 --web.listen-address。
//
// 六个官方 Exporter 都支持该开关；不加的话 Prometheus 会去抓一个没人监听的端口。
func webListenArg(in remotePlaybookInput) string {
	if in.Port <= 0 || in.Port == in.ContainerPort {
		return ""
	}
	return fmt.Sprintf("--web.listen-address=:%d", in.Port)
}

// binaryUnitLines 生成原生（二进制）systemd 单元：口令通过 EnvironmentFile 注入。
func binaryUnitLines(in remotePlaybookInput) []string {
	exec := in.BinaryPath
	for _, arg := range binaryArgs(in) {
		exec += " " + arg
	}
	return []string{
		"[Unit]",
		"Description=mwops exporter " + in.Name + "（平台托管）",
		"After=network-online.target",
		"Wants=network-online.target",
		"[Service]",
		"EnvironmentFile=" + in.EnvFile,
		"ExecStart=" + exec,
		"Restart=always",
		"RestartSec=5",
		"[Install]",
		"WantedBy=multi-user.target",
	}
}

// binaryArgs 归一化二进制模式的启动参数。
//
//   - --path.rootfs：容器模式是 /host（宿主根挂载点），原生运行时根就是宿主根，改成 /；
//     该开关只属于 node_exporter，因此只在原本就带它时才补。
//   - 端口不一致时显式指定 --web.listen-address。
func binaryArgs(in remotePlaybookInput) []string {
	out := make([]string, 0, len(in.Args)+2)
	hasRootfs := false
	for _, arg := range in.Args {
		if strings.HasPrefix(arg, "--path.rootfs=") {
			hasRootfs = true
			continue
		}
		out = append(out, shellArg(arg))
	}
	if hasRootfs {
		out = append(out, "--path.rootfs=/")
	}
	if arg := webListenArg(in); arg != "" {
		out = append(out, arg)
	}
	return out
}

// ReleaseURL 生成二进制下载地址（arch 为空时保留 {arch} 占位，由 playbook 运行时决定）。
func ReleaseURL(tpl Template, arch string) string {
	rel := tpl.Release
	if rel == nil {
		return ""
	}
	if arch == "" {
		arch = "{arch}"
	}
	pattern := rel.URLTemplate
	if pattern == "" {
		pattern = "https://github.com/{repo}/releases/download/v{version}/{binary}-{version}.linux-{arch}.tar.gz"
	}
	return strings.NewReplacer(
		"{repo}", rel.Repo, "{version}", rel.Version, "{binary}", rel.Binary, "{arch}", arch,
	).Replace(pattern)
}

// ReleaseTopDir 返回解压后的顶层目录名（默认 {binary}-{version}.linux-{arch}）。
func ReleaseTopDir(tpl Template, arch string) string {
	rel := tpl.Release
	if rel == nil {
		return ""
	}
	if arch == "" {
		arch = "{arch}"
	}
	pattern := rel.TopDir
	if pattern == "" {
		pattern = "{binary}-{version}.linux-{arch}"
	}
	return strings.NewReplacer(
		"{version}", rel.Version, "{binary}", rel.Binary, "{arch}", arch,
	).Replace(pattern)
}

// releaseBinary 返回可执行文件名（缺省 exporter，仅用于纯展示/兜底）。
func releaseBinary(tpl Template) string {
	if tpl.Release == nil || strings.TrimSpace(tpl.Release.Binary) == "" {
		return "exporter"
	}
	return tpl.Release.Binary
}

// systemdUnitLines 生成 systemd 单元内容（用容器承载，避免引入发行版相关的二进制打包）。
func systemdUnitLines(in remotePlaybookInput) []string {
	return []string{
		"[Unit]",
		"Description=mwops exporter " + in.Name,
		"After=docker.service",
		"Requires=docker.service",
		"[Service]",
		"Restart=always",
		"ExecStartPre=-/usr/bin/docker rm -f " + in.Container,
		"ExecStart=" + strings.ReplaceAll(dockerRunLine(in), "{{ exporter_container }}", in.Container),
		"ExecStop=/usr/bin/docker stop " + in.Container,
		"[Install]",
		"WantedBy=multi-user.target",
	}
}

func networkOrDefault(network string) string {
	if strings.TrimSpace(network) == "" {
		return "host"
	}
	return network
}

// renderVarsFile 渲染 `-e @vars.json` 用的 YAML 变量文件（0600）。
//
// exporter_env_content 是一段多行文本，用 YAML 块标量承载，ansible 会原样写进目标机的 env 文件。
func renderVarsFile(env map[string]string) string {
	var b strings.Builder
	b.WriteString("exporter_env_content: |\n")
	for _, line := range strings.Split(strings.TrimRight(renderEnvFile(env), "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// renderEnvFile 渲染 KEY=VALUE 形式的 env 文件内容（排序保证幂等，便于审计 diff）。
func renderEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key + "=" + shellArg(env[key]) + "\n")
	}
	return b.String()
}

// maskEnv 把明文口令替换成占位，供界面展示。
func maskEnv(env map[string]string, password string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		if password != "" && strings.Contains(value, password) {
			out[key] = strings.ReplaceAll(value, password, "${MONITOR_PASSWORD}")
			continue
		}
		out[key] = value
	}
	return out
}

// renderInventory 渲染临时 inventory（含 SSH 凭据，调用方负责 0600 与删除）。
//
// password 为空表示"展示用"版本：此时写占位符，绝不把真实口令交给界面/日志。
func renderInventory(host string, opts RemoteOptions, password string) string {
	port := opts.SSHPort
	if port <= 0 {
		port = 22
	}
	var b strings.Builder
	b.WriteString("[exporter_target]\n")
	b.WriteString(host + " ansible_user=" + opts.SSHUser + " ansible_port=" + strconv.Itoa(port))
	b.WriteString(" ansible_ssh_common_args='-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/dev/null'")
	if opts.SSHKeyFile != "" {
		b.WriteString(" ansible_ssh_private_key_file=" + opts.SSHKeyFile)
	} else if password != "" {
		b.WriteString(" ansible_password=" + password)
	} else {
		b.WriteString(" ansible_password=${SSH_PASSWORD}")
	}
	b.WriteString("\n[exporter_target:vars]\nansible_python_interpreter=auto_silent\n")
	return b.String()
}

// RenderAnsibleCommand 生成执行命令（凭据都在文件里，不出现在命令行）。
func renderAnsibleCommand(playbookPath, inventoryPath string, opts RemoteOptions) string {
	parts := []string{"ansible-playbook", "-i", inventoryPath, playbookPath}
	if opts.Become {
		parts = append(parts, "--become")
	}
	parts = append(parts, opts.ExtraArgs...)
	return strings.Join(parts, " ")
}

// RemotePlaybookPath / RemoteInventoryPath 为产物落盘路径（供审计与人工复核）。
func RemotePlaybookPath(name string) string  { return "ansible/" + name + ".yml" }
func RemoteInventoryPath(name string) string { return "ansible/" + name + ".ini" }
func RemoteVarsPath(name string) string      { return "ansible/" + name + ".vars.yml" }

// JoinHostPort 拼接 host:port（IPv6 加方括号）。
func JoinHostPort(host string, port int) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	return host + ":" + strconv.Itoa(port)
}

// shellArg 按需为 shell 参数加引号（口令等敏感值也要安全落进 env 文件）。
func shellArg(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " \t\n\"'\\$`&|;<>()*?[]{}!") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
