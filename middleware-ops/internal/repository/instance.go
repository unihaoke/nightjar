package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// InstanceFilter 是中间件实例的检索条件。
type InstanceFilter struct {
	Keyword     string
	MWType      string
	Environment string
	GroupName   string
	Status      *int16
	// EnvScope / GroupScope 为数据权限过滤（为空表示不限制）。
	EnvScope   []string
	GroupScope []string
}

// InstanceRepository 提供中间件实例的数据访问。
type InstanceRepository struct {
	Base
}

// NewInstanceRepository 构造实例仓储。
func NewInstanceRepository(db *gorm.DB) *InstanceRepository {
	return &InstanceRepository{Base: Base{db: db}}
}

// query 依据过滤条件构造查询。
func (r *InstanceRepository) query(ctx context.Context, f InstanceFilter) *gorm.DB {
	q := r.withCtx(ctx).Model(&model.MiddlewareInstance{})
	if f.Keyword != "" {
		like := "%" + strings.TrimSpace(f.Keyword) + "%"
		q = q.Where("name LIKE ? OR host LIKE ?", like, like)
	}
	if f.MWType != "" {
		q = q.Where("mw_type = ?", f.MWType)
	}
	if f.Environment != "" {
		q = q.Where("environment = ?", f.Environment)
	}
	if f.GroupName != "" {
		q = q.Where("group_name = ?", f.GroupName)
	}
	if f.Status != nil {
		q = q.Where("status = ?", *f.Status)
	}
	if len(f.EnvScope) > 0 {
		q = q.Where("environment IN ?", f.EnvScope)
	}
	if len(f.GroupScope) > 0 {
		q = q.Where("group_name IN ?", f.GroupScope)
	}
	return q
}

// List 分页检索实例。
func (r *InstanceRepository) List(ctx context.Context, f InstanceFilter, limit, offset int) ([]model.MiddlewareInstance, int64, error) {
	var total int64
	if err := r.query(ctx, f).Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count instances")
	}
	var items []model.MiddlewareInstance
	if err := r.query(ctx, f).Order("id DESC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list instances")
	}
	return items, total, nil
}

// All 返回全部实例（受数据权限约束），供大盘与健康探测使用。
func (r *InstanceRepository) All(ctx context.Context, f InstanceFilter) ([]model.MiddlewareInstance, error) {
	var items []model.MiddlewareInstance
	if err := r.query(ctx, f).Order("id ASC").Find(&items).Error; err != nil {
		return nil, wrap(err, "list all instances")
	}
	return items, nil
}

// Get 按 ID 查询实例。
func (r *InstanceRepository) Get(ctx context.Context, id int64) (*model.MiddlewareInstance, error) {
	var item model.MiddlewareInstance
	if err := r.withCtx(ctx).First(&item, id).Error; err != nil {
		return nil, wrap(err, "get instance")
	}
	return &item, nil
}

// FindByName 按名称精确查询（用于自动发现去重）。
func (r *InstanceRepository) FindByName(ctx context.Context, name string) (*model.MiddlewareInstance, error) {
	var item model.MiddlewareInstance
	if err := r.withCtx(ctx).Where("name = ?", name).First(&item).Error; err != nil {
		return nil, wrap(err, "find instance by name")
	}
	return &item, nil
}

// Create 新增实例。
func (r *InstanceRepository) Create(ctx context.Context, item *model.MiddlewareInstance) error {
	if err := r.withCtx(ctx).Create(item).Error; err != nil {
		return wrap(err, "create instance")
	}
	return nil
}

// Update 全量更新实例的可编辑字段。
func (r *InstanceRepository) Update(ctx context.Context, item *model.MiddlewareInstance) error {
	res := r.withCtx(ctx).Model(&model.MiddlewareInstance{}).
		Where("id = ?", item.ID).
		Updates(map[string]any{
			"name":               item.Name,
			"mw_type":            item.MWType,
			"host":               item.Host,
			"port":               item.Port,
			"username":           item.Username,
			"password_encrypted": item.PasswordEncrypted,
			"environment":        item.Environment,
			"group_name":         item.GroupName,
			"tags":               item.Tags,
			"config":             item.Config,
			"prom_job":           item.PromJob,
			"prom_instance":      item.PromInstance,
		})
	if res.Error != nil {
		return wrap(res.Error, "update instance")
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("update instance: %w", ErrNotFound)
	}
	return nil
}

// UpdateStatus 更新健康状态与最近探测结论。
func (r *InstanceRepository) UpdateStatus(ctx context.Context, id int64, status int16, message string) error {
	res := r.withCtx(ctx).Model(&model.MiddlewareInstance{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":        status,
			"last_message":  message,
			"last_check_at": time.Now().UTC(),
		})
	if res.Error != nil {
		return wrap(res.Error, "update instance status")
	}
	return nil
}

// Delete 删除实例。
func (r *InstanceRepository) Delete(ctx context.Context, id int64) error {
	res := r.withCtx(ctx).Delete(&model.MiddlewareInstance{}, id)
	if res.Error != nil {
		return wrap(res.Error, "delete instance")
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("delete instance: %w", ErrNotFound)
	}
	return nil
}

// CountByType 统计各中间件类型的实例数量。
func (r *InstanceRepository) CountByType(ctx context.Context, envScope, groupScope []string) (map[string]int64, error) {
	type row struct {
		MWType string
		Total  int64
	}
	q := r.withCtx(ctx).Model(&model.MiddlewareInstance{}).
		Select("mw_type, COUNT(*) AS total").
		Group("mw_type")
	if len(envScope) > 0 {
		q = q.Where("environment IN ?", envScope)
	}
	if len(groupScope) > 0 {
		q = q.Where("group_name IN ?", groupScope)
	}
	var rows []row
	if err := q.Scan(&rows).Error; err != nil {
		return nil, wrap(err, "count instances by type")
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.MWType] = item.Total
	}
	return out, nil
}

// CountStatus 统计在线/离线实例数量。
func (r *InstanceRepository) CountStatus(ctx context.Context, envScope, groupScope []string) (online, offline int64, err error) {
	q := r.withCtx(ctx).Model(&model.MiddlewareInstance{})
	if len(envScope) > 0 {
		q = q.Where("environment IN ?", envScope)
	}
	if len(groupScope) > 0 {
		q = q.Where("group_name IN ?", groupScope)
	}
	if err = q.Where("status = ?", 1).Count(&online).Error; err != nil {
		return 0, 0, wrap(err, "count online")
	}
	if err = q.Where("status = ?", 0).Count(&offline).Error; err != nil {
		return 0, 0, wrap(err, "count offline")
	}
	return online, offline, nil
}

// ListGroups 返回现有分组与环境清单（供前端筛选器）。
func (r *InstanceRepository) ListGroups(ctx context.Context) (groups []string, envs []string, err error) {
	if err = r.withCtx(ctx).Model(&model.MiddlewareInstance{}).
		Distinct().Pluck("group_name", &groups).Error; err != nil {
		return nil, nil, wrap(err, "list groups")
	}
	if err = r.withCtx(ctx).Model(&model.MiddlewareInstance{}).
		Distinct().Pluck("environment", &envs).Error; err != nil {
		return nil, nil, wrap(err, "list envs")
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if strings.TrimSpace(g) != "" {
			out = append(out, g)
		}
	}
	return out, envs, nil
}

// EnsureNotFound 判断错误是否为记录不存在。
func EnsureNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
