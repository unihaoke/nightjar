package integration

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件渲染「在目标主机上执行监控账号 SQL」的 Ansible 产物。
//
// 为什么需要它（而不是复用平台侧的 docker 一次性容器）：
//   - 远程集成本不该依赖平台的 docker.sock（平台只是 SSH 到目标机装 Exporter）；
//   - 现实中数据库常只对本机/内网开放（未发布宿主端口），平台直连 DB 端口会直接失败；
//   - 因此把「建号 / 轮换 / 删除账号」的 SQL 放到**数据库所在的机器**上执行：
//     首选目标机自带的 mysql / psql 客户端，缺失时回退到目标机上的 docker 一次性容器。
//
// 已知边界（文档中同样写明）：若数据库只存在于容器内、宿主又没有发布端口
//（例如 compose 起 MySQL 且未映射 3306），目标机本地客户端同样连不上——
// 那种场景请继续使用「本机」部署位置（平台把一次性客户端接进同一 docker 网络），
// 或手工建号后只填账号口令。

// AccountSQLRequest 描述一次远程账号 SQL 执行。
type AccountSQLRequest struct {
	// Name 为集成名（用于产物文件名与一次性容器名）。
	Name string
	// MWType 为组件类型：mysql / pg（决定用哪个客户端与口令环境变量）。
	MWType string
	// DBHost / DBPort 为数据库地址（在**目标机**上解析）。
	DBHost string
	DBPort int
	// ExecUser / ExecPassword 为执行 SQL 的账号与口令（管理员，或监控账号自身用于自助改口令）。
	// ExecPassword 只写进 0600 的 vars 文件，不进 playbook、不进命令行。
	ExecUser     string
	ExecPassword string
	// Statements 为内置模板 SQL（不接受使用者传入任意语句）。
	Statements []string
	// ClientImage 为回退到 docker 时使用的一次性客户端镜像。
	ClientImage string
}

// AccountSQLArtifacts 是一次远程账号 SQL 执行的产物。
type AccountSQLArtifacts struct {
	Playbook string
	// VarsFile 含口令（落盘 0600、用完即删）；MaskedVarsFile 供界面展示。
	VarsFile       string
	MaskedPlaybook string
	MaskedVarsFile string
	// RunCommand 是平台实际执行的命令（不含口令）。
	RunCommand string
	// TaskName 用于在备注/日志里说明这次做了什么。
	TaskName string
}

// accountSQLClient 返回该类型在目标机上使用的客户端命令、口令环境变量与默认客户端镜像。
func accountSQLClient(mwType string) (client, passwordEnv, defaultImage string) {
	if mwType == TypePG {
		return "psql", "PGPASSWORD", "postgres:15-alpine"
	}
	return "mysql", "MYSQL_PWD", "mysql:8.0"
}

