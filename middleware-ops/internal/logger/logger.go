// Package logger 基于 zap 提供结构化日志与本地轮转。
package logger

import (
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// Config 为日志初始化参数。
type Config struct {
	Level      string
	FilePath   string
	MaxSizeMB  int
	MaxBackups int
	MaxAgeDays int
	Console    bool
}

// New 构建 zap Logger。写盘失败时自动退化为仅控制台输出，保证平台可启动。
func New(cfg Config) (*zap.Logger, error) {
	level := zapcore.InfoLevel
	if err := level.UnmarshalText([]byte(cfg.Level)); err != nil {
		level = zapcore.InfoLevel
	}

	encCfg := zap.NewProductionEncoderConfig()
	encCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	encCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
	encCfg.TimeKey = "ts"
	encCfg.MessageKey = "msg"

	cores := make([]zapcore.Core, 0, 2)
	if cfg.Console {
		cores = append(cores, zapcore.NewCore(
			zapcore.NewConsoleEncoder(encCfg),
			zapcore.AddSync(os.Stdout),
			level,
		))
	}
	if cfg.FilePath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.FilePath), 0o755); err == nil {
			sink := zapcore.AddSync(&lumberjack.Logger{
				Filename:   cfg.FilePath,
				MaxSize:    cfg.MaxSizeMB,
				MaxBackups: cfg.MaxBackups,
				MaxAge:     cfg.MaxAgeDays,
				Compress:   true,
			})
			cores = append(cores, zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), sink, level))
		}
	}
	if len(cores) == 0 {
		cores = append(cores, zapcore.NewCore(
			zapcore.NewConsoleEncoder(encCfg),
			zapcore.AddSync(os.Stdout),
			level,
		))
	}

	return zap.New(zapcore.NewTee(cores...), zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel)), nil
}

// Nop 返回不输出任何内容的 Logger，供测试使用。
func Nop() *zap.Logger { return zap.NewNop() }

// WithTime 返回带时间戳字段的 SugaredLogger，便于周期性任务统一日志格式。
func WithTime(log *zap.Logger, name string) *zap.SugaredLogger {
	return log.With(zap.String("component", name), zap.Time("at", time.Now().UTC())).Sugar()
}
