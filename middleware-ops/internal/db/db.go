package db

import (
	"context"
	"fmt"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
)

// New 建立数据库连接并执行迁移。
//
// 说明：平台数据库按设计文档 2.3 固定为 PostgreSQL + pgvector，
// 因此不引入其他方言以保持 SQL 语义单一。
func New(cfg *config.Config, log *Logger) (*gorm.DB, error) {
	gormLog := logger.Default
	if !log.SQLVerbose {
		gormLog = logger.Default.LogMode(logger.Silent)
	}
	gdb, err := gorm.Open(postgres.Open(cfg.Database.DSN()), &gorm.Config{
		Logger: gormLog,
		NamingStrategy: schema.NamingStrategy{
			SingularTable: false,
		},
		// 平台所有时间统一以 UTC 落库，避免跨时区部署的口径漂移。
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("acquire sql db: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.Database.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.Database.ConnMaxLifetimeMinutes) * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	if cfg.Database.AutoMigrate {
		if err := model.Migrate(gdb); err != nil {
			return nil, fmt.Errorf("auto migrate: %w", err)
		}
		if err := postMigrate(gdb); err != nil {
			return nil, fmt.Errorf("post migrate: %w", err)
		}
	}
	return gdb, nil
}

// Logger 是数据库层所需的最小日志能力，由 internal/logger 注入。
type Logger struct {
	SQLVerbose bool
}

// Close 释放底层连接池。
func Close(gdb *gorm.DB) error {
	if gdb == nil {
		return nil
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
