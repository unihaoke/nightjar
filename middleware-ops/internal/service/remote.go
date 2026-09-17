package service

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
)

// 本文件实现「远程服务器一键安装 Exporter」。
//
// 与「本机（Docker API）」路径的区别：
//   - 本机：平台在自己的宿主上创建容器，Exporter 与 Prometheus 同网络，抓取目标是容器名:端口；
//   - 远程：平台渲染内置 Ansible playbook，在**目标服务器**上安装并启动 Exporter，
//     抓取目标是 目标IP:端口（远程没有容器名可解析）。
//
// 安全约定（公共平台，与既有凭据策略一致）：
//   - SSH 凭据只在本次请求内存中流转，写进 0600 的临时 inventory / vars 文件，执行完立即删除；
//   - 绝不落库、绝不写审计、绝不回显；Ansible 输出回传前再做一次口令擦除；
//   - 生产环境（env=prod）只创建审批工单，不直接执行。

// RemoteCreds 是一次远程安装所需的 SSH 凭据（仅内存传递）。
type RemoteCreds struct {
	Host         string
	User         string
	Port         int
	Password     string
	Key          string
	Become       *bool
	InstallMode  string
	ExporterPort int
}

// provided 判断本次是否带了可用的 SSH 凭据。
func (c RemoteCreds) provided() bool {
	return strings.TrimSpace(c.User) != "" && (c.Password != "" || strings.TrimSpace(c.Key) != "")
}

// remoteReady 判断平台是否允许远程安装。
func (s *IntegrationService) remoteReady() error {
	if !s.cfg.Integration.AllowRemoteInstall || !s.cfg.Integration.Ansible.Enabled {
		return fmt.Errorf("远程安装未启用：需要 integration.allow_remote_install=true 且 integration.ansible.enabled=true，" +
			"并确保平台镜像内已安装 ansible-playbook")
	}
	if strings.TrimSpace(s.cfg.Integration.Ansible.Binary) == "" {
		return fmt.Errorf("未配置 ansible-playbook 路径（integration.ansible.binary）")
	}
	if _, err := exec.LookPath(s.cfg.Integration.Ansible.Binary); err != nil {
		return fmt.Errorf("平台镜像内找不到 %s：请用带 Ansible 的镜像（见 deploy/ansible/README.md）",
			s.cfg.Integration.Ansible.Binary)
	}
	return nil
}

// lookPath 是 exec.LookPath 的替身点（便于测试"平台缺 sshpass"这条分支）。
var lookPath = exec.LookPath

// checkSSHPass 校验平台侧具备"用口令 SSH 登录"的能力。
//
// 为什么需要这个前置检查：ansible 的 ssh 连接插件本身不实现认证，它把口令交给
// OpenSSH，而 OpenSSH 不接受命令行口令——必须由 sshpass 代答。镜像里缺 sshpass 时
// ansible 只在执行阶段抛一句
//
//	to use the 'ssh' connection type with passwords or pkcs11_provider,
//	you must install the sshpass program
//
// 这句话既没提"平台"，也没告诉使用者该怎么办（真实故障 INC-007）。
// 私钥认证不经过 sshpass，因此只在口径令认证时检查。
func (s *IntegrationService) checkSSHPass(creds RemoteCreds) error {
	if strings.TrimSpace(creds.Password) == "" {
		return nil
	}
	if _, err := lookPath("sshpass"); err != nil {
		return fmt.Errorf("平台容器内缺少 sshpass，无法用「SSH 口令」登录目标机。两种解法：" +
			"① 重建平台镜像（WITH_ANSIBLE=true 会一并安装 sshpass 与 openssh-client）：" +
			"docker compose build backend && docker compose up -d backend；" +
			"② 或改用「SSH 私钥」认证——私钥路径不依赖 sshpass，当前镜像即可执行")
	}
	return nil
}

// SSHPassAvailable 报告平台容器内是否具备 sshpass。
//
// /healthz 与 deploy/ansible/tools/check-backend-freshness.sh 用它判断镜像能力：
// 口令方式远程安装要求平台侧有 sshpass，而这与"镜像新旧"无关（同一版渲染器
// 可能来自装了 sshpass 的新镜像，也可能来自没装它的旧镜像）。
func SSHPassAvailable() bool {
	_, err := lookPath("sshpass")
	return err == nil
}

