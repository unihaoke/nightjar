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
		return fmt.Errorf("Ansible 执行失败：%w（输出：%s）", runErr, truncateText(safe, 600))
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
		return safe, fmt.Errorf("在目标机上执行账号 SQL 失败：%w（输出：%s）", runErr, truncateText(safe, 600))
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
