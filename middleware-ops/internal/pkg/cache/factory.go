package cache

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"middleware-ops/internal/config"
)

// Deps 是缓存与队列的组合依赖。
type Deps struct {
	Store Store
	Queue Queue
	// Degraded 为 true 表示 Redis 不可用，已降级为内存实现。
	Degraded bool
	// Note 记录降级原因。
	Note string
}

// New 依据配置创建缓存与队列，Redis 不可用时按配置降级为内存实现。
//
// 降级语义（设计文档 10. 可用性目标）：缓存层不可用不应导致平台不可启动或
// AI 诊断不可用，因此这里「能降级就降级」，并把降级事实暴露给系统概览接口。
func New(ctx context.Context, cfg *config.Config, log *zap.Logger) (Deps, error) {
	if cfg.Redis.Addr == "" {
		log.Warn("未配置 redis.addr，缓存与任务队列使用进程内实现（单机模式）")
		return Deps{
			Store:    NewMemoryStore(),
			Queue:    NewMemoryQueue(0),
			Degraded: true,
			Note:     "未配置 Redis，使用进程内缓存与队列",
		}, nil
	}

	client := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		PoolSize:     cfg.Redis.PoolSize,
		DialTimeout:  cfg.Redis.DialTimeout,
		ReadTimeout:  cfg.Redis.ReadTimeout,
		WriteTimeout: cfg.Redis.WriteTimeout,
	})

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		if !cfg.Redis.FallbackToMemory {
			return Deps{}, fmt.Errorf("connect redis: %w", err)
		}
		log.Warn("Redis 连接失败，已降级为进程内实现", zap.Error(err))
		return Deps{
			Store:    NewMemoryStore(),
			Queue:    NewMemoryQueue(0),
			Degraded: true,
			Note:     "Redis 连接失败：" + err.Error(),
		}, nil
	}

	consumer := fmt.Sprintf("%s-%d", hostname(), os.Getpid())
	queue, err := NewRedisQueue(ctx, client, cfg.Redis.StreamKey, cfg.Redis.Group, consumer, 5*time.Second)
	if err != nil {
		_ = client.Close()
		if !cfg.Redis.FallbackToMemory {
			return Deps{}, fmt.Errorf("init queue: %w", err)
		}
		log.Warn("Redis Stream 初始化失败，任务队列降级为进程内实现", zap.Error(err))
		return Deps{
			Store:    NewRedisStore(client),
			Queue:    NewMemoryQueue(0),
			Degraded: true,
			Note:     "任务队列降级：" + err.Error(),
		}, nil
	}

	log.Info("Redis 已连接", zap.String("addr", cfg.Redis.Addr), zap.String("consumer", consumer))
	return Deps{Store: NewRedisStore(client), Queue: queue}, nil
}

// hostname 返回主机名，用于构造消费者名称。
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "mwops"
	}
	return h
}
