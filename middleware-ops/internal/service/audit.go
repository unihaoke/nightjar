package service

import (
	"context"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// AuditEntry 是一条待写入的审计记录。
type AuditEntry struct {
	UserID     int64
	Username   string
	InstanceID int64
	ActionType string
	Level      string
	Result     string
	IPAddress  string
	UserAgent  string
	Route      string
	Detail     map[string]any
}

// AuditService 提供审计日志写入与检索（4.7 / 6.4）。
type AuditService struct {
	repo *repository.AuditRepository
	log  *zap.Logger
	// snapshotDir 为每日快照落盘目录。
	snapshotDir string
	// ch 为异步写入通道，避免审计阻塞请求主链路。
	ch   chan AuditEntry
	done chan struct{}
}

// NewAuditService 构造审计服务并启动异步写入协程。
func NewAuditService(repo *repository.AuditRepository, snapshotDir string, log *zap.Logger) *AuditService {
	svc := &AuditService{
		repo:        repo,
		log:         log,
		snapshotDir: snapshotDir,
		ch:          make(chan AuditEntry, 1024),
		done:        make(chan struct{}),
	}
	go svc.loop()
	return svc
}

// Record 同步写入审计记录。
func (s *AuditService) Record(ctx context.Context, entry AuditEntry) error {
	item := &model.AuditLog{
		UserID:       entry.UserID,
		Username:     entry.Username,
		InstanceID:   entry.InstanceID,
		ActionType:   entry.ActionType,
		ActionDetail: model.JSONMap(entry.Detail),
		Result:       entry.Result,
		Level:        entry.Level,
		IPAddress:    entry.IPAddress,
		UserAgent:    entry.UserAgent,
		Route:        entry.Route,
		CreatedAt:    time.Now().UTC(),
	}
	if item.Result == "" {
		item.Result = "success"
	}
	if err := s.repo.Append(ctx, item); err != nil {
		s.log.Error("写入审计日志失败", zap.Error(err), zap.String("action", entry.ActionType))
		return err
	}
	return nil
}

// RecordAsync 异步写入审计记录（用于高频只读操作，如 AI 工具调用）。
func (s *AuditService) RecordAsync(ctx context.Context, entry AuditEntry) {
	select {
	case s.ch <- entry:
	case <-ctx.Done():
	default:
		// 通道满时退化为同步写入，宁可慢一点也不能丢审计。
		if err := s.Record(context.Background(), entry); err != nil {
			s.log.Error("审计日志兜底写入失败", zap.Error(err))
		}
	}
}

// loop 消费异步审计队列。
func (s *AuditService) loop() {
	defer close(s.done)
	for entry := range s.ch {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.Record(ctx, entry); err != nil {
			s.log.Error("异步审计写入失败", zap.Error(err), zap.String("action", entry.ActionType))
		}
		cancel()
	}
}

// Close 关闭异步写入并落盘剩余记录。
func (s *AuditService) Close() {
	close(s.ch)
	<-s.done
}

// List 分页检索审计日志。
func (s *AuditService) List(ctx context.Context, f repository.AuditFilter, limit, offset int) ([]model.AuditLog, int64, error) {
	items, total, err := s.repo.List(ctx, f, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// Get 查询审计详情。
func (s *AuditService) Get(ctx context.Context, id int64) (*model.AuditLog, error) {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "审计记录不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// Verify 校验哈希链完整性。
func (s *AuditService) Verify(ctx context.Context, fromID, toID int64) (bool, int64, error) {
	ok, brokenID, err := s.repo.VerifyChain(ctx, fromID, toID)
	if err != nil {
		return false, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return ok, brokenID, nil
}

// Snapshot 生成当日哈希链快照。
func (s *AuditService) Snapshot(ctx context.Context) (*model.AuditSnapshot, error) {
	snapshot, err := s.repo.CreateSnapshot(ctx, s.snapshotDir)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if !snapshot.Verified {
		s.log.Error("审计哈希链校验失败，疑似被篡改",
			zap.String("date", snapshot.SnapshotDate), zap.String("chain_hash", snapshot.ChainHash))
	}
	return snapshot, nil
}

// ListSnapshots 返回最近快照。
func (s *AuditService) ListSnapshots(ctx context.Context, limit int) ([]model.AuditSnapshot, error) {
	items, err := s.repo.ListSnapshots(ctx, limit)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, nil
}

// ActionStats 返回操作类型统计。
func (s *AuditService) ActionStats(ctx context.Context, since time.Time) ([]repository.ActionCount, error) {
	items, err := s.repo.CountByAction(ctx, since)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, nil
}
