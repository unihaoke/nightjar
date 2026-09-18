package model

import (
	"reflect"
	"sync"

	"gorm.io/gorm/schema"
)

// tableNameCache 缓存「实体 → 真实表名」：解析走反射，开销不小，而结果恒定不变。
var tableNameCache sync.Map

// TableNameOf 返回实体在当前 GORM 命名策略下的**真实表名**。
//
// 为什么必须用它而不是写字面量（INC-019）：GORM 的 toDBName 会先把常见缩写改写
// （ID → Id）再按大小写切词，于是：
//
//	AIDiagnosis  → a_idiagnoses   （不是直觉上的 ai_diagnoses）
//	KnowledgeBase → knowledge_bases（不是设计文档里的 knowledge_base）
//
// 手写 SQL / DDL 时按直觉写表名，就会在运行期报 SQLSTATE 42P01
// relation "..." does not exist（用量接口 500），或者让建索引语句永远失败。
// 凡是要在 SQL 里出现表名的地方（含 CREATE INDEX、ALTER TABLE），一律用本函数取名字。
//
// 返回空串表示解析失败：调用方必须显式处理（报错或警告），不要当成"没问题"继续执行。
func TableNameOf(entity any) string {
	if entity == nil {
		return ""
	}
	key := reflect.TypeOf(entity).String()
	if cached, ok := tableNameCache.Load(key); ok {
		return cached.(string)
	}
	parsed, err := schema.Parse(entity, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		return ""
	}
	tableNameCache.Store(key, parsed.Table)
	return parsed.Table
}
