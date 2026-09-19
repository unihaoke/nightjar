package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"middleware-ops/internal/logpipe"
	"middleware-ops/internal/model"
)

// KafkaConsumeStatRepository 持久化日志消费链路的累计计数（按 topic + 消费组一行）。
//
// 它是 logpipe.Storer 的实现：消费者只认"读历史 + 加增量"两个动作，
// 不关心底下是 PostgreSQL 还是别的存储。
type KafkaConsumeStatRepository struct {
	Base
}

// NewKafkaConsumeStatRepository 构造消费计数仓储。
func NewKafkaConsumeStatRepository(db *gorm.DB) *KafkaConsumeStatRepository {
	return &KafkaConsumeStatRepository{Base: Base{db: db}}
}

// Load 读取历史累计；记录不存在时返回全 0（新 topic / 新消费组属于正常路径）。
func (r *KafkaConsumeStatRepository) Load(ctx context.Context, topic, group string) (logpipe.Stats, error) {
	var item model.KafkaConsumeStat
	err := r.withCtx(ctx).Where("topic = ? AND group_id = ?", topic, group).First(&item).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return logpipe.Stats{}, nil
		}
		return logpipe.Stats{}, wrap(err, "load kafka consume stat")
	}
	return logpipe.Stats{
		Consumed: item.Consumed,
		Ingested: item.Ingested,
		Ignored:  item.Ignored,
		Dropped:  item.Dropped,
		Failed:   item.Failed,
	}, nil
}

// Add 把增量累加进历史值。
//
// 用 SQL 层的自增（`consumed + ?`）而不是"读出 → 加完 → 写回"：
// 平台多副本时同一 topic+group 会被多个实例同时加，读改写会互相覆盖丢计数。
func (r *KafkaConsumeStatRepository) Add(ctx context.Context, topic, group string, delta logpipe.Stats) error {
	item := model.KafkaConsumeStat{
		Topic: topic, GroupID: group,
		Consumed: delta.Consumed, Ingested: delta.Ingested, Ignored: delta.Ignored,
		Dropped: delta.Dropped, Failed: delta.Failed,
	}
	updates := map[string]any{
		"consumed": gorm.Expr("consumed + ?", delta.Consumed),
		"ingested": gorm.Expr("ingested + ?", delta.Ingested),
		"ignored":  gorm.Expr("ignored + ?", delta.Ignored),
		"dropped":  gorm.Expr("dropped + ?", delta.Dropped),
		"failed":   gorm.Expr("failed + ?", delta.Failed),
	}
	// 先尝试更新；没有这一行时再插入（并发下插入可能撞唯一性，此时回退到更新）。
	res := r.withCtx(ctx).Where("topic = ? AND group_id = ?", topic, group).
		Model(&model.KafkaConsumeStat{}).Updates(updates)
	if res.Error != nil {
		return wrap(res.Error, "increase kafka consume stat")
	}
	if res.RowsAffected > 0 {
		return nil
	}
	if err := r.withCtx(ctx).Create(&item).Error; err != nil {
		// 并发场景下另一副本可能刚插进来：再更新一次即可，不必让调用方感知。
		if res2 := r.withCtx(ctx).Where("topic = ? AND group_id = ?", topic, group).
			Model(&model.KafkaConsumeStat{}).Updates(updates); res2.Error == nil && res2.RowsAffected > 0 {
			return nil
		}
		return wrap(err, "create kafka consume stat")
	}
	return nil
}