// deployRemote 渲染并执行远程安装。
//
// 返回 nil 表示安装命令成功且端口可达；否则返回可读原因（会被写进集成备注与待处理）。
func (s *IntegrationService) deployRemote(
	ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template,
	instance integration.Instance, meta IntegrationMeta, creds RemoteCreds, operator Operator,
) error {
	if err := s.remoteReady(); err != nil {
		return err
	}
	host := strings.TrimSpace(creds.Host)
	if host == "" {
		host = strings.TrimSpace(meta.TargetHost)
	}
	if host == "" {
		return fmt.Errorf("远程安装需要填写目标服务器地址（集群维度）")
	}

	// 生产环境：在别的机器上装东西属于 L2 → 只开工单，不执行。
	if instance.Environment == model.EnvProd && s.approval != nil {
		ticket, ticketErr := s.approval.Create(ctx, ApprovalRequest{
			InstanceID: item.ID, Environment: instance.Environment,
			ActionType: "integration_remote_install",
			ActionDetail: map[string]any{
				"name": item.Name, "mw_type": tpl.Type, "target_host": host,
				"exporter_port": meta.ExporterHostPort, "install_mode": s.installMode(creds),
			},
			Reason: "生产环境由平台在远程服务器安装 Exporter（L2）",
		}, operator)
		if ticketErr != nil {
			return fmt.Errorf("创建审批工单失败：%w", ticketErr)
		}
		return fmt.Errorf("生产环境需审批：已创建工单 %s；审批通过后请点「重新应用」（届时需重新填写 SSH 凭据）", ticket.TicketID)
	}

	if !creds.provided() {
		return fmt.Errorf("远程安装需要 SSH 凭据（用户名 + 口令或私钥）：编辑该集成并填写后保存。" +
			"凭据只在本次请求中使用、不落库，因此「重新应用」无法复用上一次的凭据")
	}
	if err := s.checkSSHPass(creds); err != nil {
		return err
	}

	// 渲染产物
	become := s.cfg.Integration.Ansible.Become
	if creds.Become != nil {
		become = *creds.Become
	}
	port := meta.ExporterHostPort
	if port <= 0 {
		port = creds.ExporterPort
	}
	opts := integration.RemoteOptions{
		Host: host, SSHUser: creds.User, SSHPort: creds.Port,
		ExporterPort: port, InstallMode: s.installMode(creds),
		InstallDir: s.cfg.Integration.Ansible.InstallDir,
		DockerNetwork: s.cfg.Integration.Ansible.DockerNetwork,
		Become:        become, ExtraArgs: s.cfg.Integration.Ansible.ExtraArgs,
	}
	if creds.Password == "" && strings.TrimSpace(creds.Key) != "" {
		path, writeErr := s.writeSecret(item.Name+".key", creds.Key)
		if writeErr != nil {
			return fmt.Errorf("写入临时私钥失败：%w", writeErr)
		}
		defer func() { _ = os.Remove(path) }()
		opts.SSHKeyFile = path
	}
	opts.SSHPassword = creds.Password
	art, err := integration.RenderRemoteInstall(tpl, instance, opts)
	if err != nil {
		return err
	}

	// 落盘：playbook 供审计与人工复核；inventory / vars 含凭据 → 0600 且用完即删。
	artifactDir := filepath.Join(s.cfg.Integration.OutputDir, "ansible")
	if mkErr := os.MkdirAll(artifactDir, 0o750); mkErr != nil {
		return fmt.Errorf("创建产物目录失败：%w", mkErr)
	}
	playbookPath := filepath.Join(artifactDir, item.Name+".yml")
	if writeErr := os.WriteFile(playbookPath, []byte(art.Playbook), 0o644); writeErr != nil {
		return fmt.Errorf("写入 playbook 失败：%w", writeErr)
	}
	inventoryPath, err := s.writeSecret(item.Name+".ini", art.Inventory)
	if err != nil {
		return fmt.Errorf("写入临时 inventory 失败：%w", err)
	}
	defer func() { _ = os.Remove(inventoryPath) }()
	varsPath, err := s.writeSecret(item.Name+".vars.yml", art.VarsFile)
	if err != nil {
		return fmt.Errorf("写入临时变量文件失败：%w", err)
	}
	defer func() { _ = os.Remove(varsPath) }()

	// 执行
	args := []string{"-i", inventoryPath, playbookPath, "-e", "@" + varsPath}
	if become {
		args = append(args, "--become")
	}
	if s.cfg.Integration.Ansible.CheckMode {
		args = append(args, "--check")
	}
	args = append(args, s.cfg.Integration.Ansible.ExtraArgs...)
	timeout := s.cfg.Integration.Ansible.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.cfg.Integration.Ansible.Binary, args...)
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False", "ANSIBLE_NOCOLOR=1")
	output, runErr := cmd.CombinedOutput()
	safe := redactSecrets(string(output), creds.Password, creds.Key)
	if runErr != nil {
		// 完整（已脱敏）输出进平台日志：界面里的摘要只够定位，深挖要看原始输出。
		s.log.Error("集成：远程安装失败",
			zap.String("integration", item.Name), zap.String("host", host),
			zap.String("output", truncateText(safe, 8000)))
		return fmt.Errorf("Ansible 执行失败：%w\n%s%s", runErr, ansibleFailureExcerpt(safe, 900), censoredHint(safe))
	}

	// 安装完不等于可用：从平台侧探一次端口，把结论写回来。
	if probeErr := probeHostPort(runCtx, host, port); probeErr != nil {
		return fmt.Errorf("Ansible 执行成功，但平台探测 %s 失败：%v（请检查目标机防火墙与 Exporter 监听地址；输出：%s）",
			integration.JoinHostPort(host, port), probeErr, truncateText(safe, 300))
	}
	s.log.Info("集成：远程 Exporter 安装完成",
		zap.String("integration", item.Name), zap.String("host", host), zap.Int("port", port))
	return nil
}

