package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
	"middleware-ops/internal/utils"
)

// AuditFilter 是审计日志检索条件。
type AuditFilter struct {
	UserID     int64
	InstanceID int64
	ActionType string
	Level      string
	Result     string
	Keyword    string
	From       *time.Time
	To         *time.Time
}

// AuditRepository 提供审计日志数据访问。
//
// 只追加语义（6.4）：本类型只提供 Create / List / Get / Snapshot 方法，
// 刻意不提供 Update / Delete，从 API 层到数据层封堵篡改路径。
type AuditRepository struct {
	Base
}

// NewAuditRepository 构造审计仓储。
func NewAuditRepository(db *gorm.DB) *AuditRepository {
	return &AuditRepository{Base: Base{db: db}}
}

// query 构造检索查询。
func (r *AuditRepository) query(ctx context.Context, f AuditFilter) *gorm.DB {
	q := r.withCtx(ctx).Model(&model.AuditLog{})
	if f.UserID > 0 {
		q = q.Where("user_id = ?", f.UserID)
	}
	if f.InstanceID > 0 {
		q = q.Where("instance_id = ?", f.InstanceID)
	}
	if f.ActionType != "" {
		q = q.Where("action_type = ?", f.ActionType)
	}
	if f.Level != "" {
		q = q.Where("level = ?", f.Level)
	}
	if f.Result != "" {
		q = q.Where("result = ?", f.Result)
	}
	if f.Keyword != "" {
		like := "%" + f.Keyword + "%"
		q = q.Where("username LIKE ? OR route LIKE ?", like, like)
	}
	if f.From != nil {
		q = q.Where("created_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("created_at <= ?", *f.To)
	}
	return q
}

// List 分页检索审计日志。
func (r *AuditRepository) List(ctx context.Context, f AuditFilter, limit, offset int) ([]model.AuditLog, int64, error) {
	var total int64
	if err := r.query(ctx, f).Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count audit logs")
	}
	var items []model.AuditLog
	if err := r.query(ctx, f).Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list audit logs")
	}
	return items, total, nil
}

// Get 按 ID 查询审计日志。
func (r *AuditRepository) Get(ctx context.Context, id int64) (*model.AuditLog, error) {
	var item model.AuditLog
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get audit log")
	}
	return &item, nil
}

// Tail 返回最后一条审计记录，用于哈希链续接。
func (r *AuditRepository) Tail(ctx context.Context) (*model.AuditLog, error) {
	var item model.AuditLog
	if err := r.withCtx(ctx).Order("id DESC").First(&item).Error; err != nil {
		return nil, wrap(err, "tail audit log")
	}
	return &item, nil
}

