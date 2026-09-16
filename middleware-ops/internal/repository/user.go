package repository

import (
	"context"
	"strings"
	"time"

	"gorm.io/gorm"

	"middleware-ops/internal/model"
)

// UserRepository 提供用户与角色的数据访问。
type UserRepository struct {
	Base
}

// NewUserRepository 构造用户仓储。
func NewUserRepository(db *gorm.DB) *UserRepository {
	return &UserRepository{Base: Base{db: db}}
}

// GetByUsername 按用户名查询。
func (r *UserRepository) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	var user model.User
	if err := r.withCtx(ctx).Where("username = ?", strings.TrimSpace(username)).First(&user).Error; err != nil {
		return nil, wrap(err, "get user by username")
	}
	return &user, nil
}

// Get 按 ID 查询用户。
func (r *UserRepository) Get(ctx context.Context, id int64) (*model.User, error) {
	var user model.User
	if err := r.withCtx(ctx).First(&user, id).Error; err != nil {
		return nil, wrap(err, "get user")
	}
	return &user, nil
}

// List 分页检索用户。
func (r *UserRepository) List(ctx context.Context, keyword string, limit, offset int) ([]model.User, int64, error) {
	q := r.withCtx(ctx).Model(&model.User{})
	if keyword != "" {
		like := "%" + keyword + "%"
		q = q.Where("username LIKE ? OR nickname LIKE ? OR email LIKE ?", like, like, like)
	}
	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, wrap(err, "count users")
	}
	var items []model.User
	if err := q.Order("id ASC").Limit(limit).Offset(offset).Find(&items).Error; err != nil {
		return nil, 0, wrap(err, "list users")
	}
	return items, total, nil
}

// Create 新增用户。
func (r *UserRepository) Create(ctx context.Context, user *model.User) error {
	if err := r.withCtx(ctx).Create(user).Error; err != nil {
		return wrap(err, "create user")
	}
	return nil
}

// Update 更新用户资料与权限范围。
func (r *UserRepository) Update(ctx context.Context, user *model.User) error {
	if err := r.withCtx(ctx).Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]any{
			"nickname":    user.Nickname,
			"email":       user.Email,
			"role_code":   user.RoleCode,
			"env_scope":   user.EnvScope,
			"group_scope": user.GroupScope,
			"status":      user.Status,
		}).Error; err != nil {
		return wrap(err, "update user")
	}
	return nil
}

// UpdatePassword 更新密码哈希。
func (r *UserRepository) UpdatePassword(ctx context.Context, id int64, hash string) error {
	if err := r.withCtx(ctx).Model(&model.User{}).Where("id = ?", id).
		Update("password_hash", hash).Error; err != nil {
		return wrap(err, "update password")
	}
	return nil
}

// TouchLogin 记录最近登录时间。
func (r *UserRepository) TouchLogin(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	if err := r.withCtx(ctx).Model(&model.User{}).Where("id = ?", id).
		Update("last_login", now).Error; err != nil {
		return wrap(err, "touch login")
	}
	return nil
}

// Delete 删除用户。
func (r *UserRepository) Delete(ctx context.Context, id int64) error {
	if err := r.withCtx(ctx).Delete(&model.User{}, id).Error; err != nil {
		return wrap(err, "delete user")
	}
	return nil
}

// CountByRole 统计角色下用户数（用于角色列表）。
func (r *UserRepository) CountByRole(ctx context.Context) (map[string]int64, error) {
	type row struct {
		RoleCode string
		Total    int64
	}
	var rows []row
	if err := r.withCtx(ctx).Model(&model.User{}).
		Select("role_code, COUNT(*) AS total").Group("role_code").Scan(&rows).Error; err != nil {
		return nil, wrap(err, "count users by role")
	}
	out := make(map[string]int64, len(rows))
	for _, item := range rows {
		out[item.RoleCode] = item.Total
	}
	return out, nil
}

// RoleRepository 提供角色数据访问。
type RoleRepository struct {
	Base
}

// NewRoleRepository 构造角色仓储。
func NewRoleRepository(db *gorm.DB) *RoleRepository {
	return &RoleRepository{Base: Base{db: db}}
}

// List 返回全部角色。
func (r *RoleRepository) List(ctx context.Context) ([]model.Role, error) {
	var items []model.Role
	if err := r.withCtx(ctx).Order("id ASC").Find(&items).Error; err != nil {
		return nil, wrap(err, "list roles")
	}
	return items, nil
}

// GetByCode 按角色码查询。
func (r *RoleRepository) GetByCode(ctx context.Context, code string) (*model.Role, error) {
	var role model.Role
	if err := r.withCtx(ctx).Where("code = ?", code).First(&role).Error; err != nil {
		return nil, wrap(err, "get role")
	}
	return &role, nil
}

// Create 新增角色。
func (r *RoleRepository) Create(ctx context.Context, role *model.Role) error {
	if err := r.withCtx(ctx).Create(role).Error; err != nil {
		return wrap(err, "create role")
	}
	return nil
}

// Update 更新角色权限点。
func (r *RoleRepository) Update(ctx context.Context, role *model.Role) error {
	if err := r.withCtx(ctx).Model(&model.Role{}).Where("id = ?", role.ID).
		Updates(map[string]any{
			"name":        role.Name,
			"description": role.Description,
			"permissions": role.Permissions,
			"levels":      role.Levels,
		}).Error; err != nil {
		return wrap(err, "update role")
	}
	return nil
}
