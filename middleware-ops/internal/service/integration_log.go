package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
)

// 本文件实现「日志集成」（docs/LOG_INTEGRATION.md）：平台用 Ansible 在目标服务器上
// **幂等**部署 Filebeat（已存在则跳过安装，只校验配置），Filebeat 把日志推到平台自带的
// Kafka，平台后端按消费组消费后进入既有日志事件链路。
//
// 与指标集成（Exporter + Prometheus）刻意分成两条路径：
//   - 不生成 Prometheus 抓取配置（file_sd / 告警规则都不参与，见 ServiceDiscovery 的过滤）；
//   - 不做"平台 → Exporter 端口"探测，改为探测 Kafka 接入地址与"日志是否真的进来了"；
//   - 部署产物是 filebeat.yml + 一份 ansible playbook，不是容器/二进制 Exporter。

// 日志集成的参数键（与 integration 模板的 Options 一一对应）。
const (
	optLogPaths       = "MWOPS_LOG_PATHS"
	optLogService     = "MWOPS_LOG_SERVICE"
	optLogEnvironment = "MWOPS_LOG_ENVIRONMENT"
	optLogLevel       = "MWOPS_LOG_LEVEL"
	optLogMultiline   = "MWOPS_LOG_MULTILINE"
	optLogPattern     = "MWOPS_LOG_MULTILINE_PATTERN"
	optLogInstallMode = "MWOPS_LOG_INSTALL_MODE"
	optLogBeatVersion = "MWOPS_LOG_FILEBEAT_VERSION"
)

// LogPipelineProbe 是日志集成自检需要的最小依赖面。
//
// 用接口而不是直接持有 *LogPipeline：集成服务不应该知道日志链路的内部结构，
// 它只问两个问题——"Kafka 通不通"和"链路现在什么状态"。
type LogPipelineProbe interface {
	Status() LogPipelineStatus
	Probe(ctx context.Context) LogPipelineProbeResult
}

// SetLogPipeline 注入日志链路（在容器装配阶段调用，避免构造函数签名继续膨胀）。
func (s *IntegrationService) SetLogPipeline(p LogPipelineProbe) { s.logPipe = p }

// isLogTemplate 判断某个模板是否属于「日志集成」。
func isLogTemplate(tpl integration.Template) bool {
	return tpl.CategoryOf() == integration.CategoryLog
}

// deployAttemptLabel / redeployAttemptLabel 是「平台正在为你做什么」的文案。
//
// 为什么必须按类型分：这两个字符串会出现在待处理横幅、实例备注与后台任务日志里
// （真实反馈："待处理项：创建只读账号并拉起 Exporter失败"——可那是日志集成，
// 既没有只读账号也没有 Exporter）。文案错位会让人按错误的思路排查。
func deployAttemptLabel(tpl integration.Template) string {
	if isLogTemplate(tpl) {
		return "部署 Filebeat 日志采集"
	}
	return "创建只读账号并拉起 Exporter"
}

func redeployAttemptLabel(tpl integration.Template) string {
	if isLogTemplate(tpl) {
		return "重新部署 Filebeat"
	}
	return "重建账号与 Exporter"
}

// isLogMeta 从集成元信息判断它是不是日志集成（删除、自检等处只有 meta）。
func isLogMeta(meta IntegrationMeta) bool {
	tpl, ok := integration.TemplateOf(meta.Template)
	return ok && isLogTemplate(tpl)
}

// containerNameFor 返回 Exporter 容器名。
//
// 日志集成没有 Exporter 容器（采集侧是目标机上的 Filebeat），这里返回空串：
// 否则「删除集成」会去 docker 里找一个不存在的容器，留下一条没意义的警告。
func containerNameFor(tpl integration.Template, name string) string {
	if isLogTemplate(tpl) {
		return ""
	}
	return integration.ContainerName(name)
}

