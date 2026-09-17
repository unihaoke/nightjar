package service

import (
	"strings"
	"testing"

	"middleware-ops/internal/integration"
)

// 本文件锁定「平台代为创建只读监控账号」的模板 SQL。
//
// 这是平台唯一会**写被管数据库**的地方，因此必须固化三件事：
//  1. 只允许内置模板（服务层不接受使用者传入任意 SQL）；
//  2. 幂等（可重复执行，不会因账号已存在而失败）；
//  3. 最小权限 + 不含任何破坏性语句（DROP/DELETE/UPDATE/GRANT ALL 等一律不得出现）。

func TestMonitoringAccountSQLIsIdempotentAndMinimal(t *testing.T) {
	statements, err := monitoringAccountSQL(integration.TypeMySQL, "exporter", "deadbeef")
	if err != nil {
		t.Fatalf("MySQL 模板应可用：%v", err)
	}
	joined := strings.Join(statements, ";\n")

	if !strings.Contains(joined, "CREATE USER IF NOT EXISTS") {
		t.Fatalf("必须是 CREATE USER IF NOT EXISTS（保证幂等）：\n%s", joined)
	}
	if !strings.Contains(joined, "ALTER USER") {
		t.Fatalf("必须带 ALTER USER（保证重复执行时口令被重置）：\n%s", joined)
	}
	if !strings.Contains(joined, "GRANT PROCESS, REPLICATION CLIENT, SELECT ON *.*") {
		t.Fatalf("必须是监控所需的最小权限集合：\n%s", joined)
	}
	if !strings.Contains(joined, "MAX_USER_CONNECTIONS") {
		t.Fatalf("应限制最大连接数，避免高频抓取压垮实例：\n%s", joined)
	}
	if !strings.Contains(joined, "mysql_native_password") {
		t.Fatalf("MySQL 8 需要显式指定认证插件（兼容 mysqld_exporter）：\n%s", joined)
	}

	// 破坏性关键字一律不得出现。
	for _, forbidden := range []string{"DROP ", "DELETE ", "UPDATE ", "INSERT ", "TRUNCATE", "GRANT ALL", "SHUTDOWN"} {
		if strings.Contains(strings.ToUpper(joined), forbidden) {
			t.Fatalf("模板中不得出现 %q：\n%s", forbidden, joined)
		}
	}
}

func TestMonitoringAccountSQLRejectsUnsupportedComponents(t *testing.T) {
	// Redis 的口令由目标自身的 requirepass 决定，平台不该也不需要建账号。
	if _, err := monitoringAccountSQL(integration.TypeRedis, "exporter", "x"); err == nil {
		t.Fatal("Redis 不应支持代建账号（应返回明确错误）")
	}
	if _, err := monitoringAccountSQL(integration.TypeKafka, "exporter", "x"); err == nil {
		t.Fatal("Kafka 不应支持代建账号")
	}
}

func TestMonitoringAccountSQLPostgres(t *testing.T) {
	statements, err := monitoringAccountSQL(integration.TypePG, "exporter", "deadbeef")
	if err != nil {
		t.Fatalf("PG 模板应可用：%v", err)
	}
	joined := strings.Join(statements, ";\n")
	if !strings.Contains(joined, "pg_roles") || !strings.Contains(joined, "GRANT pg_monitor") {
		t.Fatalf("PG 模板应幂等建角色并授予 pg_monitor：\n%s", joined)
	}
}

func TestRandomHexPasswordHasNoEscapeRisk(t *testing.T) {
	secret := randomHexPassword(24)
	if len(secret) != 48 {
		t.Fatalf("24 字节应生成 48 位十六进制，实际 %d：%s", len(secret), secret)
	}
	// 口令会进入 SQL 与容器环境变量：不能含引号、反斜杠、@、空格等需要转义的字符。
	for _, ch := range secret {
		if !strings.ContainsRune("0123456789abcdef", ch) {
			t.Fatalf("口令含非十六进制字符 %q：%s", ch, secret)
		}
	}
	if randomHexPassword(24) == randomHexPassword(24) {
		t.Fatal("两次生成的口令不应相同")
	}
}

