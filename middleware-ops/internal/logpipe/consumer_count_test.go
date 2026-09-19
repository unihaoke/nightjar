package logpipe

import (
	"context"
	"testing"
)

// memStore 是 Storer 的内存实现（顺手充当"数据库"：两个消费者实例共享它就等于共享累计值）。
type memStore struct {
	total Stats
	loads int
	adds  int
}

func (m *memStore) Load(context.Context, string, string) (Stats, error) {
	m.loads++
	return m.total, nil
}

func (m *memStore) Add(_ context.Context, _, _ string, delta Stats) error {
	m.adds++
	m.total.Consumed += delta.Consumed
	m.total.Ingested += delta.Ingested
	m.total.Ignored += delta.Ignored
	m.total.Dropped += delta.Dropped
	m.total.Failed += delta.Failed
	return nil
}

// TestCountingSplitsIngestedAndIgnored 钉住计数口径：
// "已消费"必须能拆成"入库"与"有意忽略"，否则页面上的差值无从解释。
func TestCountingSplitsIngestedAndIgnored(t *testing.T) {
	c := &Consumer{opts: Options{Topic: "mwops-logs", GroupID: "g"}}

	c.count(Stats{Consumed: 1, Ingested: 1}) // 正常入库
	c.count(Stats{Consumed: 1, Ignored: 1})  // 未命中规则 / 命中屏蔽项
	c.count(Stats{Dropped: 1})               // 脏消息
	c.count(Stats{Failed: 1})                // 入库失败

	got := c.Status()
	if got.Consumed != 2 || got.Ingested != 1 || got.Ignored != 1 {
		t.Fatalf("口径错误：consumed=%d ingested=%d ignored=%d（应满足 consumed = ingested + ignored）",
			got.Consumed, got.Ingested, got.Ignored)
	}
	if got.Dropped != 1 || got.Failed != 1 {
		t.Fatalf("dropped/failed 计数错误：%d / %d", got.Dropped, got.Failed)
	}
	// 丢弃与失败不算"已消费"：它们没有真正进入平台（consumed 应只有 2，而不是 4）。
	if got.Consumed != 2 || got.ConsumedTotal != 2 {
		t.Fatalf("dropped/failed 不应计入 consumed：consumed=%d total=%d", got.Consumed, got.ConsumedTotal)
	}
}

// TestFlushPersistsAcrossRestarts 钉住"累计不随重启归零"——这正是"消费条数对不上"的主因。
func TestFlushPersistsAcrossRestarts(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}

	first := &Consumer{opts: Options{Topic: "t", GroupID: "g", Store: store}}
	first.loadBase(ctx)
	first.count(Stats{Consumed: 3, Ingested: 2, Ignored: 1})
	first.flush(ctx)
	if store.total.Consumed != 3 {
		t.Fatalf("flush 后累计应写入存储，实际 %+v", store.total)
	}

	// 模拟重启：新实例读回历史累计，会话计数从 0 开始，但总量接得上。
	second := &Consumer{opts: Options{Topic: "t", GroupID: "g", Store: store}}
	second.loadBase(ctx)
	second.count(Stats{Consumed: 2, Ingested: 2})
	got := second.Status()
	if got.Consumed != 2 {
		t.Fatalf("会话计数应为本次的 2，实际 %d", got.Consumed)
	}
	if got.ConsumedTotal != 5 {
		t.Fatalf("累计应为 3 + 2 = 5，实际 %d（累计断掉就是页面数字对不上的根因）", got.ConsumedTotal)
	}
	if !got.Persistent {
		t.Fatal("配置了 Store 时应标记 Persistent，页面据此说明口径")
	}
}

// TestFlushRestoresPendingOnFailure 落库失败时增量必须还回去，否则计数永久丢失。
func TestFlushRestoresPendingOnFailure(t *testing.T) {
	store := &failingStore{}
	c := &Consumer{opts: Options{Topic: "t", GroupID: "g", Store: store}}
	c.count(Stats{Consumed: 4})
	c.flush(context.Background())

	// 第一次失败：增量回到 pending，状态里仍能看到这条（不能凭空消失）。
	if got := c.Status().Consumed; got != 4 {
		t.Fatalf("落库失败后会话计数不应丢失，实际 %d", got)
	}
	if c.pending.consumed.Load() != 4 {
		t.Fatalf("落库失败后增量应留在 pending 待重试，实际 %d", c.pending.consumed.Load())
	}
}

// failingStore 总是写失败（模拟数据库抖动）。
type failingStore struct{}

func (failingStore) Load(context.Context, string, string) (Stats, error) { return Stats{}, nil }
func (failingStore) Add(context.Context, string, string, Stats) error     { return context.DeadlineExceeded }

// TestStatusWithoutStoreNoPersistence 未配置存储时如实说明"仅本次启动以来"。
func TestStatusWithoutStoreNoPersistence(t *testing.T) {
	c := &Consumer{opts: Options{Topic: "t", GroupID: "g"}}
	c.count(Stats{Consumed: 7})
	got := c.Status()
	if got.Persistent {
		t.Fatal("未配置 Store 时不应标记为可持久化")
	}
	if got.ConsumedTotal != 7 {
		t.Fatalf("未落库时累计等于本次计数，实际 %d", got.ConsumedTotal)
	}
}