// logInputOf 把集成记录 + 参数渲染成 Filebeat 输入。
//
// Kafka 地址与 topic **只来自平台配置**，不允许在集成里填写：被管机应该推到平台的哪条总线，
// 是平台自己的事实，不是每个集成各自的选择——否则一个填错就会写进一条永远没人消费的 topic。
func (s *IntegrationService) logInputOf(item *model.MiddlewareInstance, tpl integration.Template, instance integration.Instance, meta IntegrationMeta) (integration.LogInput, error) {
	options := meta.Options
	if options == nil {
		options = instance.Options
	}
	paths := splitLogPaths(options[optLogPaths])
	if len(paths) == 0 {
		return integration.LogInput{}, apperr.Newf(apperr.CodeInvalidParam,
			"日志路径不能为空：请填至少一个 glob（如 /var/log/app/*.log），多个用换行或逗号分隔")
	}
	if !s.cfg.Kafka.Active() {
		return integration.LogInput{}, apperr.New(apperr.CodeForbidden,
			"平台未启用日志总线（kafka.brokers 为空）：请在 .env 配置 KAFKA_BROKERS 与 KAFKA_ADVERTISED_HOST 后重启平台")
	}

	installMode := strings.TrimSpace(options[optLogInstallMode])
	if installMode == "" {
		// 默认 auto：目标机上已有 Filebeat 就复用，有 docker 就用容器，否则走包安装。
		installMode = "auto"
	}
	version := strings.TrimSpace(options[optLogBeatVersion])
	if version == "" {
		version = s.cfg.Kafka.FilebeatVersion
	}

	return integration.LogInput{
		// 目标机地址：优先用远程部署填的 TargetHost（集群维度），否则用集成的地址字段。
		Host:             firstNonEmptyString(meta.TargetHost, instance.Address.Host),
		Name:             item.Name,
		Service:          firstNonEmptyString(options[optLogService], item.Name),
		Environment:      firstNonEmptyString(options[optLogEnvironment], item.Environment, s.defaultEnvironment()),
		Paths:            paths,
		Level:            firstNonEmptyString(options[optLogLevel], "ERROR"),
		Multiline:        !isFalse(options[optLogMultiline]),
		MultilinePattern: strings.TrimSpace(options[optLogPattern]),
		KafkaHosts:       s.cfg.Kafka.FilebeatHosts(),
		Topic:            s.cfg.Kafka.LogTopic,
		FilebeatVersion:  version,
		InstallMode:      installMode,
	}, nil
}

// validateLogInstance 校验日志集成的入参。
//
// 刻意复用渲染器的校验（路径非空、Kafka 地址非空、topic 非空、名称合法），
// 而不是另写一套：两套校验早晚会分叉，届时会出现"保存通过了但渲染失败"。
func (s *IntegrationService) validateLogInstance(item *model.MiddlewareInstance, tpl integration.Template, instance integration.Instance) error {
	meta := IntegrationMeta{
		Template: tpl.Type, Address: instance.Address.Raw, Options: instance.Options,
		DeployTarget: DeployTargetRemote, TargetHost: instance.Address.Host,
	}
	input, err := s.logInputOf(item, tpl, instance, meta)
	if err != nil {
		return err
	}
	if _, err := integration.RenderFilebeatConfig(input); err != nil {
		return err
	}
	return nil
}

