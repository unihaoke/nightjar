package integration

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// 本文件渲染「日志集成」的远程产物：一份**幂等**安装 Filebeat 的 playbook 与变量文件。
//
// 与 ansible.go（安装 Exporter）共享同一套约定：
//   - SSH 凭据只出现在 0600 的 inventory；展示用产物一律 ${SSH_PASSWORD} 占位；
//   - become 由 opts.Become 控制；
//   - 交给 ansible 之前先 validatePlaybookYAML 自校验（INC-005 / INC-006 的教训）。
//
// 但有一条**本质差异**：日志集成没有 Exporter 端口，也就不存在"等端口就绪"这一步。
// 它的成败判据是「Filebeat 是否在跑 + 配置是否合法 + 能不能连上 Kafka」，
// 因此末尾给出 filebeat test config / test output 两条**尽力而为**的自检（见 writeLogVerifyTasks）。
//
// 幂等规则（用户明确要求"如果存在则不需要部署"，docs/LOG_INTEGRATION.md §四）：
//  1. 先探测 `command -v filebeat` 与 `systemctl is-active filebeat`；
//  2. package 模式只在"未安装"时安装（deb/rpm）；
//  3. docker 模式只在容器不存在或镜像/配置变化时重建；
//  4. auto 模式按 已安装 → docker → package 的顺序兜底；
//  5. 配置内容不变 → copy 的 checksum 判定为 ok → **不触发** restart handler。

// 日志集成在目标机上的固定对象名。
const (
	// FilebeatContainerName 是 docker 安装方式下的容器名。
	//
	// 刻意**不用** ContainerName(name)（那是 mwops-exporter-<name>）：容器名要能一眼看出
	// 这条链路是日志采集，而不是导出指标；同时固定成单一名字，避免同一台机器上装了多个
	// 日志集成时容器互相覆盖（集成名体现在配置目录与配置文件里，不体现在容器名）。
	FilebeatContainerName = "mwops-filebeat"
	// FilebeatUnitName 是 package 安装方式下的 systemd 单元名。
	FilebeatUnitName = "filebeat.service"
	// filebeatConfigDirDefault 是配置目录（集成名作为子目录，机器上可并存多个集成）。
	filebeatConfigDirDefault = "/opt/mwops/filebeat"
	// filebeatRemoteConfigPath 是 package 模式使用的绝对路径。
	//
	// 必须是这个路径：官方 unit 写死了 `filebeat -c /etc/filebeat/filebeat.yml`，
	// 换成别的路径必须自己写单元文件（那会让"复用目标机已装的 Filebeat"变成不可能）。
	filebeatRemoteConfigPath = "/etc/filebeat/filebeat.yml"
	// filebeatDataDir 是平台托管的 Filebeat 数据目录（注册表/位点 + 缓冲），
	// docker 模式挂进容器，保证容器重建后断点续传不失效。
	//
	// 刻意**不用**系统包安装的 /var/lib/filebeat：那是"系统 filebeat"的注册表与位点目录，
	// 两者是**两套互不干扰的采集状态**。如果 docker 模式复用它，就等于：
	//   - 把系统 filebeat 的注册表 chown 给 1000；
	//   - 再往里写容器版 Filebeat 的注册表；
	// 结果是系统 filebeat 重启后可能重复采集、或状态错乱（同一批日志被两套采集器反复读），
	// 而且在平台上表现为"日志重复/丢失"，很难追到是这里。
	filebeatDataDir = "/var/lib/mwops-filebeat"
	// filebeatContainerDataDir 是**容器内**的数据目录，必须与官方镜像约定一致。
	filebeatContainerDataDir = "/usr/share/filebeat/data"
	// filebeatDebURLTemplate / filebeatRPMURLTemplate 是官方制品地址（package 模式的兜底来源）。
	filebeatDebURLTemplate = "https://artifacts.elastic.co/downloads/beats/filebeat/filebeat-{version}-amd64.deb"
	filebeatRPMURLTemplate = "https://artifacts.elastic.co/downloads/beats/filebeat/filebeat-{version}-x86_64.rpm"
)

// RenderFilebeatInstall 渲染日志集成的远程安装产物。
//
// 返回的 RemoteArtifacts 复用 Exporter 那套结构（产物文件 + 展示用脱敏版本 + 执行命令），
// 其中 Target 为 opts.Host（日志集成没有 Exporter 端口）、UnitName 为 filebeat.service、
// ContainerName 为 mwops-filebeat，便于平台按同一套逻辑做自检与卸载。
//
// 已知边界（未在产物里覆盖，交付时须知悉）：
//   - 复用分支不会主动 `systemctl start`：目标机上"装了但没启用/没在跑"的 Filebeat
//     只会被下发新配置（配置变化时 handler 会 restart，从而把它拉起来）；
//     需要"一定在跑"时请先用 package/docker 模式重建，或人工 systemctl enable --now filebeat。
func RenderFilebeatInstall(in LogInput, opts RemoteOptions) (RemoteArtifacts, error) {
	host := strings.TrimSpace(opts.Host)
	if host == "" {
		return RemoteArtifacts{}, fmt.Errorf("远程安装需要填写目标服务器地址")
	}
	if strings.TrimSpace(opts.SSHUser) == "" {
		return RemoteArtifacts{}, fmt.Errorf("远程安装需要填写 SSH 用户名")
	}
	mode, err := normalizeLogInstallMode(in.InstallMode)
	if err != nil {
		return RemoteArtifacts{}, err
	}
	// 先渲染 filebeat.yml：路径/topic/hosts 的校验都在里面，校验不过就不该产出任何 playbook。
	// （把配置当字符串传进 playbook 是刻意的：ansible 会先做 Jinja 渲染，
	//   内联的 YAML 里只要有 {% 或 {{ 就会先被当成模板表达式而报错，见 INC-005/INC-006。）
	config, err := RenderFilebeatConfig(in)
	if err != nil {
		return RemoteArtifacts{}, err
	}
	if err := validateYAMLParse("filebeat.yml", config); err != nil {
		return RemoteArtifacts{}, err
	}
	version := normalizeFilebeatVersion(in.FilebeatVersion)
	configDir := strings.TrimSpace(opts.InstallDir)
	if configDir == "" {
		configDir = filebeatConfigDirDefault
	}
	// 去掉尾部的 /，避免拼出 //filebeat.yml（不同发行版的 Filebeat 对路径都宽容，
	// 但产物的可读性与 diff 会变差）。
	configDir = strings.TrimRight(configDir, "/")
	if configDir == "" {
		configDir = "/"
	}

	playbook := renderFilebeatPlaybook(filebeatPlaybookInput{
		Name: in.Name, Version: version, Mode: mode, Become: opts.Become,
		ConfigDir: configDir,
		// 远程绝对路径与容器内路径在**本平台**恰好相同（都指向 /etc/filebeat/filebeat.yml）：
		// docker 模式把宿主上的这份文件只读挂进容器，容器内 Filebeat 用默认路径即可，
		// 不必再传 -c（官方镜像入口默认读这个位置）。
		ConfigPath: filebeatRemoteConfigPath,
		MirrorPath: configDir + "/filebeat.yml",
		DataDir:    filebeatDataDir,
		Container:  FilebeatContainerName,
		Image:      filebeatImage(version),
		LogMounts:  logMountDirs(in.Paths),
		KafkaProbe: firstKafkaHost(in.KafkaHosts),
	})
	if err := validatePlaybookYAML("日志集成（Filebeat 安装）", playbook); err != nil {
		return RemoteArtifacts{}, err
	}

	varsFile := renderFilebeatVars(config, mode, false)
	if err := ensureFilebeatVarsParsable("日志集成（Filebeat 安装）", varsFile); err != nil {
		return RemoteArtifacts{}, err
	}
	// 展示用版本：自检命令里的平台地址换成说明文字（截图/工单里不该出现内网拓扑）。
	// filebeat.yml 本身不含任何凭据（Kafka 未开 SASL 时也没有口令），因此配置内容原样展示。
	maskedVars := renderFilebeatVars(config, mode, true)
	if err := ensureFilebeatVarsParsable("日志集成（Filebeat 安装）", maskedVars); err != nil {
		return RemoteArtifacts{}, err
	}
	return RemoteArtifacts{
		Playbook: playbook, Inventory: renderInventory(host, opts, opts.SSHPassword), VarsFile: varsFile,
		MaskedPlaybook: playbook, MaskedInventory: renderInventory(host, opts, ""), MaskedVarsFile: maskedVars,
		RunCommand: renderAnsibleCommand(RemotePlaybookPath(in.Name), RemoteInventoryPath(in.Name), opts),
		// 日志集成没有 Exporter 端口：Target 就是主机本身（自检第 3 步的探测目标由命令决定）。
		Target: host, ContainerName: FilebeatContainerName, UnitName: FilebeatUnitName,
	}, nil
}

