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
	"users":             "username",
	"roles":             "code",
	"approvals":         "ticket_id",
	"ai_analysis_tasks": "task_id",
	"log_alert_events":  "event_id",
	"log_alert_rules":   "name",
	"audit_snapshots":   "snapshot_date",
	"platform_settings": "name",
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

// postgresReservedKeywords 收录相关度较高、确属 PostgreSQL 保留关键字（reserved）
// 或「列名 + 数据类型」位置会产生歧义的词。
//
// 背景：deploy 阶段的初始化脚本曾用未加引号的建表语句，`window INTEGER DEFAULT 5`
// 直接报 syntax error at or near "window"（WINDOW 为 reserved，属 reserved_keywords，
// 不能作列名；而 COUNT / RESULT / LEVEL / STATUS 等属 non-reserved，可裸写）。
// GORM 会对标识符加引号，但初始化脚本、手工 SQL、BI 工具不会，
// 因此模型列名一律不得使用下列词汇。
var postgresReservedKeywords = map[string]bool{
	"all": true, "analyse": true, "analyze": true, "and": true, "any": true, "array": true,
	"as": true, "asc": true, "asymmetric": true, "both": true, "case": true, "cast": true,
	"check": true, "collate": true, "column": true, "constraint": true, "create": true,
	"current_catalog": true, "current_date": true, "current_role": true, "current_time": true,
	"current_timestamp": true, "current_user": true, "default": true, "deferrable": true,
	"desc": true, "distinct": true, "do": true, "else": true, "end": true, "except": true,
	"false": true, "fetch": true, "for": true, "foreign": true, "from": true, "grant": true,
	"group": true, "having": true, "in": true, "initially": true, "intersect": true,
	"into": true, "lateral": true, "leading": true, "limit": true, "localtime": true,
	"localtimestamp": true, "not": true, "null": true, "offset": true, "on": true,
	"only": true, "or": true, "order": true, "placing": true, "primary": true,
	"references": true, "returning": true, "select": true, "session_user": true,
	"some": true, "symmetric": true, "table": true, "then": true, "to": true,
	"trailing": true, "true": true, "union": true, "unique": true, "user": true,
	"using": true, "variadic": true, "when": true, "where": true, "window": true,
	"with": true,
}

// TestModelColumnsAvoidPostgresReservedKeywords 防止模型列名撞上 PostgreSQL 保留字。
//
// 这是线上故障的直接回归：alert_rules.window 曾导致初始化脚本建表语法错误，
// 进而留下半成品 schema 并让后端 AutoMigrate 失败。
func TestModelColumnsAvoidPostgresReservedKeywords(t *testing.T) {
	parser := schema.NamingStrategy{}
	cache := &sync.Map{}
	checked := 0
	for _, entity := range MigrationList() {
		parsed, err := schema.Parse(entity, cache, parser)
		if err != nil {
			t.Fatalf("解析实体失败（%T）: %v", entity, err)
		}
		for _, field := range parsed.Fields {
			name := strings.ToLower(field.DBName)
			checked++
			if postgresReservedKeywords[name] {
				t.Fatalf("表 %s 的列 %s 命中 PostgreSQL 保留关键字，裸写 SQL（初始化脚本/手工运维）会语法报错；"+
					"请改用带 gorm:\"column:...\" 的非保留字列名",
					parsed.Table, field.DBName)
			}
		}
	}
	if checked == 0 {
		t.Fatal("未解析到任何列，测试前置条件失效")
	}
}

// TestSchemaReferenceColumnsAvoidReserved 校验参考 schema 的列名同样不撞保留字。
func TestSchemaReferenceColumnsAvoidReserved(t *testing.T) {
	content, ok := readSchemaReference(t)
	if !ok {
		t.Skip("跳过：未找到 docs/SCHEMA.sql")
	}
	code := stripSQLLineComments(content)
	tablePattern := regexp.MustCompile(`(?is)CREATE TABLE IF NOT EXISTS\s+([a-z0-9_]+)\s*\((.*?)\n\);`)
	columnPattern := regexp.MustCompile(`(?m)^\s{4}([a-z_][a-z0-9_]*)\s+[A-Z]`)
	for _, tableMatch := range tablePattern.FindAllStringSubmatch(code, -1) {
		table := tableMatch[1]
		for _, columnMatch := range columnPattern.FindAllStringSubmatch(tableMatch[2], -1) {
			column := strings.ToLower(columnMatch[1])
			if postgresReservedKeywords[column] {
				t.Fatalf("docs/SCHEMA.sql 中表 %s 的列 %s 命中 PostgreSQL 保留关键字，必须加引号或改名", table, column)
			}
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
