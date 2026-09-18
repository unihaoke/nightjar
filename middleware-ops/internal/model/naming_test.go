package model

import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

// 本文件的测试固化 INC-019 的教训：**表名不能靠直觉，也不能只信文档**。
//
// 出事链条：用量接口按"直觉表名"手写 SQL（ai_diagnoses），但 GORM 对 AIDiagnosis 实际
// 生成的是 a_idiagnoses（AI 里的 ID 被当成常见缩写改写后再切词）→ 线上接口 500
// SQLSTATE 42P01；而 docs/SCHEMA.sql 里同样写着 ai_diagnoses，于是"文档核对"这一步
// 也拦不住它。同一处直觉还出现在 vector_*.go 的建索引/ALTER 语句里
// （knowledge_base、ai_diagnoses），默认构建下静默失败、pgvector 构建下直接启动失败。

// TestTableNameOfKnownTraps 钉住几个"直觉会写错"的真实表名。
func TestTableNameOfKnownTraps(t *testing.T) {
	cases := []struct {
		entity any
		want   string
		why    string
	}{
		{&AIDiagnosis{}, "a_idiagnoses", "AI 里的 ID 被当作常见缩写改写（AI|Diagnosis → A_Idiagnosis）"},
		{&AICodeAnalysis{}, "ai_code_analyses", "AI 后接 Code，不含常见缩写，正常切词"},
		{&KnowledgeBase{}, "knowledge_bases", "复数化，不是设计文档里的 knowledge_base"},
		{&PlatformSetting{}, "platform_settings", "显式 TableName"},
		{&AuditLog{}, "audit_logs", "显式 TableName"},
		{&MiddlewareInstance{}, "middleware_instances", "显式 TableName"},
	}
	for _, c := range cases {
		got := TableNameOf(c.entity)
		if got != c.want {
			t.Fatalf("%T 的真实表名 = %q，期望 %q（%s）。"+
				"若确实要换名，必须同时改 AutoMigrate 目标与所有手写 SQL，并评估存量数据迁移",
				c.entity, got, c.want, c.why)
		}
	}
	// 反向断言：反直觉的名字不得出现（这正是线上 500 的那一个）。
	if TableNameOf(&AIDiagnosis{}) == "ai_diagnoses" {
		t.Fatal("AIDiagnosis 的表名不应是 ai_diagnoses——线上 42P01 就是照直觉写 SQL 造成的")
	}
}

// TestTableNameOfCoversMigrationList 保证迁移清单里的每个实体都能解析出表名：
// 解析失败会让用到 TableNameOf 的 SQL/DDL 静默缺表，必须是硬失败。
func TestTableNameOfCoversMigrationList(t *testing.T) {
	for _, entity := range MigrationList() {
		if got := TableNameOf(entity); got == "" {
			t.Fatalf("实体 %T 解析不出表名", entity)
		}
	}
}

// TestSchemaReferenceTableNamesMatchModels 校验参考 schema 的表名与 GORM 实际生成的一致。
//
// 为什么值得单独守一条：本次故障能漏到线上，正因为"按 docs/SCHEMA.sql 核对"时
// 文档本身写的是错的名字。文档与实现必须一起被锁住。
func TestSchemaReferenceTableNamesMatchModels(t *testing.T) {
	content, ok := readSchemaReference(t)
	if !ok {
		t.Skip("未找到 docs/SCHEMA.sql，跳过参考 schema 与模型的表名一致性校验")
	}

	re := regexp.MustCompile(`(?im)^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-z_][a-z0-9_]*)`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(content, -1) {
		found[strings.ToLower(m[1])] = true
	}
	if len(found) == 0 {
		t.Fatal("未从 docs/SCHEMA.sql 解析到任何 CREATE TABLE，测试前置条件失效")
	}

	expected := map[string]bool{}
	for _, entity := range MigrationList() {
		expected[TableNameOf(entity)] = true
	}

	var missing, extra []string
	for name := range expected {
		if !found[name] {
			missing = append(missing, name)
		}
	}
	for name := range found {
		if !expected[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("docs/SCHEMA.sql 与模型表名不一致：模型有而文档缺 %v；文档有而模型无 %v。"+
			"（提示：AIDiagnosis→a_idiagnoses、KnowledgeBase→knowledge_bases 是 GORM 命名策略的结果）",
			missing, extra)
	}
}

// TestTablesAndColumnsUsedByRawSQLExist 校验手写 SQL / DDL 里用到的表与列确实存在于模型中。
//
// 覆盖两处手写 SQL：
//  1. 用量统计（service/setting.go）：a_idiagnoses / ai_code_analyses 的
//     created_at、cost_tokens、user_id，以及 users 的 id、username；
//  2. 启动期补索引与 pgvector 升级（internal/db/vector_*.go）：
//     alerts(instance_id, triggered_at)、a_idiagnoses(instance_id)、audit_logs(user_id,
//     instance_id)、log_alert_events(error_signature, server_id, last_seen_at)、
//     knowledge_bases/alert_embeddings(embedding)。
//
// 列名写错与表名写错的表现一样（42P01 / 42703），所以一起守住。
func TestTablesAndColumnsUsedByRawSQLExist(t *testing.T) {
	cache := &sync.Map{}
	parser := schema.NamingStrategy{}

	columns := func(entity any) map[string]bool {
		parsed, err := schema.Parse(entity, cache, parser)
		if err != nil {
			t.Fatalf("解析 %T 失败: %v", entity, err)
		}
		out := map[string]bool{}
		for _, field := range parsed.Fields {
			out[field.DBName] = true
		}
		return out
	}

	cases := []struct {
		entity any
		need   []string
	}{
		{&AIDiagnosis{}, []string{"created_at", "cost_tokens", "user_id", "instance_id"}},
		{&AICodeAnalysis{}, []string{"created_at", "cost_tokens"}},
		{&User{}, []string{"id", "username"}},
		{&Alert{}, []string{"instance_id", "triggered_at"}},
		{&AuditLog{}, []string{"user_id", "instance_id", "created_at"}},
		{&LogAlertEvent{}, []string{"error_signature", "server_id", "last_seen_at"}},
		{&KnowledgeBase{}, []string{"embedding", "status"}},
		{&AlertEmbedding{}, []string{"embedding"}},
	}
	for _, c := range cases {
		have := columns(c.entity)
		for _, column := range c.need {
			if !have[column] {
				t.Fatalf("手写 SQL/DDL 引用了 %s.%s，但模型里没有这一列（可用列：%v）",
					TableNameOf(c.entity), column, sortedKeys(have))
			}
		}
	}
}

// sortedKeys 便于失败信息稳定可读。
func sortedKeys(in map[string]bool) []string {
	out := make([]string, 0, len(in))
	for key := range in {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
