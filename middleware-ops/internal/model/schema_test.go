package model

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

// 本文件的测试目标是固化一条曾导致部署启动失败的不变量。
//
// 事实（已核对 GORM v1.25.x 源码）：
//   - 模型使用 `uniqueIndex` 标签时，AutoMigrate 通过 NamingStrategy.IndexName
//     创建「唯一索引」，名字形如 idx_<表>_<列>；
//   - 但迁移「列唯一性」时会用 NamingStrategy.UniqueName 生成 uni_<表>_<列>，
//     在 migrateColumnUnique 中执行 DropConstraint/CreateConstraint；
//   - 若数据库里的唯一约束来自非 GORM 来源（例如初始化脚本的内联 UNIQUE），
//     PostgreSQL 会把它命名为 <表>_<列>_key，于是 DropConstraint("uni_<表>_<列>")
//     报 SQLSTATE 42704（constraint does not exist），后端启动失败。
//
// 故障复现路径：deploy/postgres/init 下的脚本先建了表。修复方式是初始化脚本不建表，
// 表结构统一由 GORM 负责；本测试同时锁定：
//  1. 模型唯一索引命名必须等于 idx_<表>_<列>（防止模型被改坏导致迁移行为漂移）；
//  2. 模型唯一索引与 GORM 迁移期期望的 uni_<表>_<列> 必须能一一对应（迁移的硬约束）；
//  3. docs/SCHEMA.sql 参考实现必须给出 GORM 期望的 uni_<表>_<列> 约束名，
//     且不得残留未命名的内联 UNIQUE。

// expectedUniqueIndexes 是模型当前的唯一索引全集（表名 → 列名）。
var expectedUniqueIndexes = map[string]string{
	"users":            "username",
	"roles":            "code",
	"approvals":        "ticket_id",
	"log_alert_events": "event_id",
	"audit_snapshots":  "snapshot_date",
}

// TestUniqueIndexNamingMatchesGormStrategy 校验模型生成的唯一索引命名。
func TestUniqueIndexNamingMatchesGormStrategy(t *testing.T) {
	parser := schema.NamingStrategy{}
	actual := collectUniqueIndexes(t, parser)

	if len(actual) != len(expectedUniqueIndexes) {
		t.Fatalf("唯一索引数量变化：期望 %d 个，实际 %d 个（%v）",
			len(expectedUniqueIndexes), len(actual), actual)
	}
	for table, column := range expectedUniqueIndexes {
		// 迁移期 GORM 使用的约束名（UniqueName）必须能在模型侧找到对应列。
		uniqueName := parser.UniqueName(table, column)
		wantUnique := "uni_" + table + "_" + column
		if uniqueName != wantUnique {
			t.Fatalf("GORM UniqueName 行为变化：%s != %s", uniqueName, wantUnique)
		}
		// 建索引期使用的名字（IndexName）。
		indexName := parser.IndexName(table, column)
		wantIndex := "idx_" + table + "_" + column
		if indexName != wantIndex {
			t.Fatalf("GORM IndexName 行为变化：%s != %s", indexName, wantIndex)
		}
		got, ok := actual[indexName]
		if !ok {
			t.Fatalf("模型缺少唯一索引 %s（实际：%v）", indexName, actual)
		}
		if got.table != table || got.column != column {
			t.Fatalf("唯一索引 %s 定义不符：table=%s column=%s", indexName, got.table, got.column)
		}
	}

	// 反例校验：PostgreSQL 默认命名形式不得出现在模型输出中。
	for name := range actual {
		if strings.HasSuffix(name, "_key") || strings.HasSuffix(name, "_pkey") {
			t.Fatalf("检测到 PostgreSQL 默认约束名 %s：会与 AutoMigrate 期望的命名冲突", name)
		}
	}
}

// constraintRef 描述一个唯一约束/索引的归属。
type constraintRef struct {
	table  string
	column string
}

// collectUniqueIndexes 用 GORM schema 解析全部实体，收集唯一索引。
func collectUniqueIndexes(t *testing.T, parser schema.NamingStrategy) map[string]constraintRef {
	t.Helper()
	cache := &sync.Map{}
	out := make(map[string]constraintRef)
	for _, entity := range MigrationList() {
		parsed, err := schema.Parse(entity, cache, parser)
		if err != nil {
			t.Fatalf("解析实体失败（%T）: %v", entity, err)
		}
		for name, index := range parsed.ParseIndexes() {
			if index.Class != "UNIQUE" {
				continue
			}
			column := ""
			if len(index.Fields) == 1 && index.Fields[0].Field != nil {
				column = index.Fields[0].DBName
			}
			out[name] = constraintRef{table: parsed.Table, column: column}
			if column != "" {
				want := parser.IndexName(parsed.Table, column)
				if name != want {
					t.Fatalf("唯一索引命名不符合预期：表 %s 列 %s 生成 %s，期望 %s",
						parsed.Table, column, name, want)
				}
			}
		}
	}
	return out
}

