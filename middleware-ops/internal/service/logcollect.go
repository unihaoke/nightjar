package service

import (
	"context"
	"os"
	"strings"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/docker"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/model"
)

// 日志接入：平台自己创建采集容器，从被管容器的 docker 配置里**反查**日志位置。
//
// 为什么这样做（用户明确要求）：
//   - 被管项目零改动：不需要它装 Agent、不需要改 compose、不需要暴露日志目录；
//   - 位置必须"读得到"：平台读被管容器的环境变量与挂载点，找到日志目录与承载它的
//     命名卷/宿主目录，然后把**同一个卷**挂进自己的采集容器（卷名相同即共享）；
//   - **读不到就拒绝配置**：绝不猜路径——猜错的后果是采集容器空转、日志页永远为空，
//     比直接报错难排查得多。

// LogCollectInput 是日志接入入参。
type LogCollectInput struct {
	// Name 为接入名称（唯一，用于采集容器命名与服务器记录）。
	Name string `json:"name" binding:"required,min=1,max=63"`
	// TargetContainer 为被管容器名（如 app-backend）。
	TargetContainer string `json:"target_container" binding:"required"`
	// Service 为日志事件归属的服务名（缺省取 Name）。
	Service string `json:"service"`
	// Environment 为环境（dev/staging/prod）。
	Environment string `json:"environment"`
	// Glob 可覆盖默认的采集通配（默认 <挂载点>/*.log）。
	Glob string `json:"glob"`
	// LevelFilter 为最低采集级别（ERROR / WARN / INFO），默认 ERROR。
	LevelFilter string `json:"level_filter"`
}

// LogCollectPlan 是"将要做什么"的预览结果。
type LogCollectPlan struct {
	Name            string                `json:"name"`
	Service         string                `json:"service"`
	Source          integration.LogSource `json:"source"`
	CollectorName   string                `json:"collector_name"`
	CollectorImage  string                `json:"collector_image"`
	Binds           []string              `json:"binds"`
	Networks        []string              `json:"networks"`
	AgentEnv        map[string]string     `json:"agent_env"`
	Steps           []string              `json:"steps"`
	DiscoveredFrom  string                `json:"discovered_from"`
	TargetContainer string                `json:"target_container"`
	Existing        *docker.State         `json:"existing,omitempty"`
	Warnings        []string              `json:"warnings"`
}

// collectorName 返回采集容器名。
func collectorName(name string) string {
	return integration.ContainerName(name) + "-logs"
}

// PreviewLogCollect 读取被管容器配置并给出采集方案；**读不到日志位置时返回错误**。
func (s *IntegrationService) PreviewLogCollect(ctx context.Context, in LogCollectInput) (*LogCollectPlan, error) {
	if s.docker == nil {
		return nil, apperr.New(apperr.CodeForbidden,
			"日志接入需要平台能访问 Docker（integration.docker_enabled=true 且挂载 docker.sock）")
	}
	if err := integration.ValidateName(in.Name); err != nil {
		return nil, apperr.New(apperr.CodeInvalidParam, err.Error())
	}
	detail, err := s.docker.InspectDetail(ctx, strings.TrimSpace(in.TargetContainer))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream, err)
	}
	if detail == nil {
		return nil, apperr.Newf(apperr.CodeNotFound,
			"找不到容器 %q：请确认容器名（docker ps 里的 NAMES 列）", in.TargetContainer)
	}

	mounts := make([]integration.MountInfo, 0, len(detail.Mounts))
	for _, mount := range detail.Mounts {
		mounts = append(mounts, integration.MountInfo{
			Type: mount.Type, Name: mount.Name, Source: mount.Source, Destination: mount.Destination,
		})
	}
	source, ok := integration.DiscoverLogSource(detail.Name, detail.Env, mounts, detail.Networks)
	if !ok {
		// 用户要求：读不到位置就不能配置。这里把"为什么读不到"讲清楚。
		reason := "未从 docker 配置中发现日志位置"
		if len(source.Evidence) > 0 {
			reason += "：" + strings.Join(source.Evidence, "；")
		}
		return nil, apperr.Newf(apperr.CodeInvalidParam,
			"%s。请在容器里显式声明日志目录（环境变量 LOG_PATH）或把日志目录挂载为卷/宿主目录后重试——"+
				"平台不接受手工猜测的路径，以避免采集容器空转", reason)
	}

	service := defaultString(strings.TrimSpace(in.Service), in.Name)
	glob := defaultString(strings.TrimSpace(in.Glob), source.Glob)
	level := strings.ToUpper(defaultString(strings.TrimSpace(in.LevelFilter), "ERROR"))

	agentEnv := map[string]string{
		"MWOPS_AGENT_PLATFORM_URL": "http://mwops-backend:8080",
		// 令牌与平台 /api/hooks/* 的校验令牌同源（环境变量注入，不落库）
		"MWOPS_AGENT_HOOK_TOKEN":    os.Getenv("MWOPS_HOOK_TOKEN"),
		"MWOPS_AGENT_SERVICE":       service,
		"MWOPS_AGENT_SERVER_NAME":   strings.TrimSpace(in.TargetContainer),
		"MWOPS_AGENT_ENVIRONMENT":   defaultString(in.Environment, s.defaultEnvironment()),
		"MWOPS_AGENT_POSITION_FILE": "/data/agent-position.json",
		"MWOPS_AGENT_FILES":         glob + ":error:" + level,
	}
	networks := append([]string{}, s.exporterNetworks()...)
	plan := &LogCollectPlan{
		Name: in.Name, Service: service, Source: source,
		CollectorName:  collectorName(in.Name),
		CollectorImage: s.selfImage(ctx),
		Binds: []string{
			source.MountSpec,              // 日志卷（只读）
			"mwops-log-agent-state:/data", // 偏移量，避免重启后重复上报
		},
		Networks: networks, AgentEnv: agentEnv,
		TargetContainer: detail.Name,
		DiscoveredFrom:  "docker inspect " + detail.Name,
		Steps: []string{
			"读取被管容器配置：" + detail.Name,
			"发现日志位置：" + source.Dir + "（" + strings.Join(source.Evidence, "；") + "）",
			"挂载同一份存储进采集容器：" + source.MountSpec,
			"用平台镜像创建采集容器 " + collectorName(in.Name) + "（Entrypoint=mwops-agent）",
			"采集 " + glob + "，级别 ≥ " + level + "，上报到平台 /api/hooks/logs",
		},
	}
	if agentEnv["MWOPS_AGENT_HOOK_TOKEN"] == "" {
		plan.Warnings = append(plan.Warnings,
			"平台未配置 HOOK_TOKEN：日志上报将不带令牌（仅平台未启用校验时可用）")
	}
	if existing, err := s.docker.Inspect(ctx, collectorName(in.Name)); err == nil && existing != nil {
		plan.Existing = existing
	}
	return plan, nil
}

