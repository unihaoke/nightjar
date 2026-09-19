// Command server 是中间件智能问题解决平台的入口。
//
// 启动顺序：加载配置 → 初始化日志 → 连接数据库并迁移 → 初始化缓存/队列 →
// 初始化密钥与令牌 → 装配 AI 引擎与护栏 → 装配服务容器 → 引导内置角色与管理员
// → 注册路由 → 启动定时任务 → 监听端口 → 优雅关闭。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/db"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/integration"
	"middleware-ops/internal/logger"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/pkg/cache"
	"middleware-ops/internal/router"
	"middleware-ops/internal/service"
	"middleware-ops/internal/utils"
)

// version 为构建期注入的版本号（可通过 -ldflags 覆盖）。
var version = "1.0.0"

func main() {
	configPath := flag.String("config", "", "配置文件路径（默认读取 ./configs/config.yaml）")
	printVersion := flag.Bool("version", false, "打印版本号后退出")
	flag.Parse()

	if *printVersion {
		fmt.Printf("middleware-ops %s\n", version)
		return
	}

	if err := run(*configPath); err != nil {
		// 日志尚未初始化时直接输出到 stderr，避免静默失败。
		fmt.Fprintf(os.Stderr, "启动失败: %v\n", err)
		os.Exit(1)
	}
}