// previewLogIntegration 渲染 filebeat.yml 与执行命令（集成表单的「预览」）。
//
// Artifacts 这个结构原本是给 Prometheus 集成用的，这里复用它的字段承载日志集成产物：
//
//	Compose    = 渲染后的 filebeat.yml（会落到目标机 /etc/filebeat/filebeat.yml）
//	Playbook   = 实际执行的 ansible playbook（保持与指标集成一致的"可审计"要求）
//	DeployCmd  = 执行命令（不含凭据）
//	VerifySteps= 自检步骤文案
//
// 其余 Prometheus 相关字段（Targets/FileSD/ScrapeJob/Selector）保持为空——
// 日志集成与 Prometheus 无关，填进去只会误导。
func (s *IntegrationService) previewLogIntegration(in IntegrationInput) (*integration.Artifacts, error) {
	tpl, instance, err := s.build(in)
	if err != nil {
		return nil, err
	}
	meta := IntegrationMeta{
		Template: tpl.Type, Address: instance.Address.Raw, Options: instance.Options,
		DeployTarget: normalizeDeployTarget(in.DeployTarget), TargetHost: strings.TrimSpace(in.TargetHost),
	}
	item := &model.MiddlewareInstance{Name: instance.Name, MWType: tpl.Type, Environment: instance.Environment}
	input, err := s.logInputOf(item, tpl, instance, meta)
	if err != nil {
		return nil, err
	}
	config, err := integration.RenderFilebeatConfig(input)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	opts := integration.RemoteOptions{
		Host: input.Host, SSHUser: strings.TrimSpace(in.SSHUser), SSHPort: in.SSHPort,
		Become: s.cfg.Integration.Ansible.Become,
	}
	art, err := integration.RenderFilebeatInstall(input, opts)
	if err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	steps := []string{
		"渲染 filebeat.yml（输入：" + strings.Join(input.Paths, "、") + "；级别 ≥ " + input.Level + "）",
		"Ansible 连接目标机 " + input.Host + "，探测是否已安装 Filebeat（已存在则跳过安装）",
		"下发 /etc/filebeat/filebeat.yml（内容未变化则不重启，避免采集抖动）",
		"确保 filebeat 服务运行：" + "systemctl status filebeat",
		"Filebeat 推送到 Kafka " + strings.Join(input.KafkaHosts, ",") + "（topic " + input.Topic + "）",
		"平台消费后进入「日志监控」列表（按服务与指纹归集）",
	}
	return &integration.Artifacts{
		JobName:     s.jobName(),
		Compose:     config,
		DeployCmd:   art.RunCommand,
		VerifySteps: steps,
		NetworkNote: "日志集成不需要给 Prometheus 建抓取目标；被管机只需要能访问 " +
			strings.Join(input.KafkaHosts, ",") + "（即 .env 的 KAFKA_ADVERTISED_HOST:KAFKA_PORT）",
	}, nil
}

// deployLogIntegration 远程安装/更新 Filebeat（幂等）。
//
// 与 Exporter 的远程安装在流程上刻意保持一致（生产走审批、凭据只入 0600 临时文件、
// 泄露风险高的输出先脱敏再截断），差别只有产物与"部署完怎么算成功"：
// Exporter 靠端口探测，日志集成靠"日志是否真的进来了"（自检第 3 步）。
func (s *IntegrationService) deployLogIntegration(
	ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template,
	instance integration.Instance, meta IntegrationMeta, creds RemoteCreds, operator Operator,
) error {
	if err := s.remoteReady(); err != nil {
		return err
	}
	input, err := s.logInputOf(item, tpl, instance, meta)
	if err != nil {
		return err
	}
	if strings.TrimSpace(input.Host) == "" {
		return fmt.Errorf("日志集成需要填写目标服务器地址（集群维度）")
	}

	// 生产环境：在别的机器上装东西属于 L2 → 只开工单，不执行（与 Exporter 安装同一策略）。
	if instance.Environment == model.EnvProd && s.approval != nil {
		ticket, ticketErr := s.approval.Create(ctx, ApprovalRequest{
			InstanceID: item.ID, Environment: instance.Environment,
			ActionType: "integration_filebeat_install",
			ActionDetail: map[string]any{
				"name": item.Name, "target_host": input.Host, "paths": input.Paths,
				"install_mode": input.InstallMode, "kafka": input.KafkaHosts, "topic": input.Topic,
			},
			Reason: "生产环境由平台在远程服务器安装 Filebeat 采集日志（L2）",
		}, operator)
		if ticketErr != nil {
			return fmt.Errorf("创建审批工单失败：%w", ticketErr)
		}
		return fmt.Errorf("生产环境需审批：已创建工单 %s；审批通过后请点「重新应用」（届时需重新填写 SSH 凭据）", ticket.TicketID)
	}

	if !creds.provided() {
		return fmt.Errorf("远程安装 Filebeat 需要 SSH 凭据（用户名 + 口令或私钥）：" +
			"请在「重新应用」弹窗里填写；凭据只在本次请求使用、不落库，因此无法复用上一次的凭据")
	}
	if err := s.checkSSHPass(creds); err != nil {
		return err
	}

	become := s.cfg.Integration.Ansible.Become
	if creds.Become != nil {
		become = *creds.Become
	}
	opts := integration.RemoteOptions{
		Host: input.Host, SSHUser: creds.User, SSHPort: creds.Port,
		InstallMode: input.InstallMode, InstallDir: s.cfg.Integration.Ansible.InstallDir,
		Become: become, ExtraArgs: s.cfg.Integration.Ansible.ExtraArgs,
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

	art, err := integration.RenderFilebeatInstall(input, opts)
	if err != nil {
		return err
	}
	artifactDir := filepath.Join(s.cfg.Integration.OutputDir, "ansible")
	if mkErr := os.MkdirAll(artifactDir, 0o750); mkErr != nil {
		return fmt.Errorf("创建产物目录失败：%w", mkErr)
	}
	playbookPath := filepath.Join(artifactDir, item.Name+"-filebeat.yml")
	if writeErr := os.WriteFile(playbookPath, []byte(art.Playbook), 0o644); writeErr != nil {
		return fmt.Errorf("写入 playbook 失败：%w", writeErr)
	}
	inventoryPath, err := s.writeSecret(item.Name+"-filebeat.ini", art.Inventory)
	if err != nil {
		return fmt.Errorf("写入临时 inventory 失败：%w", err)
	}
	defer func() { _ = os.Remove(inventoryPath) }()
	varsPath, err := s.writeSecret(item.Name+"-filebeat.vars.yml", art.VarsFile)
	if err != nil {
		return fmt.Errorf("写入临时变量文件失败：%w", err)
	}
	defer func() { _ = os.Remove(varsPath) }()

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
		s.log.Error("日志集成：Filebeat 安装失败",
			zap.String("integration", item.Name), zap.String("host", input.Host),
			zap.String("output", truncateText(safe, 8000)))
		return fmt.Errorf("Ansible 执行失败：%w\n%s%s", runErr, ansibleFailureExcerpt(safe, 900), censoredHint(safe))
	}

	// 部署成功只代表"Filebeat 已就位"；日志是否真的流到平台由自检第 3 步判定。
	// 这里把 ansible 的关键结论写进备注，省得使用者为了看一行结论去翻平台日志。
	note := fmt.Sprintf("Filebeat 已就位（目标机 %s，安装方式 %s）：%s", input.Host, input.InstallMode,
		lastMeaningfulLine(safe))
	s.setDeployNote(ctx, item.ID, note)
	s.log.Info("日志集成：Filebeat 部署完成",
		zap.String("integration", item.Name), zap.String("host", input.Host),
		zap.String("kafka", strings.Join(input.KafkaHosts, ",")), zap.String("topic", input.Topic))
	return nil
}