// TestBootstrapDefaultsByComponent 锁定「自动建号」的默认策略。
//
// 产品口径（用户明确要求）：**需要账号的组件默认由平台代建**，
// 使用者不需要提前建号、也不需要自己想账号名与口令；
// 只有显式传 false 才关闭，不需要账号的组件（Redis 等）恒为关闭。
func TestBootstrapDefaultsByComponent(t *testing.T) {
	svc := &IntegrationService{}

	for _, mwType := range []string{integration.TypeMySQL, integration.TypePG} {
		if !svc.shouldBootstrapAccount(IntegrationInput{}, mwType) {
			t.Fatalf("%s 默认应由平台自动建号（使用者不必提前建号）", mwType)
		}
	}
	if svc.shouldBootstrapAccount(IntegrationInput{}, integration.TypeRedis) {
		t.Fatal("Redis 口令由目标自身鉴权决定，不应默认建号")
	}

	off := false
	if svc.shouldBootstrapAccount(IntegrationInput{BootstrapAccount: &off}, integration.TypeMySQL) {
		t.Fatal("显式关闭后不得动被管数据库")
	}
	on := true
	if !svc.shouldBootstrapAccount(IntegrationInput{BootstrapAccount: &on}, integration.TypeRedis) {
		t.Fatal("显式开启后应执行（即使该组件默认不需要账号）")
	}
}

// TestDefaultMonitorUserName 锁定：账号名留空时由模板给出默认值。
func TestDefaultMonitorUserName(t *testing.T) {
	for _, mwType := range []string{integration.TypeMySQL, integration.TypePG} {
		tpl, ok := integration.TemplateOf(mwType)
		if !ok {
			t.Fatalf("模板 %s 应存在", mwType)
		}
		if tpl.MonitorUser == "" {
			t.Fatalf("%s 应声明默认监控账号名（表单留空时自动填充）", mwType)
		}
	}
	redisTpl, _ := integration.TemplateOf(integration.TypeRedis)
	if redisTpl.MonitorUser != "" {
		t.Fatal("Redis 不应声明默认监控账号名")
	}
}

// TestRotateAndDropSQLAreSelfService 锁定轮换/删除 SQL 的语义：
// 轮换必须是「账号改自己口令」（否则就要再要一次管理员凭据），删除必须幂等。
func TestRotateAndDropSQLAreSelfService(t *testing.T) {
	rotate, err := rotateAccountSQL(integration.TypeMySQL, "deadbeef")
	if err != nil {
		t.Fatalf("MySQL 轮换模板应可用：%v", err)
	}
	if !strings.Contains(rotate[0], "ALTER USER USER()") {
		t.Fatalf("MySQL 轮换必须是 ALTER USER USER()（自助轮换的前提）：%v", rotate)
	}
	pgRotate, err := rotateAccountSQL(integration.TypePG, "deadbeef")
	if err != nil || !strings.Contains(pgRotate[0], "ALTER ROLE CURRENT_USER") {
		t.Fatalf("PostgreSQL 轮换必须是 ALTER ROLE CURRENT_USER：%v %v", pgRotate, err)
	}
	drop, err := dropAccountSQL(integration.TypeMySQL, "mwops_exporter")
	if err != nil || !strings.Contains(drop[0], "DROP USER IF EXISTS") {
		t.Fatalf("删除账号必须幂等（IF EXISTS）：%v %v", drop, err)
	}
	// 轮换模板绝不能带破坏性权限语句。
	if strings.Contains(strings.ToUpper(strings.Join(rotate, ";")), "GRANT ") {
		t.Fatal("轮换不应包含 GRANT")
	}
}