// normalizeLogInstallMode 校验安装方式（不静默降级：写错的值必须报错，
// 否则使用者以为选了 docker，实际却按 package 装了一套 —— 排查会从错误的方向开始）。
func normalizeLogInstallMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", LogInstallAuto:
		return LogInstallAuto, nil
	case LogInstallPackage:
		return LogInstallPackage, nil
	case LogInstallDocker:
		return LogInstallDocker, nil
	default:
		return "", fmt.Errorf("安装方式 %q 不合法：只能是 %s / %s / %s",
			value, LogInstallAuto, LogInstallPackage, LogInstallDocker)
	}
}

// normalizeFilebeatVersion 归一化版本号（去掉可能被顺手粘进来的 v 前缀）。
func normalizeFilebeatVersion(value string) string {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return "8.16.0"
	}
	return trimmed
}

// filebeatImage 返回官方镜像坐标。
func filebeatImage(version string) string {
	return filebeatDefaultImageRepo + ":" + version
}

// filebeatVerifyCommands 生成两条**尽力而为**的自检命令（在目标机上执行）。
//
//   - test config：配置文件是否合法。写错一处就再也采不到日志，这是最值得先跑的一条；
//   - test output：能否连上 Kafka 并完成一次握手。**专门用来抓 advertised 地址配错**——
//     平台侧看起来一切正常，目标机却在报 dial tcp 127.0.0.1:9092。
//
// masked 为 true 时把命令体换成说明文字：展示用产物不该把平台内网地址带进截图/工单。
//
// 不写成"两条独立命令"是为了在 docker 模式下也能用：统一交给 `sh -c` 顺序执行更稳。
func filebeatVerifyCommands(mode string, masked bool) string {
	commands := "filebeat test config; filebeat test output"
	if masked {
		commands = "filebeat test config; filebeat test output（目标地址见平台 Kafka 配置）"
	}
	if mode == LogInstallDocker {
		// docker 模式：filebeat 在容器里，必须在容器内执行（宿主上没有 filebeat 二进制）。
		return "docker exec {{ " + filebeatCardinality().container + " }} sh -c " + shellArg(commands)
	}
	return commands
}

// filebeatCardinality 集中管理 playbook 里的变量名，避免散落的字符串拼写出错。
type filebeatVars struct {
	container string
	unit      string
	config    string
	// parent 是 config 的父目录；dir 是集成自己的配置目录（放可读副本）。
	// 两者都由变量表达：INC-013 的根因之一就是"配置目录"有两个来源
	//（一个是 /etc/filebeat，一个是 opts.InstallDir），最终谁也没保证它存在。
	parent string
	dir    string
	mirror string
	image  string
	mode   string
	// containerConfig 是**容器内**的配置文件路径，必须与官方镜像的默认路径一致
	// （见 filebeatContainerConfigPath 的说明）。用变量表达，避免 docker run 与
	// 排障说明里各写一遍、日后改一处漏一处。
	containerConfig string
}

func filebeatCardinality() filebeatVars {
	return filebeatVars{
		container:       "filebeat_container",
		unit:            "filebeat_unit",
		config:          "filebeat_config_path",
		parent:          "filebeat_config_parent_dir",
		dir:             "filebeat_config_dir",
		mirror:          "filebeat_mirror_path",
		image:           "filebeat_image",
		mode:            "filebeat_tested_mode",
		containerConfig: "filebeat_container_config_path",
	}
}

// filebeatContainerConfigPath 是**容器内**配置文件必须落在的路径。
//
// 为什么必须是 /usr/share/filebeat/filebeat.yml（INC-014 的根因）：
// Filebeat 官方 Docker 镜像的工作目录与默认配置路径就是 /usr/share/filebeat/，官方文档的
// 卷挂载示例也是把配置挂到这个路径（https://www.elastic.co/docs/reference/beats/filebeat/running-on-docker）。
// 之前挂到 /etc/filebeat/filebeat.yml 看着"像那么回事"（package 模式确实用这个路径），
// 但容器里 Filebeat **根本不会读它** —— 它会用镜像内置的默认配置去连 Elasticsearch，
// 必然失败并崩溃重启；表现出来就是"部署成功、平台一条日志都没有"，而且正好触发
// Docker 反复重建绑定源目录的那个循环（INC-013/INC-014 是同一个坑的两面）。
//
// ⚠️ 改这个常量等于改变"容器读哪份配置"，改错不会有报错、只会没有日志，请勿随手改回 /etc/filebeat。
const filebeatContainerConfigPath = "/usr/share/filebeat/filebeat.yml"

// logMountDirs 从日志路径反推要在 docker 模式下挂进容器的目录。
//
// 为什么必须挂载：容器里的 Filebeat 只能看见自己命名空间内的文件系统，
// 不挂载宿主日志目录的话 paths 一个都匹配不到 —— 现象是"容器在跑、平台没有日志"，
// 而且 `docker logs` 里连报错都没有（Filebeat 只是安静地没有匹配到文件）。
//
// 只挂"最具体的那一层"：/var/log/app/*.log → /var/log/app，
// /var/log/*.log → /var/log，/data/logs/**/*.log → /data/logs。
// 顶层目录（/var、/data、/opt…）一律跳过：挂整个 /var 进容器会连 /var/lib/docker 一起带上，
// 既拖慢启动也可能把宿主敏感内容暴露给容器；这类路径请直接用 package 安装方式。
func logMountDirs(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, raw := range normalizeLogPaths(paths) {
		dir := literalDirPrefix(raw)
		// 顶层目录（/var、/data、/opt…）一律跳过：挂整个 /var 进容器会连 /var/lib/docker
		// 一起带上，既拖慢启动也可能把宿主敏感内容暴露给容器；这类路径请用 package 安装方式。
		if dir == "" || strings.Count(dir, "/") < 2 {
			continue
		}
		if seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	return out
}

// literalDirPrefix 返回 glob 路径里"确定存在"的那一级目录。
//
// 逐段扫描、遇到含通配符的段就停下：段含通配符说明它可能匹配多个文件或目录，
// 挂载它的父目录才既安全（不多挂）又够用（不漏挂）。
//
//	/var/log/app/*.log   → /var/log/app
//	/var/log/*.log       → /var/log
//	/data/logs/**/*.log  → /data/logs
//	/var/log/app.log     → /var/log
//	/*.log 或 /var/*.log → ""（没有任何确定目录，交由调用方跳过）
func literalDirPrefix(pattern string) string {
	segments := strings.Split(strings.TrimSpace(pattern), "/")
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		if strings.ContainsAny(segment, "*?[") {
			break
		}
		parts = append(parts, segment)
	}
	if len(parts) == 0 {
		return ""
	}
	// 恰好是绝对路径且没有通配符（/var/log/app.log）时，最后一段是文件名而非目录。
	if !strings.ContainsAny(pattern, "*?[") {
		if len(parts) < 2 {
			return ""
		}
		parts = parts[:len(parts)-1]
	}
	return "/" + strings.Join(parts, "/")
}