// applyLogIntegration 是「重新应用」在日志集成上的实现。
//
// 顺序刻意与指标集成一致：先清掉上一次的失败原因（否则界面会把旧错误当成这次的结果），
// 再放后台执行（装包可能要几分钟，不能让请求超时），最后登记服务器记录。
func (s *IntegrationService) applyLogIntegration(
	ctx context.Context, item *model.MiddlewareInstance, tpl integration.Template,
	meta IntegrationMeta, creds SSHCredsInput, operator Operator,
) (*IntegrationView, error) {
	if !creds.provided() {
		return nil, apperr.New(apperr.CodeInvalidParam,
			"部署 Filebeat 需要 SSH 凭据：请在「重新应用」弹窗里填写 SSH 用户名与口令或私钥"+
				"（凭据仅本次使用、不落库）")
	}
	password, err := s.decrypt(item.PasswordEncrypted)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	address := integration.Address{
		Host: firstNonEmptyString(meta.TargetHost, item.Host), Raw: meta.Address,
	}
	instance := integration.Instance{
		Name: item.Name, MWType: tpl.Type, Address: address,
		Username: item.Username, Password: password,
		Options: meta.Options, Environment: item.Environment, GroupName: item.GroupName,
	}
	if fresh := s.beginAttempt(ctx, item.ID, "部署 Filebeat"); fresh != nil {
		item = fresh
	}
	s.ensureLogServer(ctx, item, meta)
	s.runAsync(item.ID, item.Name, "部署 Filebeat", func(bgCtx context.Context) error {
		if err := s.deployLogIntegration(bgCtx, item, tpl, instance, meta,
			creds.remoteCreds(meta.TargetHost), operator); err != nil {
			return err
		}
		s.markApplied(context.Background(), item.ID)
		return nil
	})
	s.record(ctx, operator, item.ID, "log_integration_apply", map[string]any{
		"name": item.Name, "target_host": address.Host,
	})
	return s.Get(ctx, item.ID, Scope{})
}