// run 执行完整的启动流程。
func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("加载配置: %w", err)
	}
	cfg.App.Version = version

	log, err := logger.New(logger.Config{
		Level:      cfg.Log.Level,
		FilePath:   cfg.Log.FilePath,
		MaxSizeMB:  cfg.Log.MaxSizeMB,
		MaxBackups: cfg.Log.MaxBackups,
		MaxAgeDays: cfg.Log.MaxAgeDays,
		Console:    cfg.Log.Console,
	})
	if err != nil {
		return fmt.Errorf("初始化日志: %w", err)
	}
	defer func() { _ = log.Sync() }()

	log.Info("中间件智能问题解决平台启动中",
		zap.String("version", version),
		zap.String("mode", cfg.App.Mode),
		zap.String("config", configPath),
		// 渲染器版本与平台代码修订号写进启动日志：远程安装报错时，
		// 先看这一行就能判断「平台模板有缺陷」还是「镜像没重建、仍在跑旧代码」（INC-005~009）。
		zap.String("playbook_renderer", integration.PlaybookRendererVersion),
		zap.String("code_revision", service.CodeRevision))

	// 数据库
	gdb, err := db.New(cfg, &db.Logger{SQLVerbose: cfg.App.Mode == "debug"})
	if err != nil {
		return fmt.Errorf("初始化数据库: %w", err)
	}
	defer func() { _ = db.Close(gdb) }()
	log.Info("数据库已就绪", zap.String("host", cfg.Database.Host), zap.String("name", cfg.Database.Name))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 缓存与队列（Redis 不可用时自动降级为内存实现）
	cacheDeps, err := cache.New(ctx, cfg, log)
	if err != nil {
		return fmt.Errorf("初始化缓存: %w", err)
	}
	defer func() { _ = cacheDeps.Store.Close() }()
	if cacheDeps.Degraded {
		log.Warn("缓存/队列已降级", zap.String("note", cacheDeps.Note))
	}

	// 主密钥与加密器
	masterKey, keySource, err := utils.LoadOrCreateMasterKey(cfg.Security.MasterKey, cfg.Security.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("加载主密钥: %w", err)
	}
	cipher, err := utils.NewCipher(masterKey)
	if err != nil {
		return fmt.Errorf("初始化加密器: %w", err)
	}
	log.Info("敏感数据加密已就绪", zap.String("key_source", keySource), zap.String("key_id", cipher.KeyID()))

	tokens, err := utils.NewTokenManager(cfg.JWT.Secret, cfg.JWT.Issuer, cfg.JWT.AccessTTL)
	if err != nil {
		return fmt.Errorf("初始化令牌管理: %w", err)
	}
	if cfg.App.Mode == "release" && len(cfg.JWT.Secret) < 32 {
		return errors.New("生产模式必须配置足够长度的 jwt.secret")
	}

	// AI 引擎（含降级链）
	factory := engine.NewFactory(engine.FactoryOptions{
		Config: cfg,
		OnDegrade: func(reason, from string) {
			log.Warn("AI 引擎降级", zap.String("from", from), zap.String("reason", reason))
		},
	})
	aiEngine := factory.Engine()
	engine.SetVectorNative(db.VectorEnabled)
	for _, note := range factory.Notes() {
		log.Warn("AI 引擎装配提示", zap.String("note", note))
	}
	strategy, providers := engine.Keys(cfg)
	log.Info("AI 引擎已就绪",
		zap.String("strategy", strategy),
		zap.Strings("providers", providers),
		zap.String("active", aiEngine.Name()))

	// 监控查询
	mon := monitor.New(cfg, cacheDeps.Store, log)
	log.Info("监控数据源已就绪", zap.String("source", mon.Kind()))

	// 启动期连通性预检：跨栈部署里最常见的故障是网络没接对
	// （例如平台容器解析不了 Prometheus 别名），
	// 此时页面只会显示"没有数据"，排查方向完全靠猜。这里把结论直接写进启动日志。
	if mon.Kind() == "prometheus" {
		if mon.Healthy(ctx) {
			log.Info("监控数据源连通性正常", zap.String("base_url", cfg.Prometheus.BaseURL))
		} else {
			log.Warn("监控数据源不可达：指标会显示「无数据」（平台不使用任何模拟数据）",
				zap.String("base_url", cfg.Prometheus.BaseURL),
				zap.String("hint", "确认 mwops-prometheus 容器在运行；跨栈集成**不需要**给平台准备任何互联网络——"+
					"集成中心会按容器名自动发现并接入目标网络，核对 MWOPS_PROMETHEUS_BASE_URL 与 PROMETHEUS_PORT 即可"))
		}
	} else {
		log.Warn("未配置 prometheus.base_url：监控数据源已禁用，页面会显示「无数据」（平台不提供模拟数据）",
			zap.String("hint", "设置 MWOPS_PROMETHEUS_BASE_URL=http://prometheus:9090 并启动平台自带 Prometheus"))
	}

	// 服务容器
	deps, err := service.NewContainer(service.ContainerOptions{
		Config:        cfg,
		DB:            gdb,
		Log:           log,
		Cache:         cacheDeps.Store,
		Queue:         cacheDeps.Queue,
		Cipher:        cipher,
		Tokens:        tokens,
		EngineFactory: factory,
		Monitor:       mon,
		AppURL:        fmt.Sprintf("http://%s:%d", cfg.Server.Host, cfg.Server.Port),
	})
	if err != nil {
		return fmt.Errorf("装配服务: %w", err)
	}
	defer deps.Audit.Close()

	// 引导内置角色与管理员账号
	if err := deps.Auth.Bootstrap(ctx); err != nil {
		return fmt.Errorf("初始化内置角色/账号: %w", err)
	}

	// 路由
	handlerEngine := router.New(router.Options{
		Config: cfg, Log: log, Deps: deps,
		HookToken: os.Getenv("MWOPS_HOOK_TOKEN"),
	})
	server := router.Server(cfg, handlerEngine)

	// 定时任务
	scheduler := service.NewScheduler(service.SchedulerConfig{
		Enabled:          cfg.Scheduler.Enabled,
		HealthProbe:      cfg.Scheduler.HealthProbe,
		RuleEval:         cfg.Scheduler.MetricRuleEval,
		AlertCluster:     cfg.Scheduler.AlertCluster,
		AuditSnapshot:    cfg.Scheduler.AuditSnapshot,
		ApprovalExpire:   cfg.Scheduler.ApprovalExpire,
		ClusterThreshold: cfg.Guardrail.VectorReferenceThreshold,
		LogAlertProcess:  time.Duration(cfg.LogAlert.WorkerInterval) * time.Second,
	}, deps.Middleware, deps.AlertSvc, deps.Audit, deps.Approval, deps.Integration, deps.LogAlertWorker, log)
	if err := scheduler.Start(); err != nil {
		return fmt.Errorf("启动定时任务: %w", err)
	}
	defer scheduler.Stop()

	// AI 分析任务的轮询兜底：回调丢了、或 AI 服务根本不支持回调时靠它把结论取回来，
	// 顺带把超过 task_timeout 的任务按超时收尾（事件不能永远停在"分析中"）。
	if deps.LogAlertWorker != nil && cfg.AIAnalysis.Enabled {
		pollInterval := cfg.AIAnalysis.PollInterval
		if pollInterval <= 0 {
			pollInterval = 60 * time.Second
		}
		go func() {
			ticker := time.NewTicker(pollInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					done, err := deps.LogAlertWorker.PollTasks(ctx)
					if err != nil {
						log.Warn("轮询 AI 分析任务失败", zap.Error(err))
					} else if done > 0 {
						log.Info("AI 分析任务已收尾", zap.Int("count", done))
					}
				}
			}
		}()
	}

	// 日志集成接收链路：Filebeat → 平台 Kafka → 日志事件。
	// 与 HTTP 服务同生命周期：进程退出时停止消费（未提交的位点下次会重新消费，日志事件层按指纹去重）。
	// 未配置 Kafka 时它只写一条说明日志，不阻断启动。
	deps.LogPipeline.Start(ctx)
	defer func() {
		if err := deps.LogPipeline.Close(); err != nil {
			log.Warn("关闭日志消费失败", zap.Error(err))
		}
	}()

	// 监听
	serverErr := make(chan error, 1)
	go func() {
		log.Info("HTTP 服务已启动",
			zap.String("addr", server.Addr),
			zap.String("health", "http://"+server.Addr+"/healthz"))
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-serverErr:
		return fmt.Errorf("HTTP 服务异常退出: %w", err)
	case sig := <-quit:
		log.Info("收到关闭信号，开始优雅关闭", zap.String("signal", sig.String()))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error("优雅关闭失败", zap.Error(err))
		return err
	}
	// 给审计异步队列留出落盘时间。
	time.Sleep(200 * time.Millisecond)
	log.Info("服务已安全退出")
	return nil
}