// filebeatPlaybookInput 是日志安装 playbook 的渲染入参。
type filebeatPlaybookInput struct {
	Name       string
	Version    string
	Mode       string
	Become     bool
	ConfigDir  string
	ConfigPath string
	// MirrorPath 是配置目录里的副本（保留目标机上"人可读"的一份，便于排障时对比）。
	MirrorPath string
	DataDir    string
	Container  string
	Image      string
	// KafkaProbe 是目标机上要探测的平台 Kafka 地址（取 in.KafkaHosts[0]，与配置里的一致）。
	KafkaProbe string
	// LogMounts 为 docker 模式下要挂进容器的宿主日志目录。
	LogMounts []string
}

// renderFilebeatPlaybook 渲染完整的安装 playbook。
func renderFilebeatPlaybook(in filebeatPlaybookInput) string {
	v := filebeatCardinality()
	var b strings.Builder
	b.WriteString("# 由平台「集成中心」生成：日志集成（Filebeat " + in.Version + "，集成 " + in.Name + "）。\n")
	b.WriteString("# 请勿手工修改：平台按此模板执行，改动会在下次「重新应用」时被覆盖。\n")
	// 版本戳与 Exporter 产物保持一致的位置（第 3 行）：远程安装报错时先看这一行。
	// 注意：新增注释要排在它**之后**，否则会破坏这个位置约定。
	b.WriteString("# 渲染器: " + PlaybookRendererVersion + "\n")
	// 容器内配置路径必须与镜像默认一致——写进产物注释，避免以后有人"顺手改回 /etc/filebeat/"。
	// 这类改动不会报错，只会让容器用镜像内置默认配置去连 Elasticsearch，表现为"部署成功但没有日志"。
	b.WriteString("# 容器内配置路径固定为 " + filebeatContainerConfigPath +
		"（必须与 Filebeat 官方镜像的默认路径一致，改回 /etc/filebeat/ 会导致容器读不到平台下发的配置）。\n")
	b.WriteString("- name: 部署 Filebeat 日志采集（" + in.Name + "）\n")
	b.WriteString("  hosts: exporter_target\n")
	if in.Become {
		b.WriteString("  become: true\n")
	}
	// gather_facts: false —— playbook 里刻意不依赖任何 fact（ansible_os_family 等在此模式下
	// 是未定义变量，直接引用会报 undefined）。发行版判定改为读 /etc/debian_version。
	b.WriteString("  gather_facts: false\n")
	b.WriteString("  vars:\n")
	b.WriteString("    filebeat_version: " + yamlScalar(in.Version) + "\n")
	b.WriteString("    filebeat_mode: " + yamlScalar(in.Mode) + "\n")
	b.WriteString("    filebeat_container: " + yamlScalar(in.Container) + "\n")
	b.WriteString("    filebeat_unit: " + yamlScalar(FilebeatUnitName) + "\n")
	b.WriteString("    filebeat_config_path: " + yamlScalar(in.ConfigPath) + "\n")
	// 父目录也用变量表达（由 ConfigPath 推导，见 configParentDir）：INC-013 的教训是
	// "配置目录"一旦有两个来源（写死的 /etc/filebeat 与 opts.InstallDir），
	// 就会出现"建了 A、却往 B 写"的分叉，而这次的代价是产出一份必然失败的 playbook。
	b.WriteString("    " + v.parent + ": " + yamlScalar(configParentDir(in.ConfigPath)) + "\n")
	b.WriteString("    filebeat_config_dir: " + yamlScalar(in.ConfigDir) + "\n")
	b.WriteString("    filebeat_mirror_path: " + yamlScalar(in.MirrorPath) + "\n")
	b.WriteString("    filebeat_data_dir: " + yamlScalar(in.DataDir) + "\n")
	b.WriteString("    filebeat_image: " + yamlScalar(in.Image) + "\n")
	// 容器内配置路径也走变量：docker run 与排障说明必须同源，避免"改了一处漏一处"。
	b.WriteString("    " + v.containerConfig + ": " + yamlScalar(filebeatContainerConfigPath) + "\n")
	b.WriteString("  tasks:\n")
	writeFilebeatDetectTasks(&b)
	writeFilebeatConfigDirTask(&b)
	writeFilebeatResolveTasks(&b, v)
	// 顺序是**刻意**的，改动前请先读下面这段（线上真实故障 INC-013 / INC-014）：
	//
	//	① docker 的单文件挂载 `-v <宿主缺失路径>:<容器内配置>` 会让 Docker
	//	   **把缺失的宿主路径创建成目录**。如果先起容器、后 copy 配置，
	//	   copy 就会拿到一个目录当 dest，直接报：
	//	     can not use content with a dir as dest
	//	② 更糟的是这个错误状态会**自我维持**：容器里的 Filebeat 读不到有效配置 → 崩溃 →
	//	   `--restart=always` 让它反复重启 → **每次重启都把缺失的绑定源重新创建成目录**。
	//	   于是"删目录 → 写文件"之间又被插回一个目录，自愈等于白做
	//	   （INC-014 现场：自愈任务 changed=1 确实删掉了目录，紧接着的 copy 依然报同一个错）。
	//	   所以清理必须把**旧容器一起删掉**：`docker rm -f` 才是打断这个循环的那一步——
	//	   它不只是"让新配置生效"，而是让 Docker 失去"重建这个绑定源目录"的机会。
	//
	// 因此固定为：
	//	  建目录 → 自愈历史遗留（非普通文件则删）→ 删平台托管的旧容器（打断重建循环）
	//	  → 下发 filebeat.yml（容器挂载前文件必须已存在且是普通文件）
	//	  → assert 是普通文件 → 起容器 → Kafka 连通性探测 → 自检
	writeFilebeatHealTasks(&b, v)
	writeFilebeatContainerRemoveTask(&b, v, in.Mode)
	writeFilebeatConfigTask(&b, in)
	switch in.Mode {
	case LogInstallPackage:
		writeFilebeatPackageTasks(&b, in, v)
	case LogInstallDocker:
		writeFilebeatDockerTasks(&b, in, v)
	default:
		// auto：三条分支都要在产物里，由 when 条件决定实际执行哪一条，
		// 这样"为什么这台机器用了 package 而不是 docker"可以直接从产物里读出来。
		writeFilebeatAutoTasks(&b, in, v)
	}
	writeFilebeatKafkaProbeTask(&b, in)
	writeFilebeatVerifyTasks(&b, in)
	// handlers 必须放在最后：Ansible 要求它和 tasks 同级，且写在 tasks 之后更贴近
	// "任务怎么触发重启"的阅读顺序。
	writeFilebeatHandlers(&b, in)
	return b.String()
}

// writeFilebeatHandlers 渲染重启 handler。
//
// 为什么重启只能放在 handler 里（而不是配置下发任务之后跟一条 restart）：
// ansible.builtin.copy 默认按 **checksum** 比对目标文件内容，内容没变时任务报 ok、
// **不会**通知 handler；一旦把重启写成普通任务，每次重放 playbook 都会重启一次 Filebeat，
// 采集出现抖动（"重复点击集成导致日志断流"就是这么来的）。
func writeFilebeatHandlers(b *strings.Builder, in filebeatPlaybookInput) {
	v := filebeatCardinality()
	b.WriteString("  handlers:\n")
	// handler 的层级比 tasks 浅一级（tasks 里的任务缩进 4 空格，handler 只有 2 空格），
	// 因此这里不用 logTask（那是给 tasks 用的），避免把 handler 写进上一层导致 YAML 结构错位。
	b.WriteString("  - name: 重启 Filebeat\n")
	// 单条 shell 同时覆盖 package 与 docker 两种落地形式（filebeat_mode 是前面 set_fact
	// 算出来的实际方式，set_fact 的变量在 handler 里同样可见）：
	//   - package / reuse（目标机已装）：systemctl restart filebeat.service；
	//   - docker：重建容器（配置是只读挂载进去的，容器必须重建才会读到新配置）；
	//   - 兜底：连 docker 都没有时至少尝试 systemd，避免"配置变了却没生效"却静默通过。
	// 刻意用一条任务而不是"两个同名 handler"：notify 的名字只能对应一个 handler，
	// 拆开会让 package 与 docker 两条链路里总有一条永远收不到通知。
	b.WriteString("    ansible.builtin.shell: |\n")
	b.WriteString("      set -eu\n")
	b.WriteString("      if [ \"{{ " + v.mode + " }}\" = \"docker\" ]; then\n")
	b.WriteString("        docker rm -f {{ " + v.container + " }} >/dev/null 2>&1 || true\n")
	b.WriteString("        " + dockerRunLineForFilebeat(in, v) + "\n")
	b.WriteString("      else\n")
	b.WriteString("        systemctl restart {{ " + v.unit + " }}\n")
	b.WriteString("      fi\n")
}