// censoredHint 在 ansible 输出被 no_log 整体屏蔽时补一句排查指引。
//
// 含密任务必须 no_log（否则口令会随 module args 回显），代价是失败结果被整段替换成
// censored：真实原因（目标机目录不存在、磁盘满、权限不足）在使用者眼里全没了（INC-008）。
// 平台已把可失败的前置步骤拆成不含密的独立任务（目录创建、docker 可用性），
// 这里再给一句"下一步该看什么"，避免使用者只能看到一行 censored 干瞪眼。
func censoredHint(output string) string {
	if !strings.Contains(output, "censored") {
		return ""
	}
	return "（该任务带 no_log，输出被整体隐藏以避免回显口令。平台已把安装目录创建、docker 可用性等" +
		"前置步骤拆成不含密的独立任务，它们的报错是可见的；若仍卡在写入/启动步骤，请在目标机上执行：" +
		"ls -ld /opt/mwops-exporter && df -h /opt && journalctl -u 'mwops-exporter-*' -n 50 --no-pager，" +
		"或用 docker logs <容器名> 看 Exporter 自身日志）"
}

// ansibleFailureExcerpt 从 ansible 输出里挑出**失败相关**的部分。
//
// 为什么不能只截前 N 个字符（真实故障 INC-009）：ansible 是"从前往后"打印的，
// 失败一定在**尾部**；按 head 截断恰好把唯一有用的那段砍掉——使用者看到的是一串
// 成功任务的 ok/changed，真正的原因一个字都没有。
//
// 这里的做法：定位第一条失败标记（fatal/unreachable/FAILED!/ERROR!），
// 回溯它所属的 TASK 行，再连同紧跟其后的若干行（msg 常是多行）一起摘出来；
// 找不到失败标记时退回"头 + 尾"摘要，保证任何情况下都有信息量。
func ansibleFailureExcerpt(output string, limit int) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	markers := []string{"fatal:", "unreachable:", "FAILED!", "ERROR!", "failed="}
	failAt := -1
	for i, line := range lines {
		for _, marker := range markers {
			if strings.Contains(line, marker) {
				failAt = i
				break
			}
		}
		if failAt >= 0 {
			break
		}
	}
	if failAt < 0 {
		// 没有失败标记（例如平台侧超时被杀）：头尾都给，避免只看开头。
		head := truncateText(output, limit/2)
		if len(strings.TrimSpace(output)) <= limit {
			return strings.TrimSpace(output)
		}
		tail := strings.TrimSpace(output)
		if len(tail) > limit/2 {
			tail = "…" + tail[len(tail)-limit/2:]
		}
		return head + "\n…（中略）…\n" + tail
	}

	// 回溯最近的 TASK 行（含 RETRYING 行，便于看出重试了几次）。
	from := failAt
	for i := failAt; i >= 0 && failAt-i < 40; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "TASK [") || strings.HasPrefix(trimmed, "PLAY [") {
			from = i
			break
		}
	}
	// 失败行之后继续收：msg 可能是多行，但遇到下一个 TASK / PLAY RECAP 就停。
	to := failAt + 1
	for to < len(lines) && to-failAt < 12 {
		trimmed := strings.TrimSpace(lines[to])
		if strings.HasPrefix(trimmed, "TASK [") || strings.HasPrefix(trimmed, "PLAY RECAP") {
			break
		}
		to++
	}
	excerpt := strings.Join(lines[from:to], "\n")
	if from > 0 {
		excerpt = "…（前面 " + strconv.Itoa(from) + " 行成功的任务已省略）\n" + excerpt
	}
	return truncateText(excerpt, limit)
}