// CreateLogCollect 按预览结果创建采集容器，并登记服务器（日志事件按服务器归集）。
func (s *IntegrationService) CreateLogCollect(ctx context.Context, in LogCollectInput, operator Operator) (*LogCollectPlan, error) {
	plan, err := s.PreviewLogCollect(ctx, in)
	if err != nil {
		return nil, err
	}
	envPairs := make([]string, 0, len(plan.AgentEnv))
	for key, value := range plan.AgentEnv {
		envPairs = append(envPairs, key+"="+integration.SanitizeEnvValue(value))
	}
	spec := docker.ContainerSpec{
		Name:  plan.CollectorName,
		Image: plan.CollectorImage,
		// 覆盖入口点复用平台镜像里的 Agent 二进制——不需要额外镜像，也不需要被管项目配合
		Entrypoint: []string{"/usr/local/bin/mwops-agent"},
		Env:        envPairs,
		Networks:   plan.Networks,
		Binds:      plan.Binds,
		Labels: map[string]string{
			"mwops.logcollect": plan.Name,
			"mwops.target":     plan.TargetContainer,
		},
	}
	action, id, err := s.docker.Ensure(ctx, spec)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeUpstream, err)
	}
	s.ensureLogServer(ctx, plan, operator)
	s.record(ctx, operator, 0, "log_collect_create", map[string]any{
		"name": plan.Name, "target": plan.TargetContainer, "service": plan.Service,
		"mount": plan.Source.MountSpec, "container": plan.CollectorName, "action": action,
	})
	s.log.Info("日志接入已创建",
		zap.String("collector", plan.CollectorName), zap.String("target", plan.TargetContainer),
		zap.String("mount", plan.Source.MountSpec), zap.String("container_id", id))
	plan.Steps = append(plan.Steps, "采集容器已"+actionLabel(action)+"（ID "+shortID(id)+"）")
	return plan, nil
}

// ensureLogServer 登记/更新日志域里的"服务器"记录。
//
// 日志事件按「服务器 + 服务名」归集（见 docs/INTEGRATION.md §8.6），
// 因此接入后要先有这条记录，页面上才看得见。
func (s *IntegrationService) ensureLogServer(ctx context.Context, plan *LogCollectPlan, operator Operator) {
	if s.servers == nil {
		return
	}
	// 已登记的服务器不重复插入（Agent 上报时也会自动注册）
	if _, _, err := s.servers.List(ctx, plan.TargetContainer, "", 5, 0); err == nil {
		return
	}
	server := &model.ServerInstance{
		Name: plan.TargetContainer, Hostname: plan.TargetContainer,
		Environment: defaultString(plan.AgentEnv["MWOPS_AGENT_ENVIRONMENT"], model.EnvDev),
		Status:      1,
		Tags:        model.JSONStringSlice([]string{"log-collect", plan.Name}),
	}
	if err := s.servers.Create(ctx, server); err != nil {
		s.log.Debug("日志接入：登记服务器失败（Agent 上报时会自动注册，不影响采集）", zap.Error(err))
	}
}

// selfImage 返回平台自身镜像名——采集容器复用它（只覆盖 Entrypoint）。
//
// 为什么用自身镜像：Agent 二进制已经打进平台镜像（见 middleware-ops/Dockerfile），
// 因此不需要额外构建/分发 Agent 镜像，版本也天然一致。
//
// 取法：Docker 会把容器 ID 写进 HOSTNAME，据此 inspect 自己拿到镜像名；
// 失败则退回 compose 里的默认镜像名。
func (s *IntegrationService) selfImage(ctx context.Context) string {
	if s.selfImageName != "" {
		return s.selfImageName
	}
	fallback := "mwops-backend:latest"
	selfID := strings.TrimSpace(os.Getenv("HOSTNAME"))
	if selfID == "" {
		return fallback
	}
	detail, err := s.docker.InspectDetail(ctx, selfID)
	if err != nil || detail == nil || detail.Image == "" {
		s.log.Debug("日志接入：读取平台自身镜像名失败，使用默认值", zap.Error(err))
		return fallback
	}
	s.selfImageName = detail.Image
	return s.selfImageName
}