// RenderAccountSQL 渲染「在目标机执行账号 SQL」的 playbook 与变量文件。
//
// 安全约定与远程安装一致：口令只在 0600 的 vars 文件里（`-e @vars.yml`），
// playbook 不含密；口令通过环境变量传给客户端，不出现在命令行；每条任务 no_log。
func RenderAccountSQL(req AccountSQLRequest) (AccountSQLArtifacts, error) {
	mwType := strings.TrimSpace(req.MWType)
	if mwType != TypeMySQL && mwType != TypePG {
		return AccountSQLArtifacts{}, fmt.Errorf("%s 不支持远程账号 SQL（仅支持 mysql / pg）", mwType)
	}
	if len(req.Statements) == 0 {
		return AccountSQLArtifacts{}, fmt.Errorf("没有需要执行的 SQL")
	}
	host := strings.TrimSpace(req.DBHost)
	if host == "" {
		return AccountSQLArtifacts{}, fmt.Errorf("远程账号 SQL 需要数据库地址")
	}
	client, passwordEnv, defaultImage := accountSQLClient(mwType)
	image := strings.TrimSpace(req.ClientImage)
	if image == "" {
		image = defaultImage
	}
	port := req.DBPort
	if port <= 0 {
		port = 3306
		if mwType == TypePG {
			port = 5432
		}
	}

	var b strings.Builder
	b.WriteString("# 由平台「集成中心」生成：在目标主机上执行监控账号 SQL（集成 " + req.Name + "）\n")
	b.WriteString("# 请勿手工修改：平台按此模板执行，改动会在下次执行时被覆盖。\n")
	b.WriteString("# 优先使用目标机自带的 " + client + " 客户端；缺失时回退到目标机上的 docker 一次性容器。\n")
	b.WriteString("# 渲染器: " + PlaybookRendererVersion + "\n")
	b.WriteString("- name: 执行账号 SQL（" + req.Name + "）\n")
	b.WriteString("  hosts: exporter_target\n")
	b.WriteString("  become: true\n")
	b.WriteString("  gather_facts: false\n")
	b.WriteString("  vars:\n")
	b.WriteString("    db_host: " + yamlScalar(host) + "\n")
	b.WriteString("    db_port: " + strconv.Itoa(port) + "\n")
	b.WriteString("    db_user: " + yamlScalar(strings.TrimSpace(req.ExecUser)) + "\n")
	b.WriteString("    client_image: " + yamlScalar(image) + "\n")
	b.WriteString("  tasks:\n")
	b.WriteString("    - name: 探测目标机是否自带 " + client + " 客户端\n")
	// shell 内建 `command -v` 需经 shell 执行；ansible.builtin.command 不经 shell（见 INC-006）。
	b.WriteString("      ansible.builtin.shell: command -v " + client + "\n")
	b.WriteString("      register: account_client\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("    - name: 探测目标机是否可用 docker\n")
	b.WriteString("      ansible.builtin.shell: command -v docker\n")
	b.WriteString("      register: account_docker\n")
	b.WriteString("      changed_when: false\n")
	b.WriteString("      failed_when: false\n")
	b.WriteString("      when: account_client.rc != 0\n")
	b.WriteString("    - name: 缺少客户端与 docker 时明确失败（不擅自改目标机软件包）\n")
	b.WriteString("      ansible.builtin.fail:\n")
	b.WriteString("        msg: >-\n")
	b.WriteString("          目标主机上既没有 " + client + " 客户端，也没有 docker，平台无法代为执行账号 SQL。" + "\n")
	b.WriteString("          请二选一：① 在目标机安装 " + client + " 客户端；" + "\n")
	b.WriteString("          ② 取消勾选「由平台创建只读监控账号」，手工建号后只把账号口令填进集成表单。" + "\n")
	b.WriteString("      when: account_client.rc != 0 and (account_docker.rc | default(1)) != 0\n")

	// 分支一：目标机自带客户端
	b.WriteString("    - name: 使用目标机本地 " + client + " 客户端执行 SQL\n")
	b.WriteString("      ansible.builtin.shell: |\n")
	for _, line := range accountSQLShellLines(client, passwordEnv, false, req.Statements) {
		b.WriteString("        " + line + "\n")
	}
	b.WriteString("      environment:\n")
	b.WriteString("        " + passwordEnv + ": \"{{ db_password }}\"\n")
	b.WriteString("      no_log: true\n")
	b.WriteString("      when: account_client.rc == 0\n")

	// 分支二：回退到目标机上的 docker（--network host 以便连到宿主上的库）
	b.WriteString("    - name: 回退用 docker 一次性容器执行 SQL\n")
	b.WriteString("      ansible.builtin.shell: |\n")
	for _, line := range accountSQLShellLines(client, passwordEnv, true, req.Statements) {
		b.WriteString("        " + line + "\n")
	}
	b.WriteString("      environment:\n")
	b.WriteString("        " + passwordEnv + ": \"{{ db_password }}\"\n")
	b.WriteString("      no_log: true\n")
	b.WriteString("      when: account_client.rc != 0 and (account_docker.rc | default(1)) == 0\n")

	playbook := strings.Join(strings.Split(b.String(), "\r\n"), "\n")
	if err := validatePlaybookYAML("账号 SQL", playbook); err != nil {
		return AccountSQLArtifacts{}, err
	}
	realVars, maskedVars := renderAccountVars(req.ExecPassword)
	return AccountSQLArtifacts{
		Playbook: playbook, VarsFile: realVars,
		MaskedPlaybook: playbook, MaskedVarsFile: maskedVars,
		RunCommand: "ansible-playbook -i <inventory> <playbook> -e @<vars.yml> --become",
		TaskName:   "账号 SQL",
	}, nil
}

// accountSQLShellLines 生成执行 SQL 的 shell 行（口令一律走环境变量，绝不进命令行）。
func accountSQLShellLines(client, passwordEnv string, viaDocker bool, statements []string) []string {
	lines := make([]string, 0, len(statements))
	for _, stmt := range statements {
		var inner string
		if client == "psql" {
			inner = "psql -h \"{{ db_host }}\" -p \"{{ db_port }}\" -U \"{{ db_user }}\" -d postgres " +
				"-v ON_ERROR_STOP=1 -c " + shellArg(stmt)
		} else {
			inner = "mysql -h \"{{ db_host }}\" -P \"{{ db_port }}\" -u \"{{ db_user }}\" " +
				"--protocol=TCP -e " + shellArg(stmt)
		}
		if viaDocker {
			inner = "docker run --rm --network host -e " + passwordEnv + " {{ client_image }} " + inner
		}
		lines = append(lines, inner)
	}
	return lines
}

// renderAccountVars 渲染 `-e @vars.yml` 内容；第二个返回值是给界面看的占位版本。
func renderAccountVars(password string) (real, masked string) {
	masked = "db_password: ${DB_PASSWORD}\n"
	if strings.TrimSpace(password) == "" {
		// 没有口令（例如自助轮换时用旧口令认证）时不留空值行，避免 YAML 里出现空密码。
		return masked, masked
	}
	return "db_password: " + yamlScalar(password) + "\n", masked
}