// CodeRevision 是**平台代码**的运行期修订号：只增不减。
//
// 与 PlaybookRendererVersion 的分工：后者只跟踪 playbook 模板（写进产物第 3 行），
// 前者跟踪平台自身行为（远程执行、错误呈现、诊断等）。任何"修好了、但需要确认
// 镜像里到底有没有这一版"的改动都要在此 +1，并在注释里留一行说明。
//
// 排查首问：`curl -s http://<平台>:8080/healthz` 里的 code_revision 是否等于代码里的常量。
// 历史：
//
//	r1：远程安装失败摘要（不再只截前 600 字符，改为摘出失败任务与原因）+ 完整输出进平台日志。
//	r2：Exporter 端口与实例端口同机冲突时自动改用模板默认端口（INC-010）。
//	r3：重新应用接受 SSH 凭据（口令/私钥）、发起新尝试时清掉上次失败、待处理项只推荐一个动作（INC-011）。
//	r4：Redis 的 REDIS_ADDR 改为不带 scheme 的 host:port（INC-012）。
//	r5：Exporter 侧地址按「目标机视角」渲染（同机改用回环）；远程回环地址不再做平台侧探测（INC-013）。
//	r6：新增「重新核验」接口与周期自愈；核验返回"是否已判定"，Prometheus 抖动不再误改状态（INC-015）。
//	r7：新增「集成自检」：平台端口 → Exporter 在位 → Prometheus 抓取 → 业务指标，分环节给结论与动作。
const CodeRevision = "r7"