// writeFilebeatDetectTasks 先探测再决策（用户明确要求："如果存在则不需要部署"）。
func writeFilebeatDetectTasks(b *strings.Builder) {
	b.WriteString("    # ---- 探测：已安装 / 运行中 就不重装（幂等的第一步，也是唯一事实来源） ----\n")
	// 必须用 shell 模块：`command -v` 是 shell 内建，ansible.builtin.command 不经 shell（INC-005 同类）。
	logTask(b, "探测目标机是否已安装 filebeat")
	b.WriteString("      ansible.builtin.shell: command -v filebeat\n")
	b.WriteString("      register: filebeat_bin\n")
	// failed_when: false —— 命令不存在时 rc=1，这是**正常结果**而不是失败；
	// 不这样写整个 playbook 会在最开始的探测就中断（"未安装"反而变成了报错）。
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	logTask(b, "探测 filebeat 服务是否正在运行")
	b.WriteString("      ansible.builtin.shell: systemctl is-active filebeat.service\n")
	b.WriteString("      register: filebeat_service\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	logTask(b, "探测目标机是否有 docker（docker 安装方式的回退依据）")
	b.WriteString("      ansible.builtin.shell: command -v docker\n")
	b.WriteString("      register: filebeat_docker_bin\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	logTask(b, "读取发行版标识（决定用 deb 还是 rpm）")
	// 不用 ansible_os_family：gather_facts 关闭时该变量不存在，引用会直接报 undefined；
	// 读一个文件反而更可靠（Debian 系一定有 /etc/debian_version）。
	b.WriteString("      ansible.builtin.stat:\n")
	b.WriteString("        path: /etc/debian_version\n")
	b.WriteString("      register: filebeat_debian\n")
}

// writeFilebeatConfigDirTask 准备配置目录。
//
// 两个目录都要建（顺序也重要：先建 filebeat.yml 的父目录，再建集成自己的配置目录）：
//   - filebeat_config_parent_dir（固定为 /etc/filebeat）：copy 的 dest 是它下面的文件，
//     父目录不存在时 copy 同样会失败；历史上这里只建了集成配置目录（/opt/mwops/filebeat），
//     父目录靠"docker 挂载顺手创建"——而 docker 创建出来的是**目录**，于是有了 INC-013；
//   - filebeat_config_dir（/opt/mwops/filebeat）：保留一份可读副本的下发目录。
func writeFilebeatConfigDirTask(b *strings.Builder) {
	v := filebeatCardinality()
	logTask(b, "准备 Filebeat 配置目录")
	b.WriteString("      ansible.builtin.file:\n")
	b.WriteString("        path: \"{{ " + v.parent + " }}\"\n")
	b.WriteString("        state: directory\n")
	b.WriteString("        mode: '0755'\n")
	logTask(b, "准备集成配置目录（保留一份可读副本）")
	b.WriteString("      ansible.builtin.file:\n")
	b.WriteString("        path: \"{{ " + v.dir + " }}\"\n")
	b.WriteString("        state: directory\n")
	b.WriteString("        mode: '0755'\n")
}

// writeFilebeatHealTasks 自愈"配置文件位置被占用成非普通文件"的历史遗留。
//
// 真实故障 INC-013（用户环境 203.195.191.75）：docker 分支先跑，宿主上没有
// /etc/filebeat/filebeat.yml，`-v <缺失路径>:/etc/filebeat/filebeat.yml:ro` 让 Docker
// 把它创建成了一个**目录**；随后 copy 报 `can not use content with a dir as dest`。
// 调整任务顺序只能防住"以后不再犯"，**用户那台机器已经被污染**，必须能自愈。
//
// when 条件 `exists and not isreg` 是安全边界：
//   - 普通文件（isreg）**绝不删** —— 否则会把用户正在用的采集配置清掉；
//   - 只有目录 / 软链（及设备文件等）这类"本来就不可能是 filebeat.yml"的东西才清。
func writeFilebeatHealTasks(b *strings.Builder, v filebeatVars) {
	logTask(b, "检查 filebeat.yml 是否被占用成目录（历史 docker 单文件挂载遗留）")
	b.WriteString("      ansible.builtin.stat:\n")
	b.WriteString("        path: \"{{ " + v.config + " }}\"\n")
	b.WriteString("      register: filebeat_config_stat\n")
	logTask(b, "清理非普通文件的 filebeat.yml（仅当它存在且不是普通文件）")
	b.WriteString("      ansible.builtin.file:\n")
	b.WriteString("        path: \"{{ " + v.config + " }}\"\n")
	// state=absent 对目录是递归删除；这里的目标一定是个空目录（Docker 刚创建的挂载点），
	// 但用递归语义更稳：万一里面被 Docker 写进了内容也不会半途报错。
	b.WriteString("        state: absent\n")
	b.WriteString("      when: " + yamlScalar("filebeat_config_stat.stat.exists and not filebeat_config_stat.stat.isreg") + "\n")
	logTask(b, "说明清理原因（供使用者与平台自检核对）")
	b.WriteString("      ansible.builtin.debug:\n")
	b.WriteString("        msg: \"检测到 {{ " + v.config + " }} 是目录（历史 docker 单文件挂载生成），已清理并重建为文件\"\n")
	b.WriteString("      when: " + yamlScalar("filebeat_config_stat.stat.exists and not filebeat_config_stat.stat.isreg") + "\n")
}

// writeFilebeatContainerRemoveTask 在写配置之前删掉平台托管的旧容器（INC-014）。
//
// 为什么这一步是**必需**的（而不是"顺手重启一下"）：
//
//	第一次失败的 docker run 已经把缺失的宿主路径创建成了目录；容器里的 Filebeat 因为
//	读不到有效配置而崩溃，`--restart=always` 让它不断重启，而 **Docker 每次启动都会把缺失的
//	绑定源重新创建成目录**。于是"自愈删目录 → copy 写文件"这条链路中间，总会再被插一个目录，
//	无论自愈跑多少次都失败。只有 `docker rm -f` 能把容器停掉，从根上断掉这个循环。
//
// 幂等与安全：
//   - `failed_when: false`：容器不存在时 docker rm 返回非 0，这是**正常结果**（首次部署就是这种），
//     不能让它中断 playbook；rc 仍然会打进日志便于核对；
//   - `when` 只在"这次真的要（重）建容器"的分支里执行（docker 模式恒为真；auto 模式是
//     "没装 filebeat 且有 docker"），package / reuse 分支不碰容器；
//   - 只删**平台自己的固定容器名**（mwops-filebeat），不会误删目标机上别人的容器。
func writeFilebeatContainerRemoveTask(b *strings.Builder, v filebeatVars, mode string) {
	if mode == LogInstallPackage {
		// package 模式完全不碰容器，产物里连这条任务都不该出现（避免误以为它会删容器）。
		return
	}
	cond := "filebeat_has_docker | bool"
	if mode == LogInstallAuto {
		cond = "not (filebeat_present | bool) and (filebeat_has_docker | bool)"
	}
	b.WriteString("    # ---- 清理旧容器：打断 Docker 反复重建\"绑定源目录\"的循环（必须在写配置之前） ----\n")
	logTask(b, "删除平台托管的旧 Filebeat 容器（存在才删，打断 Docker 重建绑定源目录的循环）")
	b.WriteString("      ansible.builtin.command: docker rm -f \"{{ " + v.container + " }}\"\n")
	b.WriteString("      register: filebeat_container_remove\n")
	// 容器不存在时 rc=1 属正常：首次部署本来就没有容器。
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: " + yamlScalar("(filebeat_container_remove.rc | default(0) | int) == 0") + "\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
}

// writeFilebeatResolveTasks 计算"选哪条安装路径"，并把探测结果固化成可读变量。
func writeFilebeatResolveTasks(b *strings.Builder, v filebeatVars) {
	b.WriteString("    # ---- 决策：把探测结果折算成布尔量，后续 when 只引用它们（可读、可审计） ----\n")
	logTask(b, "判定 filebeat 是否已安装")
	b.WriteString("      ansible.builtin.set_fact:\n")
	b.WriteString("        filebeat_present: " + yamlScalar("{{ filebeat_bin.rc == 0 }}") + "\n")
	b.WriteString("        filebeat_active: " + yamlScalar("{{ filebeat_service.stdout | default('') | trim == 'active' }}") + "\n")
	b.WriteString("        filebeat_has_docker: " + yamlScalar("{{ filebeat_docker_bin.rc == 0 }}") + "\n")
	b.WriteString("        filebeat_is_debian: " + yamlScalar("{{ filebeat_debian.stat.exists }}") + "\n")
	logTask(b, "选择实际执行的安装方式（auto → package / docker / 复用）")
	b.WriteString("      ansible.builtin.set_fact:\n")
	// 优先级：已安装（复用）→ docker → package。
	// 这个顺序对应"对现有环境影响最小"：能不动就不动，能用容器就不用装包。
	b.WriteString("        " + v.mode + ": >-\n")
	b.WriteString("          {%- if filebeat_present %}reuse\n")
	b.WriteString("          {%- elif filebeat_has_docker %}docker\n")
	b.WriteString("          {%- else %}package{% endif %}\n")
}

// writeFilebeatAutoTasks 渲染 auto 模式的三条分支。
//
// 三条分支各自带 when，互斥且覆盖全部情况：
//   - 已安装 → 什么都不做，只提示"复用"；
//   - docker → 起容器；
//   - 其余 → 包安装。
func writeFilebeatAutoTasks(b *strings.Builder, in filebeatPlaybookInput, v filebeatVars) {
	b.WriteString("    # ---- auto 分支：已安装 → 复用；否则 docker；再否则 package ----\n")
	// 顺序与决策顺序一致：先"复用"提示，再 docker，最后 package。
	// 曾经把这条提示写在两个分支之后，于是日志里会先看到一堆安装/拉镜像任务、
	// 最后才出现"检测到已安装…跳过安装"，读起来与结论相反（就是上面那个 INC 的同款问题：
	// 任务顺序必须与真实决策顺序一致，否则排障时会被日志误导）。
	logTask(b, "复用目标机已安装的 Filebeat（已存在则不部署）")
	b.WriteString("      ansible.builtin.debug:\n")
	b.WriteString("        msg: \"检测到已安装的 Filebeat（{{ filebeat_bin.stdout | default('') | trim }}），" +
		"跳过安装，仅下发配置\"\n")
	b.WriteString("      when: filebeat_present | bool\n")
	writeFilebeatDockerTasksWhen(b, in, v, "not (filebeat_present | bool) and (filebeat_has_docker | bool)")
	writeFilebeatPackageTasksWhen(b, in, v, "not (filebeat_present | bool) and not (filebeat_has_docker | bool)")
}

// writeFilebeatPackageTasks 渲染 package 安装（无条件版本，供显式 package 模式使用）。
func writeFilebeatPackageTasks(b *strings.Builder, in filebeatPlaybookInput, v filebeatVars) {
	writeFilebeatPackageTasksWhen(b, in, v, "")
}

// writeFilebeatPackageTasksWhen 渲染"未安装才安装"的包安装分支。
func writeFilebeatPackageTasksWhen(b *strings.Builder, in filebeatPlaybookInput, v filebeatVars, when string) {
	cond := "not (filebeat_present | bool)"
	if when != "" {
		cond = when
	}
	b.WriteString("    # ---- package 分支：deb/rpm + systemd（仅在未安装时执行） ----\n")
	logTask(b, "校验目标机到 artifacts.elastic.co 的出网（失败时给出离线部署提示）")
	// 只做**提示不阻断**：内网机器连不上官方制品是常见情况，但目标机可能已经装好了
	// filebeat（此时根本不需要下载），因此这一步用 debug 给出结论而不是 fail。
	b.WriteString("      ansible.builtin.shell: |\n")
	b.WriteString("        set -eu\n")
	b.WriteString("        if command -v curl >/dev/null 2>&1; then\n")
	b.WriteString("          curl -sI --max-time 10 https://artifacts.elastic.co >/dev/null && echo reachable || echo unreachable\n")
	b.WriteString("        elif command -v wget >/dev/null 2>&1; then\n")
	b.WriteString("          wget -q --spider --timeout=10 https://artifacts.elastic.co && echo reachable || echo unreachable\n")
	b.WriteString("        else\n")
	b.WriteString("          echo unknown\n")
	b.WriteString("        fi\n")
	b.WriteString("      register: filebeat_net\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
	logTask(b, "提示：目标机无法访问官方制品源时的两种离线做法")
	b.WriteString("      ansible.builtin.debug:\n")
	b.WriteString("        msg: \"目标机访问 artifacts.elastic.co：{{ filebeat_net.stdout | default('unknown') | trim }}。" +
		"若为 unreachable，请改用 docker 安装方式，或先在目标机放好 filebeat 的 deb/rpm 包（平台只下发配置）。\"\n")
	b.WriteString("      when: " + yamlScalar(cond+" and (filebeat_net.stdout | default('') | trim) != 'reachable'") + "\n")
	writeFilebeatDebTasks(b, in, cond)
	writeFilebeatRPMTasks(b, in, cond)
	writeFilebeatSystemdUnitTasks(b, v, cond)
}

// writeFilebeatDebTasks 渲染 Debian/Ubuntu 的 deb 安装（下载 + apt，仓库方式作为兜底）。
func writeFilebeatDebTasks(b *strings.Builder, in filebeatPlaybookInput, cond string) {
	b.WriteString("    # ---- Debian/Ubuntu：get_url 下载 deb + apt 安装（本机文件，不依赖 apt 源出网） ----\n")
	logTask(b, "下载 Filebeat deb 包")
	b.WriteString("      ansible.builtin.get_url:\n")
	b.WriteString("        url: " + yamlScalar(strings.ReplaceAll(filebeatDebURLTemplate, "{version}", in.Version)) + "\n")
	b.WriteString("        dest: " + yamlScalar("/tmp/filebeat-"+in.Version+"-amd64.deb") + "\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("      register: filebeat_deb\n")
	b.WriteString("      retries: 3\n")
	b.WriteString("      delay: 5\n")
	b.WriteString("      until: filebeat_deb is succeeded\n")
	b.WriteString("      when: " + yamlScalar(cond+" and (filebeat_is_debian | bool)") + "\n")
	logTask(b, "安装 Filebeat（apt 解析本地 deb 的依赖）")
	b.WriteString("      ansible.builtin.apt:\n")
	b.WriteString("        deb: " + yamlScalar("/tmp/filebeat-"+in.Version+"-amd64.deb") + "\n")
	b.WriteString("        state: present\n")
	// 兜底：官方制品拉不到时（内网/镜像站差异）退回发行版仓库。
	// 仓库里的版本通常比平台默认版本旧，但对"能装上并能采日志"而言足够。
	b.WriteString("      register: filebeat_deb_install\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond+" and (filebeat_is_debian | bool)") + "\n")
	logTask(b, "兜底：deb 安装失败时改用发行版仓库安装")
	// 官方仓库要先加 GPG key 与源，这里用 Elastic 官方的 apt 源脚本，一步到位且可重复执行。
	b.WriteString("      ansible.builtin.shell: |\n")
	b.WriteString("        set -eu\n")
	b.WriteString("        curl -fsSL https://artifacts.elastic.co/GPG-KEY-elasticsearch | gpg --dearmor -o /usr/share/keyrings/elastic-keyring.gpg\n")
	b.WriteString("        echo 'deb [signed-by=/usr/share/keyrings/elastic-keyring.gpg] https://artifacts.elastic.co/packages/8.x/apt stable main' > /etc/apt/sources.list.d/elastic-8.x.list\n")
	b.WriteString("        apt-get update -y && apt-get install -y filebeat\n")
	b.WriteString("      register: filebeat_apt_repo\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond+" and (filebeat_is_debian | bool) and (filebeat_deb_install is failed)") + "\n")
}

// writeFilebeatRPMTasks 渲染 RHEL/CentOS/Rocky 的 rpm 安装（rpm/dnf，仓库方式作为兜底）。
func writeFilebeatRPMTasks(b *strings.Builder, in filebeatPlaybookInput, cond string) {
	b.WriteString("    # ---- RHEL/CentOS/Rocky：get_url 下载 rpm + dnf 本地安装 ----\n")
	logTask(b, "下载 Filebeat rpm 包")
	b.WriteString("      ansible.builtin.get_url:\n")
	b.WriteString("        url: " + yamlScalar(strings.ReplaceAll(filebeatRPMURLTemplate, "{version}", in.Version)) + "\n")
	b.WriteString("        dest: " + yamlScalar("/tmp/filebeat-"+in.Version+"-x86_64.rpm") + "\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("      register: filebeat_rpm\n")
	b.WriteString("      retries: 3\n")
	b.WriteString("      delay: 5\n")
	b.WriteString("      until: filebeat_rpm is succeeded\n")
	b.WriteString("      when: " + yamlScalar(cond+" and not (filebeat_is_debian | bool)") + "\n")
	logTask(b, "安装 Filebeat（dnf 本地 rpm，自动解析依赖）")
	// disablerepo 与 localinstall 组合：避免目标机没有配 Elastic 源时 dnf 找不到包。
	b.WriteString("      ansible.builtin.command: dnf install -y " + yamlScalar("/tmp/filebeat-"+in.Version+"-x86_64.rpm") + "\n")
	b.WriteString("      register: filebeat_rpm_install\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond+" and not (filebeat_is_debian | bool)") + "\n")
	logTask(b, "兜底：rpm 安装失败时改用 rpm 直接安装")
	b.WriteString("      ansible.builtin.command: rpm -Uvh --replacepkgs " + yamlScalar("/tmp/filebeat-"+in.Version+"-x86_64.rpm") + "\n")
	b.WriteString("      register: filebeat_rpm_fallback\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond+" and not (filebeat_is_debian | bool) and (filebeat_rpm_install is failed)") + "\n")
	logTask(b, "确认安装结果（command -v filebeat）")
	b.WriteString("      ansible.builtin.shell: command -v filebeat\n")
	b.WriteString("      register: filebeat_install_check\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
	logTask(b, "安装结果兜底校验（三条路径都失败时明确报错）")
	b.WriteString("      ansible.builtin.assert:\n")
	b.WriteString("        that:\n")
	// 判据是"安装之后 command -v filebeat 能找到它"，而不是"某个任务 changed 了"：
	// 已经装好、只是没启用的机器上三个安装任务都会跳过，用 changed 判定会把成功判成失败。
	b.WriteString("        - filebeat_install_check.rc == 0\n")
	b.WriteString("        fail_msg: \"Filebeat 安装失败：目标机既拉不到官方制品，发行版仓库安装也没成功。" +
		"请检查目标机出网策略，或改用 docker 安装方式。\"\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
}

// writeFilebeatSystemdUnitTasks 启用并启动官方 systemd 单元。
func writeFilebeatSystemdUnitTasks(b *strings.Builder, v filebeatVars, cond string) {
	logTask(b, "启用并启动 Filebeat systemd 服务（未安装时单元由官方包提供）")
	b.WriteString("      ansible.builtin.systemd:\n")
	b.WriteString("        name: \"{{ " + v.unit + " }}\"\n")
	b.WriteString("        enabled: true\n")
	// 这里用 started 而不是 restarted：配置下发任务会通过 handler 精确触发重启
	//（"配置未变则不重启"），安装任务只负责"确保它在跑"。
	b.WriteString("        state: started\n")
	b.WriteString("        daemon_reload: true\n")
	b.WriteString("      register: filebeat_unit_state\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
}

// writeFilebeatDockerTasks 渲染 docker 安装（无条件版本，供显式 docker 模式使用）。
func writeFilebeatDockerTasks(b *strings.Builder, in filebeatPlaybookInput, v filebeatVars) {
	writeFilebeatDockerTasksWhen(b, in, v, "")
}

// writeFilebeatDockerTasksWhen 渲染"没有已安装的 filebeat 时用容器"的分支。
func writeFilebeatDockerTasksWhen(b *strings.Builder, in filebeatPlaybookInput, v filebeatVars, when string) {
	cond := "filebeat_has_docker | bool"
	if when != "" {
		cond = when
	}
	b.WriteString("    # ---- docker 分支：官方镜像 + --restart=always（挂载配置与日志目录） ----\n")
	logTask(b, "拉取 Filebeat 官方镜像")
	b.WriteString("      ansible.builtin.command: docker pull \"{{ " + v.image + " }}\"\n")
	b.WriteString("      register: filebeat_pull\n")
	b.WriteString("      retries: 3\n")
	b.WriteString("      delay: 10\n")
	b.WriteString("      until: filebeat_pull is succeeded\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
	logTask(b, "创建 Filebeat 数据目录（注册表/位点，容器重建后断点续传不失效）")
	b.WriteString("      ansible.builtin.file:\n")
	b.WriteString("        path: \"{{ filebeat_data_dir }}\"\n")
	b.WriteString("        state: directory\n")
	// 权限与属主是**刻意**的（线上真实故障 INC-015）：
	//
	//	playbook 以 become: true 执行，若用默认的 0755 root:root，官方 filebeat 镜像默认以
	//	filebeat 用户（uid/gid 1000）运行，启动时会报
	//	  Exiting: failed to create Beat meta file: open /usr/share/filebeat/data/meta.json.new: permission denied
	//	即容器能起来但立刻退出 —— 采集整体哑掉。
	//
	//	下面两件事都做，是刻意的纵深防御：
	//	  ① docker run 带 --user=root（官方示例如此，保证能读宿主日志、能写数据目录）；
	//	  ② 数据目录做成 0775 且显式 owner/group=1000。
	//	将来有人去掉 --user=root、或换成自己 build 的镜像（以非 root 运行）时，
	//	②仍然让容器写得进数据目录，不会因为一个目录属主把整条采集链路弄哑。
	//
	// 目录本身是平台专属的 {{ filebeat_data_dir }}（/var/lib/mwops-filebeat），
	// 与系统 filebeat 的 /var/lib/filebeat 分开：既不改动系统采集的属主，也不与它共用注册表。
	// 因此这条任务**只出现在 docker 分支**——package 分支的目录由系统包自己创建，平台不碰。
	b.WriteString("        mode: '0775'\n")
	b.WriteString("        owner: '1000'\n")
	b.WriteString("        group: '1000'\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
	writeFilebeatContainerTasks(b, in, v, cond)
}

// writeFilebeatContainerTasks 渲染容器（重建）任务。
//
// 刻意把"重建"与"配置下发"分开：
//   - 配置下发（copy）负责"内容变了"这一事实并通知 handler；
//   - 这里的重建负责"容器不存在/镜像变了"，用 `docker inspect` 的镜像比对做判定，
//     因此重复执行不会无故重启（幂等）。
func writeFilebeatContainerTasks(b *strings.Builder, in filebeatPlaybookInput, v filebeatVars, cond string) {
	logTask(b, "探测 Filebeat 容器是否已存在")
	// 只用 inspect 的退出码判定"容器在不在"（rc != 0 表示不存在，这是正常结果而不是错误）。
	// 刻意**不**取容器里的镜像字段做字符串比对：那需要 `--format '{{.Config.Image}}'`，
	// 而 playbook 里每个值都会先被 Ansible 当 Jinja 模板渲染，行首的 `{{.` 会直接报
	// template error while templating string: unexpected '.'（INC-006 的同类坑）。
	// 镜像/配置变了怎么办：配置变化由 copy 的 checksum + handler 负责重启，见 writeFilebeatConfigTask。
	b.WriteString("      ansible.builtin.command: docker inspect \"{{ " + v.container + " }}\"\n")
	b.WriteString("      register: filebeat_container_inspect\n")
	// 容器不存在时 inspect rc=1：这是"正常结果"（那就是要创建它），不能让它中断 playbook。
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
	// 起容器之前的最后一道闸：配置文件必须是**普通文件**。
	// 只要它不是普通文件（最典型的是上一次 docker 单文件挂载留下的目录），Docker 会再把它
	// 当成"缺失路径"创建成目录，把问题滚雪球；这里明确失败并说清怎么修（宁可报错也不要制造目录）。
	logTask(b, "启动容器前确认 filebeat.yml 已是普通文件（防止 Docker 再制造目录）")
	b.WriteString("      ansible.builtin.assert:\n")
	b.WriteString("        that:\n")
	b.WriteString("          - filebeat_config_stat.stat.exists\n")
	b.WriteString("          - filebeat_config_stat.stat.isreg\n")
	b.WriteString("        fail_msg: \"{{ " + v.config + " }} 不是普通文件（可能是目录或软链），" +
		"拒绝启动容器：Docker 会把缺失的宿主路径创建成目录，容器拿到的就不是配置文件。" +
		"请先手工删除该路径（rm -rf {{ " + v.config + " }}）后重试。\"\n")
	b.WriteString("      when: " + yamlScalar(cond) + "\n")
	logTask(b, "创建 Filebeat 容器（已存在则跳过，避免无故重启采集）")
	b.WriteString("      ansible.builtin.shell: |\n")
	b.WriteString("        set -eu\n")
	b.WriteString("        " + dockerRunLineForFilebeat(in, v) + "\n")
	b.WriteString("      register: filebeat_container_run\n")
	b.WriteString("      when: " + yamlScalar(cond+" and (filebeat_container_inspect.rc | default(1) | int) != 0") + "\n")
}

// configParentDir 返回配置文件所在目录（用于"确保父目录存在"那条任务）。
//
// 由 ConfigPath 推导而不是另写常量：父目录与 dest 必须同源，否则又会分叉出
// "建了一个目录、往另一个目录写"的老问题（INC-013）。
func configParentDir(configPath string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(configPath), "/")
	idx := strings.LastIndex(trimmed, "/")
	switch {
	case idx > 0:
		return trimmed[:idx]
	case idx == 0:
		// /filebeat.yml → 父目录就是根。
		return "/"
	default:
		// 相对路径（理论上不会出现）：原样返回，交给 ansible 的 file 模块报错更清楚。
		return trimmed
	}
}

// dockerRunLineForFilebeat 生成 docker run 命令（含配置与日志目录挂载）。
//
// 三条与官方镜像对齐的硬性要求（INC-014）：
//   - 配置挂到 `{{ filebeat_container_config_path }}`（= /usr/share/filebeat/filebeat.yml）。
//     官方镜像的默认配置路径就在这里，挂到 /etc/filebeat/ 容器**根本不会读**，
//     会拿镜像内置配置去连 Elasticsearch → 崩溃 → 触发 Docker 反复重建绑定源目录；
//   - `--user=root`：容器要读宿主 /var/log 下的日志文件，非 root 通常没有权限
//     （官方示例同样用 root 起容器）；
//   - `filebeat -e --strict.perms=false`：`-e` 让日志打到 stderr（自检与 docker logs 能看到）；
//     `--strict.perms=false` 是因为挂载进来的配置文件属主/权限必然不满足 Filebeat 的
//     "配置文件不能 group/world 可写"检查，不加它容器会以
//     `Exiting: error loading config file: config file must be owned by the beat user` 之类的原因退出。
//
// --restart=always 是刻意的（而不是 unless-stopped）：日志采集是"基础设施"，
// 目标机重启后必须自己回来，不能因为一次手工 stop 就永久停采（那会让平台安静地收不到日志）。
func dockerRunLineForFilebeat(in filebeatPlaybookInput, v filebeatVars) string {
	var b strings.Builder
	b.WriteString("docker run -d --name \"{{ " + v.container + " }}\" --restart=always")
	b.WriteString(" --user=root")
	// 配置只读挂载：容器不需要改配置，ro 能防止容器内的误写"骗过"下一次 checksum 比对。
	b.WriteString(" \\\n          -v \"{{ " + v.config + " }}:{{ " + v.containerConfig + " }}:ro\"")
	b.WriteString(" \\\n          -v \"{{ filebeat_data_dir }}:" + filebeatContainerDataDir + "\"")
	// 日志目录挂载：容器只能看见挂载进来的路径，少挂一个 = 那批日志永远采不到。
	for _, dir := range in.LogMounts {
		b.WriteString(" \\\n          -v " + shellArg(dir+":"+dir+":ro"))
	}
	// 镜像之后才是容器要执行的命令：`filebeat -e --strict.perms=false`。
	b.WriteString(" \\\n          \"{{ " + v.image + " }}\"")
	b.WriteString(" \\\n          filebeat -e --strict.perms=false")
	return b.String()
}

// writeFilebeatConfigTask 下发 filebeat.yml。
//
// **幂等的核心**：ansible.builtin.copy 默认按 checksum 比对目标文件内容，
// 内容没变时任务报 ok（不是 changed），也就**不会触发 notify 的 handler**。
// 于是"重复点击集成 / 重复重放 playbook"不会重启 Filebeat，采集不抖动；
// 只有配置真正变化时才 restart（见 handlers）。
//
// 内容走 vars 文件（-e @vars.yml）而不是内联在 playbook 里：playbook 的每个值都会先被
// Ansible 当 Jinja 模板渲染，日志路径里出现 {% 或 {{ 时会被当成模板表达式而报错
// （INC-005/INC-006 的同类坑）；放进变量文件则原样落盘，`content: "{{ … }}"` 只是取值。
func writeFilebeatConfigTask(b *strings.Builder, in filebeatPlaybookInput) {
	v := filebeatCardinality()
	logTask(b, "下发 filebeat.yml（内容不变时不重启采集）")
	// 模块参数直接写在 ansible.builtin.copy 下（**不要**用 args:）：
	// 一旦写成 "ansible.builtin.copy: null" + 平级的 args:，YAML 会把 copy 解析成空任务、
	// args 变成另一个模块名，playbook 在第 165 行附近直接报 "did not find expected '-' indicator"。
	b.WriteString("      ansible.builtin.copy:\n")
	// 两个落点：
	//   - filebeat_config_path（/etc/filebeat/filebeat.yml）：package 模式下官方 unit 读的就是它；
	//   - filebeat_mirror_path（配置目录里的副本）：容器模式挂载它，同时便于排障时对比"平台下发的是什么"。
	b.WriteString("        dest: \"{{ " + v.config + " }}\"\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("        content: \"{{ filebeat_config_content }}\"\n")
	b.WriteString("      notify: 重启 Filebeat\n")
	logTask(b, "在配置目录保留一份副本（便于对比平台下发内容与手工改动）")
	b.WriteString("      ansible.builtin.copy:\n")
	b.WriteString("        dest: \"{{ " + v.mirror + " }}\"\n")
	b.WriteString("        mode: '0644'\n")
	b.WriteString("        content: \"{{ filebeat_config_content }}\"\n")
}

// writeFilebeatKafkaProbeTask 在目标机上做一次「目标机 → 平台 Kafka」的 TCP 连通性探测。
//
// 这是"Kafka advertised 地址配错"**唯一能在现场拿到证据**的地方：
// 平台侧连 Kafka 一切正常，目标机上却是
//
//	dial tcp 127.0.0.1:9092: connect: connection refused
//
// 因为 broker 握手后把客户端引导去了配置错误的 advertised 地址（见 docs/LOG_INTEGRATION.md §三）。
// 探测目标就是 filebeat.yml 里写的第一个 Kafka 地址 —— 与真实采集用的是同一个值，
// 因此结论可以直接回答"到底是网络不通还是地址配错"。
//
// **不让 playbook 失败**（failed_when: false）：采集不一定立刻可用（平台可能正在重启），
// 把安装整体判失败会误导使用者；但结论要能被平台自检与部署备注读到，所以走 debug 输出。
func writeFilebeatKafkaProbeTask(b *strings.Builder, in filebeatPlaybookInput) {
	host, port := splitHostPort(in.KafkaProbe)
	logTask(b, "探测目标机到平台 Kafka 的 TCP 连通性（advertised 地址配错的现场证据）")
	// 用 bash /dev/tcp 与 nc 两种手段：目标机上不一定装了 nc，而 /dev/tcp 是 bash 内建，
	// 覆盖面最广。刻意用自定义的 Jinja 注释标记（{# #}）而不是 shell 的 #：
	// 后者会被 Jinja 当注释吃掉，分界的 echo 就再也打不出来了。
	b.WriteString("      ansible.builtin.shell: |\n")
	b.WriteString("        set -u\n")
	b.WriteString("        if timeout 5 bash -c 'echo > /dev/tcp/" + host + "/" + port + "' 2>/dev/null; then\n")
	b.WriteString("          echo '{#MWOPS#}reachable'\n")
	b.WriteString("        elif command -v nc >/dev/null 2>&1 && nc -z -w 5 " + host + " " + port + " >/dev/null 2>&1; then\n")
	b.WriteString("          echo '{#MWOPS#}reachable'\n")
	b.WriteString("        else\n")
	b.WriteString("          echo '{#MWOPS#}unreachable'\n")
	b.WriteString("        fi\n")
	b.WriteString("      register: filebeat_kafka_probe\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	logTask(b, "输出 Kafka 连通性结论（不通时给出可照抄的排查命令）")
	b.WriteString("      ansible.builtin.debug:\n")
	b.WriteString("        msg: >-\n")
	// 解析探测结论：regex_search 返回捕获组组成的列表，用 `| first` 取标量
	// （列表直接插进字符串会渲染成 ['reachable'] 这种带括号引号的形态，不适合给人看）。
	b.WriteString("          Kafka " + host + ":" + port + " 连通性：{{ filebeat_kafka_probe.stdout | default('') " +
		"| regex_search('MWOPS#}(reachable|unreachable)', '\\\\1') | first | default('unknown') }}。" +
		"若为 unreachable，请在目标机执行 nc -vz " + host + " " + port + " 复核，" +
		"并检查平台 .env 的 KAFKA_ADVERTISED_HOST 是否为被管机可达的地址（写 localhost/127.0.0.1 或容器名都会导致" +
		"Filebeat 连上后立刻断开）。\n")
	b.WriteString("      when: filebeat_kafka_probe is defined\n")
}

// firstKafkaHost 取探测用的 Kafka 地址（渲染器已保证 KafkaHosts 非空）。
func firstKafkaHost(hosts []string) string {
	normalized := normalizeStringList(hosts)
	if len(normalized) == 0 {
		return ""
	}
	return normalized[0]
}

// splitHostPort 把 "10.0.0.5:9092" 拆成 host / port（取不到端口时按 Kafka 默认 9092）。
func splitHostPort(address string) (string, string) {
	trimmed := strings.TrimSpace(address)
	if trimmed == "" {
		return "", ""
	}
	// IPv6 形如 [::1]:9092：只在最后一个冒号处拆，且方括号要去掉。
	if idx := strings.LastIndex(trimmed, ":"); idx > 0 {
		host := strings.Trim(trimmed[:idx], "[]")
		port := strings.TrimSpace(trimmed[idx+1:])
		if port != "" {
			return host, port
		}
		return host, "9092"
	}
	return trimmed, "9092"
}

// writeFilebeatVerifyTasks 末尾的最佳努力校验。
//
// 两条命令都**不让 playbook 失败**：安装本身已经成功，自检失败是"信息"而不是"错误"——
// 目标机可能暂时连不上 Kafka（平台在重启），此时把安装整体判失败会误导使用者。
// 但输出必须打到日志里（msg 而不是 censored），平台自检与使用者的排障都能直接取用。
func writeFilebeatVerifyTasks(b *strings.Builder, in filebeatPlaybookInput) {
	logTask(b, "校验 filebeat.yml 配置（失败不阻断，输出打到日志便于自检取用）")
	b.WriteString("      ansible.builtin.shell: \"{{ filebeat_verify_content }}\"\n")
	b.WriteString("      register: filebeat_verify\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      changed_when: false\n")
	logTask(b, "输出自检结论（配置合法性 + 到平台 Kafka 的连通性）")
	b.WriteString("      ansible.builtin.debug:\n")
	b.WriteString("        msg: \"{{ filebeat_verify.stdout_lines | default([]) + filebeat_verify.stderr_lines | default([]) }}\"\n")
}

// logTask 输出一个任务头（统一缩进，避免各渲染函数各写各的）。
func logTask(b *strings.Builder, name string) {
	b.WriteString("    - name: " + name + "\n")
}

// renderFilebeatVars 渲染 `-e @vars.yml` 变量文件。
//
// 两部分：
//   - filebeat_config_content：渲染好的 filebeat.yml（块标量，多行原样）；
//   - filebeat_verify_content ：末尾自检命令（shell 片段，块标量，展示版为说明文字）。
//
// 该文件与 Exporter 的 vars 文件一样落盘 0600、用完即删：它不是凭据，
// 但含目标机日志路径与平台内网地址，同样不该被复制到工单/聊天里。
func renderFilebeatVars(config, mode string, masked bool) string {
	var b strings.Builder
	writeBlockScalar(&b, "filebeat_config_content", config)
	writeBlockScalar(&b, "filebeat_verify_content", filebeatVerifyCommands(mode, masked))
	return b.String()
}

// writeBlockScalar 把多行文本写成 YAML 块标量（|）。
//
// 必须用块标量：单引号包住多行文本时 YAML 会把换行折叠成空格，
// filebeat.yml 会挤成一行 —— 落到目标机上就是一份语法错误的配置。
func writeBlockScalar(b *strings.Builder, key, content string) {
	b.WriteString(key + ": |\n")
	for _, line := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString("  " + line + "\n")
	}
}

// ensureFilebeatVarsParsable 确认变量文件本身是合法 YAML。
//
// 单独校验它而不是搭 playbook 的便车：vars 文件由 `-e @file` 读取，不走
// ansible-playbook 的 YAML 解析路径；一份坏掉的 vars 会让 ansible 报
// "could not read vars file"，同样值得在渲染阶段拦下（错误信息更贴近根因）。
func ensureFilebeatVarsParsable(label, vars string) error {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(vars), &doc); err != nil {
		return fmt.Errorf("平台生成的 %s 变量文件不是合法 YAML（这属于平台模板缺陷，请提交工单）：%s",
			label, firstLine(err.Error()))
	}
	return nil
}