// TestSchemaReferenceMatchesModels 校验 docs/SCHEMA.sql 与模型一致。
//
// 该文件是 DBA 手工建库的参考实现；若它与模型漂移，手工建库后 AutoMigrate 会再次失败
// （迁移期期望 uni_<表>_<列>），因此必须纳入回归。
func TestSchemaReferenceMatchesModels(t *testing.T) {
	content, ok := readSchemaReference(t)
	if !ok {
		t.Skip("跳过：未找到 docs/SCHEMA.sql")
	}
	// 先剥离行注释，避免注释中的示例文本（例如文档里说明约束命名的示例）被误判为实际 DDL。
	code := stripSQLLineComments(content)

	// 参考 schema 中不得残留「未命名」的内联 UNIQUE：
	// 形如 `<列名> <类型> ... UNIQUE` 会让 PostgreSQL 生成 <表>_<列>_key 默认约束名，
	// 与 AutoMigrate 迁移期期望的 uni_<表>_<列> 冲突。
	// 注意不能简单匹配 "UNIQUE("，否则具名约束 CONSTRAINT uni_x UNIQUE (col) 会被误判。
	inlineUnique := regexp.MustCompile(
		`(?i)\b[A-Za-z_][A-Za-z0-9_]*\s+(VARCHAR|CHARACTER|TEXT|BIGINT|INTEGER|SMALLINT|BOOLEAN|DOUBLE|TIMESTAMPTZ|TIMESTAMP|VECTOR|JSONB|NUMERIC)(\s*\([0-9, ]*\))?[^,\n]*\bUNIQUE\b`)
	if match := inlineUnique.FindString(code); match != "" {
		t.Fatalf("docs/SCHEMA.sql 存在未命名内联 UNIQUE 约束（%q），会产生 *_key 默认名并与 AutoMigrate 冲突", strings.TrimSpace(match))
	}

	// 匹配形如：CONSTRAINT uni_users_username UNIQUE (username)
	constraintPattern := regexp.MustCompile(`(?i)CONSTRAINT\s+(uni_[a-z0-9_]+)\s+UNIQUE\s*\(([^)]+)\)`)
	tablePattern := regexp.MustCompile(`(?is)CREATE TABLE IF NOT EXISTS\s+([a-z0-9_]+)\s*\((.*?)\n\);`)

	found := make(map[string]constraintRef)
	for _, tableMatch := range tablePattern.FindAllStringSubmatch(code, -1) {
		table := tableMatch[1]
		body := tableMatch[2]
		for _, constraint := range constraintPattern.FindAllStringSubmatch(body, -1) {
			found[constraint[1]] = constraintRef{
				table:  table,
				column: strings.TrimSpace(constraint[2]),
			}
		}
	}
	if len(found) != len(expectedUniqueIndexes) {
		t.Fatalf("参考 schema 的唯一约束数量与模型不一致：schema=%d 模型=%d（%v）",
			len(found), len(expectedUniqueIndexes), found)
	}
	for table, column := range expectedUniqueIndexes {
		want := "uni_" + table + "_" + column
		got, ok := found[want]
		if !ok {
			t.Fatalf("参考 schema 缺少约束 %s（实际：%v）", want, found)
		}
		if got.table != table || got.column != column {
			t.Fatalf("参考 schema 中 %s 的定义不符：table=%s column=%s", want, got.table, got.column)
		}
	}
}

// stripSQLLineComments 去掉 SQL 行注释（-- 之后的内容），逐行处理。
func stripSQLLineComments(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(lines, "\n")
}

// readSchemaReference 读取参考 schema 文件。
//
// 测试的工作目录是包目录（internal/model），因此需要向上回溯定位仓库根下的 docs/。
func readSchemaReference(t *testing.T) (string, bool) {
	t.Helper()
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 6; i++ {
			candidate := filepath.Join(dir, "docs", "SCHEMA.sql")
			if raw, readErr := os.ReadFile(candidate); readErr == nil {
				return string(raw), true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	// 兜底：直接按相对路径尝试。
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "SCHEMA.sql"))
	if err != nil {
		return "", false
	}
	return string(raw), true
}