// ensureLogServer 预登记服务器记录（日志事件按 IP 归集到它）。
//
// 为什么要预登记而不是等自动注册：自动注册出来的记录环境固定是 dev、名字是 IP，
// 而集成里明确填了环境与名称——先登记好，日志进来时就能落到正确的环境与名称上，
// 「日志监控」页按环境筛选才不会错位。
func (s *IntegrationService) ensureLogServer(ctx context.Context, item *model.MiddlewareInstance, meta IntegrationMeta) {
	if s.servers == nil {
		return
	}
	host := firstNonEmptyString(meta.TargetHost, item.Host)
	if host == "" {
		return
	}
	existing, err := s.servers.GetByIP(ctx, host)
	if err == nil && existing != nil {
		// 已存在：只补齐环境（不动用户可能手工改过的名称与其他字段）。
		if existing.Environment != item.Environment {
			existing.Environment = item.Environment
			if updateErr := s.servers.Update(ctx, existing); updateErr != nil {
				s.log.Debug("日志集成：更新服务器环境失败", zap.Error(updateErr))
			}
		}
		return
	}
	server := &model.ServerInstance{
		Name: item.Name, IP: host, Hostname: host, Environment: item.Environment, Status: 1,
		Tags: model.JSONStringSlice([]string{"log-integration", item.Name}),
	}
	if err := s.servers.Create(ctx, server); err != nil {
		s.log.Debug("日志集成：登记服务器失败（日志到达时会自动注册，不影响采集）", zap.Error(err))
	}
}

// selfCheckLog 是日志集成的自检：三段式，按"从平台到目标机再到数据"的顺序给结论。
//
// 与指标集成不同，这里**不需要 SSH**（自检只读、随时可点）：
// 目标机上 Filebeat 到底起没起来，最终一定会体现在"日志有没有进来"上——
// 与其猜，不如直接看数据面（这也是本项目"只用真实数据说话"的一贯做法）。
func (s *IntegrationService) selfCheckLog(ctx context.Context, item *model.MiddlewareInstance, meta IntegrationMeta) *IntegrationSelfCheck {
	out := &IntegrationSelfCheck{InstanceID: item.ID, Name: item.Name, Stages: make([]SelfCheckStage, 0, 3)}

	// ① 平台侧 Kafka
	kafkaStage := SelfCheckStage{Key: "kafka", Title: "平台 → Kafka 日志总线"}
	if s.logPipe == nil {
		kafkaStage.Status = stageFail
		kafkaStage.Detail = "日志链路未装配"
		kafkaStage.Advice = "平台未启用日志总线：检查 .env 的 KAFKA_BROKERS 与 docker compose 里的 kafka 服务"
	} else {
		result := s.logPipe.Probe(ctx)
		if result.OK {
			kafkaStage.Status = stageOK
			kafkaStage.Detail = fmt.Sprintf("%s（耗时 %dms）", result.Message, result.LatencyMS)
		} else {
			kafkaStage.Status = stageFail
			kafkaStage.Detail = result.Message
			kafkaStage.Advice = "确认 kafka 容器在运行、brokers 地址正确；" +
				"平台侧地址是容器网络的 kafka:29092，不是宿主端口"
		}
	}
	out.Stages = append(out.Stages, kafkaStage)

	// ② 被管机要连的对外地址（advertised）
	externalStage := SelfCheckStage{Key: "external", Title: "被管机接入地址（Kafka EXTERNAL）"}
	if s.logPipe == nil {
		externalStage.Status = stageWarn
		externalStage.Detail = "日志链路未装配，无法读取对外地址"
	} else {
		status := s.logPipe.Status()
		address := status.ExternalAddress
		externalStage.Detail = "被管机上的 Filebeat 会连 " + address + "（topic " + status.Topic + "）"
		if err := probeHostPort(ctx, splitHost(address), splitPort(address)); err != nil {
			externalStage.Status = stageWarn
			externalStage.Advice = "平台自己都连不上这个地址，被管机大概率也连不上：" +
				"把 .env 的 KAFKA_ADVERTISED_HOST 改成被管机可达的平台 IP（不要写 localhost），并放通端口"
		} else {
			externalStage.Status = stageOK
			externalStage.Detail += "；平台侧探测可达（被管机视角仍需在目标机执行 nc -vz 验证）"
		}
	}
	out.Stages = append(out.Stages, externalStage)

	// ③ 数据面：最近有没有日志进来
	dataStage := SelfCheckStage{Key: "data", Title: "日志是否已进入平台"}
	host := firstNonEmptyString(meta.TargetHost, item.Host)
	if s.servers == nil || host == "" {
		dataStage.Status = stageWarn
		dataStage.Detail = "无法定位服务器记录（缺少目标机地址）"
	} else if server, err := s.servers.GetByIP(ctx, host); err != nil || server == nil {
		dataStage.Status = stageWarn
		dataStage.Detail = fmt.Sprintf("还没有 %s 的日志记录", host)
		dataStage.Advice = "首个日志到达时平台会自动登记该服务器；若一直为空，请在目标机执行：" +
			"systemctl status filebeat 与 filebeat test output"
	} else if server.LastSeenAt == nil {
		dataStage.Status = stageWarn
		dataStage.Detail = "服务器已登记，但还没有收到过日志"
		dataStage.Advice = "检查目标机 Filebeat 服务状态与 filebeat test output（能否连上 Kafka）"
	} else {
		age := time.Since(*server.LastSeenAt)
		dataStage.Detail = fmt.Sprintf("最近一条日志：%s（%s 前）",
			server.LastSeenAt.Local().Format("2006-01-02 15:04:05"), humanDuration(age))
		if age <= 30*time.Minute {
			dataStage.Status = stageOK
		} else {
			dataStage.Status = stageWarn
			dataStage.Advice = "已经超过 30 分钟没有新日志：确认目标机日志确实在产生、" +
				"路径 glob 是否匹配、以及 Filebeat 是否仍在运行"
		}
	}
	out.Stages = append(out.Stages, dataStage)

	out.OK = true
	firstFail := SelfCheckStage{}
	for _, stage := range out.Stages {
		if stage.Status == stageFail {
			out.OK = false
			if firstFail.Key == "" {
				firstFail = stage
			}
		}
	}
	if out.OK {
		out.Summary = "链路正常：平台可连 Kafka → 接入地址可用 → 已有日志进入平台"
		return out
	}
	out.Summary = fmt.Sprintf("在「%s」这一环断了：%s", firstFail.Title, firstFail.Advice)
	out.NextAction, out.NextActionLabel = firstFail.Action, firstFail.ActionLabel
	return out
}