// Append 追加一条审计日志，并计算哈希链。
//
// 哈希链：hash_self = SHA256(hash_prev | 规范化内容)，任一历史行被篡改都会
// 在当日快照校验中被检出（6.4）。
func (r *AuditRepository) Append(ctx context.Context, entry *model.AuditLog) error {
	tail, err := r.Tail(ctx)
	prev := ""
	switch {
	case err == nil && tail != nil:
		prev = tail.HashSelf
	case err != nil && !EnsureNotFound(err):
		return err
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	entry.HashPrev = prev
	entry.HashSelf = utils.HashChain(prev, canonicalAudit(entry))
	if err := r.withCtx(ctx).Create(entry).Error; err != nil {
		return wrap(err, "append audit log")
	}
	return nil
}

// canonicalAudit 生成用于哈希的规范化内容。
func canonicalAudit(entry *model.AuditLog) string {
	detail := ""
	if entry.ActionDetail != nil {
		if b, err := json.Marshal(entry.ActionDetail); err == nil {
			detail = string(b)
		}
	}
	return fmt.Sprintf("%d|%d|%s|%d|%s|%s|%s|%s|%s|%s",
		entry.ID, entry.UserID, entry.Username, entry.InstanceID, entry.ActionType,
		detail, entry.Result, entry.Level, entry.IPAddress, entry.CreatedAt.UTC().Format(time.RFC3339Nano))
}

// VerifyChain 校验指定区间内的哈希链完整性，返回首个断链的日志 ID。
func (r *AuditRepository) VerifyChain(ctx context.Context, fromID, toID int64) (ok bool, brokenID int64, err error) {
	query := r.withCtx(ctx).Model(&model.AuditLog{})
	if fromID > 0 {
		query = query.Where("id >= ?", fromID)
	}
	if toID > 0 {
		query = query.Where("id <= ?", toID)
	}
	var items []model.AuditLog
	if err = query.Order("id ASC").Find(&items).Error; err != nil {
		return false, 0, wrap(err, "verify chain query")
	}
	prev := ""
	if fromID > 0 {
		// 区间起点之前的最后一条记录提供 prev 锚点。
		var anchor model.AuditLog
		if anchorErr := r.withCtx(ctx).Where("id < ?", fromID).Order("id DESC").First(&anchor).Error; anchorErr == nil {
			prev = anchor.HashSelf
		}
	}
	for i := range items {
		item := items[i]
		expected := utils.HashChain(prev, canonicalAudit(&item))
		if expected != item.HashSelf {
			return false, item.ID, nil
		}
		prev = item.HashSelf
	}
	return true, 0, nil
}

// CreateSnapshot 生成每日哈希链快照（6.4）。
//
// 落盘为 JSON 文件，并记录链尾哈希；周期校验任务比对链尾哈希是否变化。
func (r *AuditRepository) CreateSnapshot(ctx context.Context, dir string) (*model.AuditSnapshot, error) {
	date := utils.DayKey(time.Now())
	tail, err := r.Tail(ctx)
	if err != nil && !EnsureNotFound(err) {
		return nil, err
	}
	var count int64
	if err := r.withCtx(ctx).Model(&model.AuditLog{}).Count(&count).Error; err != nil {
		return nil, wrap(err, "count audit logs")
	}
	snapshot := &model.AuditSnapshot{SnapshotDate: date, LogCount: count}
	if tail != nil {
		snapshot.LastLogID = tail.ID
		snapshot.ChainHash = tail.HashSelf
	}
	ok, brokenID, verifyErr := r.VerifyChain(ctx, 0, 0)
	if verifyErr != nil {
		return nil, verifyErr
	}
	snapshot.Verified = ok
	now := time.Now().UTC()
	snapshot.VerifiedAt = &now
	if !ok {
		snapshot.ChainHash = fmt.Sprintf("BROKEN@%d", brokenID)
	} else if dir != "" {
		path := filepath.Join(dir, fmt.Sprintf("audit-snapshot-%s.json", date))
		payload := map[string]any{
			"date":        date,
			"last_log_id": snapshot.LastLogID,
			"log_count":   snapshot.LogCount,
			"chain_hash":  snapshot.ChainHash,
			"verified":    snapshot.Verified,
			"created_at":  now.Format(time.RFC3339),
		}
		if b, marshalErr := json.MarshalIndent(payload, "", "  "); marshalErr == nil {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr == nil {
				if writeErr := os.WriteFile(path, b, 0o600); writeErr == nil {
					snapshot.FilePath = path
				}
			}
		}
	}

	// 同日重复执行时更新既有快照，保持幂等。
	var existing model.AuditSnapshot
	err = r.withCtx(ctx).Where("snapshot_date = ?", date).First(&existing).Error
	if err == nil {
		if updateErr := r.withCtx(ctx).Model(&model.AuditSnapshot{}).Where("id = ?", existing.ID).
			Updates(map[string]any{
				"last_log_id": snapshot.LastLogID,
				"log_count":   snapshot.LogCount,
				"chain_hash":  snapshot.ChainHash,
				"file_path":   snapshot.FilePath,
				"verified":    snapshot.Verified,
				"verified_at": snapshot.VerifiedAt,
			}).Error; updateErr != nil {
			return nil, wrap(updateErr, "update audit snapshot")
		}
		snapshot.ID = existing.ID
		return snapshot, nil
	}
	if !EnsureNotFound(err) {
		return nil, err
	}
	if createErr := r.withCtx(ctx).Create(snapshot).Error; createErr != nil {
		return nil, wrap(createErr, "create audit snapshot")
	}
	return snapshot, nil
}

// ListSnapshots 返回最近的快照列表。
func (r *AuditRepository) ListSnapshots(ctx context.Context, limit int) ([]model.AuditSnapshot, error) {
	var items []model.AuditSnapshot
	if err := r.withCtx(ctx).Order("snapshot_date DESC").Limit(limit).Find(&items).Error; err != nil {
		return nil, wrap(err, "list audit snapshots")
	}
	return items, nil
}

// CountByAction 统计各操作类型数量（大盘使用）。
func (r *AuditRepository) CountByAction(ctx context.Context, since time.Time) ([]ActionCount, error) {
	var items []ActionCount
	if err := r.withCtx(ctx).Model(&model.AuditLog{}).
		Select("action_type AS action_type, COUNT(*) AS total").
		Where("created_at >= ?", since).
		Group("action_type").Order("total DESC").Limit(10).
		Scan(&items).Error; err != nil {
		return nil, wrap(err, "count audit by action")
	}
	return items, nil
}

// ActionCount 是操作类型统计项。
type ActionCount struct {
	ActionType string `json:"action_type"`
	Total      int64  `json:"total"`
}