// writeSecret 把含凭据的内容写到 0600 的临时文件，返回路径。
func (s *IntegrationService) writeSecret(name, content string) (string, error) {
	dir := s.cfg.Integration.Ansible.InventoryDir
	if strings.TrimSpace(dir) == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// accountSQLRemote 在**目标主机**上执行账号 SQL（建号 / 轮换 / 删除）。
//
// 与"平台侧起一次性容器"的区别：这条路径只用到 ansible-playbook + SSH，
// 因此远程集成可以在平台完全没有 docker.sock 的情况下完成建号。
// SQL 语句仍来自平台内置模板（不接受使用者传入任意语句）。
func (s *IntegrationService) accountSQLRemote(
	ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template,
	instance integration.Instance, meta IntegrationMeta, execUser, execPassword string,
	statements []string, creds RemoteCreds,
) (string, error) {
	if err := s.remoteReady(); err != nil {
		return "", err
	}
	if !creds.provided() {
		return "", fmt.Errorf("远程建号/改号需要在目标机上执行 SQL，但本次没有 SSH 凭据：" +
			"请在集成表单里填写 SSH 用户名与口令后重新保存（凭据不落库，因此「重新应用」无法复用）")
	}
	if err := s.checkSSHPass(creds); err != nil {
		return "", err
	}
	host := strings.TrimSpace(creds.Host)
	if host == "" {
		host = strings.TrimSpace(meta.TargetHost)
	}
	if host == "" {
		return "", fmt.Errorf("远程建号/改号需要目标服务器地址")
	}

	art, err := integration.RenderAccountSQL(integration.AccountSQLRequest{
		Name: item.Name, MWType: tpl.Type,
		DBHost: instance.Address.Host, DBPort: instance.Address.Port,
		ExecUser: execUser, ExecPassword: execPassword,
		Statements: statements,
	})
	if err != nil {
		return "", err
	}

	// 产物落盘：playbook 供审计（不含密）；inventory 与 vars 含凭据 → 0600 且用完即删。
	artifactDir := filepath.Join(s.cfg.Integration.OutputDir, "ansible")
	if mkErr := os.MkdirAll(artifactDir, 0o750); mkErr != nil {
		return "", fmt.Errorf("创建产物目录失败：%w", mkErr)
	}
	playbookPath := filepath.Join(artifactDir, item.Name+"-account.yml")
	if writeErr := os.WriteFile(playbookPath, []byte(art.Playbook), 0o644); writeErr != nil {
		return "", fmt.Errorf("写入 playbook 失败：%w", writeErr)
	}
	// 复用安装流程的 inventory 渲染（同一套 SSH 凭据与主机密钥策略）。
	installOpts := integration.RemoteOptions{
		Host: host, SSHUser: creds.User, SSHPort: creds.Port,
		SSHPassword: creds.Password, Become: true,
	}
	if creds.Password == "" && strings.TrimSpace(creds.Key) != "" {
		path, writeErr := s.writeSecret(item.Name+"-account.key", creds.Key)
		if writeErr != nil {
			return "", fmt.Errorf("写入临时私钥失败：%w", writeErr)
		}
		defer func() { _ = os.Remove(path) }()
		installOpts.SSHKeyFile = path
	}
	// inventory 文本由安装渲染器生成（含凭据），这里只需要它，不需要 playbook。
	installArt, err := integration.RenderRemoteInstall(tpl, instance, installOpts)
	if err != nil {
		// 安装渲染失败不影响账号 SQL：退化为只用 inventory 段落。
		installArt.Inventory = ""
	}
	if strings.TrimSpace(installArt.Inventory) == "" {
		return "", fmt.Errorf("生成临时 inventory 失败")
	}
	inventoryPath, err := s.writeSecret(item.Name+"-account.ini", installArt.Inventory)
	if err != nil {
		return "", fmt.Errorf("写入临时 inventory 失败：%w", err)
	}
	defer func() { _ = os.Remove(inventoryPath) }()
	varsPath, err := s.writeSecret(item.Name+"-account.vars.yml", art.VarsFile)
	if err != nil {
		return "", fmt.Errorf("写入临时变量文件失败：%w", err)
	}
	defer func() { _ = os.Remove(varsPath) }()

	timeout := s.cfg.Integration.Ansible.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, s.cfg.Integration.Ansible.Binary,
		"-i", inventoryPath, playbookPath, "-e", "@"+varsPath, "--become")
	cmd.Env = append(os.Environ(), "ANSIBLE_HOST_KEY_CHECKING=False", "ANSIBLE_NOCOLOR=1")
	output, runErr := cmd.CombinedOutput()
	safe := redactSecrets(string(output), creds.Password, creds.Key, execPassword)
	if runErr != nil {
		s.log.Error("集成：目标机账号 SQL 失败",
			zap.String("integration", item.Name), zap.String("host", host),
			zap.String("output", truncateText(safe, 8000)))
		return safe, fmt.Errorf("在目标机上执行账号 SQL 失败：%w\n%s", runErr, ansibleFailureExcerpt(safe, 900))
	}
	return safe, nil
}

// installMode 归一化安装方式（systemd → docker-systemd；空值取配置，再回落 docker）。
func (s *IntegrationService) installMode(creds RemoteCreds) string {
	mode := strings.TrimSpace(creds.InstallMode)
	if mode == "" {
		mode = s.cfg.Integration.Ansible.InstallMode
	}
	return integration.NormalizeInstallMode(mode)
}

// redactSecrets 从回传文本里擦除凭据（Ansible 已用 no_log，这里再兜一层）。
func redactSecrets(text string, secrets ...string) string {
	for _, secret := range secrets {
		if strings.TrimSpace(secret) == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "******")
	}
	return text
}

// probeHostPort 从平台侧探测目标端口（安装成功的最终判据）。
func probeHostPort(ctx context.Context, host string, port int) error {
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}