// ---------------------------------------------------------------------------
// 纯函数辅助（可单测）
// ---------------------------------------------------------------------------

// splitLogPaths 把日志路径拆成列表：既支持换行分隔（多行输入框），也支持逗号分隔。
func splitLogPaths(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	})
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// isFalse 判断"显式关闭"的取值；未填写时返回 false（表示保持默认开启）。
func isFalse(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "false", "0", "no", "off", "n":
		return true
	default:
		return false
	}
}

// firstNonEmptyString 返回第一个非空（去空白后）的字符串。
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// splitHost / splitPort 从 host:port 里取值（自检探测需要分开传参）。
func splitHost(address string) string {
	if idx := strings.LastIndex(address, ":"); idx > 0 {
		return strings.Trim(address[:idx], "[]")
	}
	return address
}

func splitPort(address string) int {
	idx := strings.LastIndex(address, ":")
	if idx < 0 {
		return 0
	}
	port := 0
	for _, r := range address[idx+1:] {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
	}
	return port
}

// humanDuration 把时长说成人话（"3 分钟前"比 "3m12.5s 前" 好用）。
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分钟", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d 小时", int(d.Hours()))
	}
	return fmt.Sprintf("%d 天", int(d.Hours()/24))
}

// lastMeaningfulLine 取 ansible 输出里最后一行有信息量的内容（用于部署备注）。
//
// 为什么不放全文：备注是给人一眼看的，ansible 的 full recap 有几十行，
// 真正有用的通常只有最后那几行（"目标机已有 filebeat，跳过安装" 之类）。
func lastMeaningfulLine(output string) string {
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		return truncateText(line, 200)
	}
	return "（无输出）"
}
