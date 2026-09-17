// renderdump 把各组件 × 各安装方式渲染出的 ansible 产物落盘，便于离线复核。
//
// 为什么需要它（INC-006 的教训）：渲染器单测只能证明"我们渲染出的字符串对不对"，
// 证明不了"下游工具怎么读"。把产物落盘后可以：
//  1. 用 ansible 真正使用的解析器复核：deploy/ansible/tools/check_artifacts.py
//  2. 在真机上跑：ansible-playbook --syntax-check -i <inventory> <playbook>
//  3. 人工逐行读一遍——本轮 4 个跨层缺陷（Jinja/Go 模板、systemd 绝对路径、YAML 折叠）
//     都是这么找出来的。
//
// 用法（在 middleware-ops 目录下）：
//
//	go run ./cmd/renderdump ./render-artifacts
//
// 产物里的口令一律是 `${MONITOR_PASSWORD}` 占位（渲染器自带的脱敏版本），可安全留存。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"middleware-ops/internal/integration"
)

func main() {
	dir := "render-artifacts"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败：%v\n", err)
		os.Exit(1)
	}
	emit := func(name, content string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "写入 %s 失败：%v\n", path, err)
			os.Exit(1)
		}
		fmt.Println(path)
	}

	cases := []struct {
		typ    string
		mode   string
		port   int
		net    string
		addr   string
		remark string
	}{
		{integration.TypeRedis, integration.InstallModeDocker, 9121, "host", "127.0.0.1:6379", ""},
		{integration.TypeRedis, integration.InstallModeDockerSystemd, 9121, "host", "127.0.0.1:6379", ""},
		{integration.TypeMySQL, integration.InstallModeDocker, 9104, "bridge", "127.0.0.1:3306", "bridge 才有端口映射"},
		{integration.TypeNode, integration.InstallModeDocker, 9100, "host", "127.0.0.1:9100", ""},
		{integration.TypeNode, integration.InstallModeBinary, 9100, "host", "127.0.0.1:9100", "无需目标机 docker"},
	}
	for _, c := range cases {
		tpl, ok := integration.TemplateOf(c.typ)
		if !ok {
			fmt.Fprintf(os.Stderr, "模板缺失：%s\n", c.typ)
			os.Exit(1)
		}
		address, err := integration.ParseAddress(c.addr, tpl.DefaultPort, tpl.URLScheme, tpl.URLPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "地址解析失败 %s：%v\n", c.addr, err)
			os.Exit(1)
		}
		name := c.typ + "-" + c.mode
		art, err := integration.RenderRemoteInstall(tpl, integration.Instance{
			Name: name, MWType: c.typ, Address: address,
			Username: "mwops_exporter", Password: "demo-password", Environment: "dev",
		}, integration.RemoteOptions{
			Host: "10.0.0.9", SSHUser: "root", SSHPort: 22, SSHPassword: "demo-ssh-password",
			ExporterPort: c.port, InstallMode: c.mode, DockerNetwork: c.net, Become: true,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s 渲染失败：%v\n", name, err)
			os.Exit(1)
		}
		emit(name+".yml", art.MaskedPlaybook)
		emit(name+".vars.yml", art.MaskedVarsFile)
		emit(name+".ini", art.MaskedInventory)
	}

	for _, typ := range []string{integration.TypeMySQL, integration.TypePG} {
		port := 3306
		if typ == integration.TypePG {
			port = 5432
		}
		art, err := integration.RenderAccountSQL(integration.AccountSQLRequest{
			Name: typ + "-account", MWType: typ, DBHost: "127.0.0.1", DBPort: port,
			ExecUser: "root", ExecPassword: "demo-password",
			Statements: []string{"SELECT 1"},
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s 账号 SQL 渲染失败：%v\n", typ, err)
			os.Exit(1)
		}
		emit(strings.Join([]string{"account", typ, "yml"}, "."), art.MaskedPlaybook)
	}

	fmt.Printf("\n渲染完成：%s\n复核：python3 deploy/ansible/tools/check_artifacts.py %s\n", dir, dir)
}
