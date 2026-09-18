package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"middleware-ops/internal/model"
)

// SettingRepository 提供「平台自管设置」的数据访问（platform_settings 表）。
//
// 背景：AI 提供方密钥与通知渠道以前只能写在 .env / configs/config.yaml 里，改一个 webhook
// 地址就要改环境变量、重建容器。搬到平台后由界面管理，本仓储只负责**密文**的读写——
// 明文 JSON 由 service 层用 AES-256-GCM 加密后落库（与实例口令同一套做法）。
type SettingRepository struct {
	Base
}

// NewSettingRepository 构造平台设置仓储。
func NewSettingRepository(db *gorm.DB) *SettingRepository {
	return &SettingRepository{Base: Base{db: db}}
}

// Get 按 name 读取设置项。
//
// 找不到时返回 (nil, nil) 而不是 ErrNotFound：DB 里还没有 ai / notify 记录是**首次启动的
// 正常状态**（此时 service 层回退到 .env 配置），不是异常。若这里返回 not found，调用方
// 每处都得先判一次「是真缺失还是真出错」，反而更容易把首次启动误判成故障。
func (r *SettingRepository) Get(ctx context.Context, name string) (*model.PlatformSetting, error) {
	var item model.PlatformSetting
	err := r.withCtx(ctx).Where("name = ?", name).First(&item).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, wrap(err, "get platform setting")
	}
	return &item, nil
}

// Upsert 按 name 写入设置项：存在则原地更新，不存在则创建。
//
// 用 ON CONFLICT 而不是「先查再写」：name 上有唯一索引，两个管理员同时保存时后者会撞唯一键，
// 先查再写在并发下必然偶发唯一键冲突；交给数据库做 UPSERT 则天然幂等，也不需要额外事务。
func (r *SettingRepository) Upsert(ctx context.Context, name, encryptedPayload, updatedBy string) error {
	item := &model.PlatformSetting{
		Name:             name,
		PayloadEncrypted: encryptedPayload,
		UpdatedBy:        updatedBy,
	}
	// updated_at 取 EXCLUDED.updated_at：GORM 在 Create 时自动把 updated_at 填为当前时间，
	// 冲突分支引用插入行里的同一值，语义与"本次保存时间"完全一致（不必自己再取一次 now）。
	return wrap(r.withCtx(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "name"}},
		DoUpdates: clause.Assignments(map[string]any{
			"payload_encrypted": encryptedPayload,
			"updated_by":        updatedBy,
			"updated_at":        gorm.Expr("EXCLUDED.updated_at"),
		}),
	}).Create(item).Error, "upsert platform setting")
}
