// Package docker 是 Docker Engine API 的最小客户端。
//
// 用途：集成中心在「一键集成」时把官方 Exporter 拉起来（等价于云厂商控制台
// 的「一键安装」）。只实现四个动作——ping / inspect / create+start / remove——
// 且只用标准库，避免为平台引入 Docker SDK 依赖。
//
// 安全边界：
//   - 默认关闭（integration.docker_enabled=false），需要显式开启并挂载 docker.sock；
//   - 挂载 docker.sock 等于把宿主机 root 权限交给平台容器，生产环境应改用
//     「渲染配置 + 人工执行」，或为 socket 配置只读代理；
//   - 客户端只接受来自内置模板的镜像名与参数，不接受用户自定义的任意镜像/命令。
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// ContainerSpec 描述一个待创建的 Exporter 容器。
type ContainerSpec struct {
	Name  string
	Image string
	Env   []string
	Cmd   []string
	// Networks 为需要加入的网络列表：第一个作为创建时的 NetworkMode，
	// 其余在容器创建后通过 /networks/{id}/connect 追加。
	//
	// 为什么需要多网络：Exporter 既要被 Prometheus 抓到（监控面，平台网络），
	// 又要能连上被管实例（目标容器所在网络）。后者由平台在集成时自动发现，
	// 例如：mwops（监控面）+ jd_jd-data（目标容器所在网络）。
	Networks []string
	Restart  string
	Labels   map[string]string
	// Binds 为容器挂载，docker 语法："<命名卷或宿主路径>:<容器内路径>[:ro]"。
	//
	// 日志采集就靠它：平台把被管容器的日志卷按名字挂进自己的采集容器，
	// 被管项目因此不需要为监控做任何改动（卷名相同即共享）。
	Binds []string
	// Entrypoint 用于复用平台镜像里附带的其它二进制（如日志 Agent）。
	Entrypoint []string
}

// ContainerDetail 是被管容器的详细配置（日志位置发现的依据）。
type ContainerDetail struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Image    string   `json:"image"`
	Env      []string `json:"env"`
	Networks []string `json:"networks"`
	Mounts   []Mount  `json:"mounts"`
}

// Mount 描述一个挂载点。
type Mount struct {
	// Type 取值 volume / bind / tmpfs。
	Type string `json:"type"`
	// Name 为命名卷名（Type=volume 时有值）。
	Name string `json:"name"`
	// Source 为宿主路径（Type=bind 时有值）。
	Source string `json:"source"`
	// Destination 为容器内路径——日志目录就是从这里看出来的。
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
}

// State 是容器的观测状态。
type State struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	Status  string `json:"status"`
	Running bool   `json:"running"`
	// ExitCode 供一次性容器（如代执行 SQL 的 client）判断成败。
	ExitCode int `json:"exit_code"`
}

// Client 是 Engine API 客户端。
type Client struct {
	baseURL string
	http    *http.Client
}

// New 依据 host 创建客户端。
//
// 支持 unix:///var/run/docker.sock（同机 socket）与 tcp://host:port（远程/测试）。
func New(host string) (*Client, error) {
	trimmed := strings.TrimSpace(host)
	if trimmed == "" {
		return nil, fmt.Errorf("docker host 未配置")
	}
	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	var baseURL string
	switch {
	case strings.HasPrefix(trimmed, "unix://"):
		socketPath := strings.TrimPrefix(trimmed, "unix://")
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		}
		baseURL = "http://docker"
	case strings.HasPrefix(trimmed, "tcp://"), strings.HasPrefix(trimmed, "http://"), strings.HasPrefix(trimmed, "https://"):
		normalized := strings.Replace(trimmed, "tcp://", "http://", 1)
		parsed, err := url.Parse(normalized)
		if err != nil {
			return nil, fmt.Errorf("docker host %q 无法解析: %w", host, err)
		}
		baseURL = parsed.Scheme + "://" + parsed.Host
	default:
		return nil, fmt.Errorf("docker host %q 不合法（支持 unix:// 或 tcp://）", host)
	}
	return &Client{baseURL: baseURL, http: &http.Client{Transport: transport, Timeout: 30 * time.Second}}, nil
}

// Ping 探测 Docker 守护进程可用性。
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/_ping", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("docker ping 返回 %d", resp.StatusCode)
	}
	return nil
}

// Inspect 按容器名查询状态；容器不存在返回 (nil, nil)。
func (c *Client) Inspect(ctx context.Context, name string) (*State, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("查询容器 %s 失败：HTTP %d", name, resp.StatusCode)
	}
	var payload struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Status   string `json:"Status"`
			Running  bool   `json:"Running"`
			ExitCode int    `json:"ExitCode"`
		} `json:"State"`
		Config struct {
			Image string `json:"Image"`
		} `json:"Config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析容器信息失败: %w", err)
	}
	return &State{
		ID: payload.ID, Name: strings.TrimPrefix(payload.Name, "/"),
		Image: payload.Config.Image, Status: payload.State.Status,
		Running: payload.State.Running, ExitCode: payload.State.ExitCode,
	}, nil
}

// Ensure 确保容器按 spec 运行：已存在则先删除再重建，保证与集成配置一致。
//
// 返回执行动作（created / recreated）与实际容器 ID。
func (c *Client) Ensure(ctx context.Context, spec ContainerSpec) (string, string, error) {
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Image) == "" {
		return "", "", fmt.Errorf("容器名与镜像不能为空")
	}
	action := "created"
	existing, err := c.Inspect(ctx, spec.Name)
	if err != nil {
		return "", "", err
	}
	if existing != nil {
		// 重建而非复用：环境变量/参数变化时容器不会自动生效，重建是最可预期的行为。
		if err := c.Remove(ctx, spec.Name); err != nil {
			return "", "", err
		}
		action = "recreated"
	}
	id, err := c.create(ctx, spec)
	if err != nil {
		return "", "", err
	}
	// 追加网络：第一个网络已在创建时指定，其余在这里补挂。
	for _, network := range spec.Networks[min(1, len(spec.Networks)):] {
		if network == "" {
			continue
		}
		if err := c.ConnectNetwork(ctx, id, network); err != nil {
			return action, id, err
		}
	}
	if err := c.Start(ctx, id); err != nil {
		return action, id, err
	}
	return action, id, nil
}

// ConnectNetwork 把容器接入指定网络（已在该网络内时视为成功）。
func (c *Client) ConnectNetwork(ctx context.Context, containerID, network string) error {
	if strings.TrimSpace(network) == "" || strings.TrimSpace(containerID) == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{"Container": containerID})
	if err != nil {
		return fmt.Errorf("序列化网络接入请求失败: %w", err)
	}
	resp, err := c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	message := strings.TrimSpace(string(raw))
	// 已在网络内属于幂等成功，不当作错误。
	if resp.StatusCode >= 400 && !strings.Contains(message, "already exists") {
		return fmt.Errorf("接入网络 %s 失败：HTTP %d %s", network, resp.StatusCode, message)
	}
	return nil
}

// Create 创建容器并返回 ID（供测试与分步调用）。
func (c *Client) Create(ctx context.Context, spec ContainerSpec) (string, error) {
	return c.create(ctx, spec)
}

// create 调用 POST /containers/create。
func (c *Client) create(ctx context.Context, spec ContainerSpec) (string, error) {
	labels := map[string]string{"mwops.managed": "true"}
	for key, value := range spec.Labels {
		labels[key] = value
	}
	restart := spec.Restart
	if restart == "" {
		restart = "unless-stopped"
	}
	hostConfig := map[string]any{
		"RestartPolicy": map[string]any{"Name": restart},
	}
	if len(spec.Networks) > 0 && spec.Networks[0] != "" {
		hostConfig["NetworkMode"] = spec.Networks[0]
	}
	if len(spec.Binds) > 0 {
		hostConfig["Binds"] = spec.Binds
	}
	body := map[string]any{
		"Image":      spec.Image,
		"Env":        spec.Env,
		"Labels":     labels,
		"HostConfig": hostConfig,
	}
	if len(spec.Cmd) > 0 {
		body["Cmd"] = spec.Cmd
	}
	if len(spec.Entrypoint) > 0 {
		body["Entrypoint"] = spec.Entrypoint
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("序列化容器配置失败: %w", err)
	}
	endpoint := "/containers/create?name=" + url.QueryEscape(spec.Name)
	resp, err := c.do(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("创建容器 %s 失败：HTTP %d %s", spec.Name, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("解析创建结果失败: %w", err)
	}
	if parsed.ID == "" {
		return "", fmt.Errorf("创建容器 %s 未返回容器 ID", spec.Name)
	}
	return parsed.ID, nil
}

// Start 启动容器。
func (c *Client) Start(ctx context.Context, id string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("启动容器失败：HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// Stop 停止容器。
func (c *Client) Stop(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(name)+"/stop", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotModified {
		return fmt.Errorf("停止容器 %s 失败：HTTP %d", name, resp.StatusCode)
	}
	return nil
}

// Remove 强制删除容器（不存在时视为成功）。
func (c *Client) Remove(ctx context.Context, name string) error {
	resp, err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(name)+"?force=1&v=1", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		return fmt.Errorf("删除容器 %s 失败：HTTP %d %s", name, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return nil
}

// ListManaged 列出由平台管理的容器（按标签过滤）。
func (c *Client) ListManaged(ctx context.Context) ([]State, error) {
	filters := url.QueryEscape(`{"label":["mwops.managed=true"]}`)
	resp, err := c.do(ctx, http.MethodGet, "/containers/json?all=1&filters="+filters, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("列出容器失败：HTTP %d", resp.StatusCode)
	}
	var payload []struct {
		ID     string   `json:"Id"`
		Names  []string `json:"Names"`
		Image  string   `json:"Image"`
		State  string   `json:"State"`
		Status string   `json:"Status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析容器列表失败: %w", err)
	}
	out := make([]State, 0, len(payload))
	for _, item := range payload {
		name := ""
		if len(item.Names) > 0 {
			name = strings.TrimPrefix(item.Names[0], "/")
		}
		out = append(out, State{
			ID: item.ID, Name: name, Image: item.Image,
			Status: item.Status, Running: item.State == "running",
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// logTailLines 限制 RunOnce 拉取的日志行数。
const logTailLines = 50

// RunOnce 起一个一次性容器执行命令并取回输出（用于"平台代为执行固定 SQL"）。
//
// 语义：创建 → 启动 → 等退出 → 取日志 → 删除。全程使用平台模板内的镜像与参数，
// 不接受使用者自定义命令（见 internal/service/integration.go 的固定 SQL 模板）。
func (c *Client) RunOnce(ctx context.Context, spec ContainerSpec, timeout time.Duration) (string, error) {
	if strings.TrimSpace(spec.Name) == "" || strings.TrimSpace(spec.Image) == "" {
		return "", fmt.Errorf("容器名与镜像不能为空")
	}
	// 同名残留先清掉，保证可重复执行
	if existing, err := c.Inspect(ctx, spec.Name); err == nil && existing != nil {
		_ = c.Remove(ctx, spec.Name)
	}
	id, err := c.create(ctx, spec)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Remove(ctx, spec.Name) }()

	for _, network := range spec.Networks[min(1, len(spec.Networks)):] {
		if network == "" {
			continue
		}
		if err := c.ConnectNetwork(ctx, id, network); err != nil {
			return "", err
		}
	}
	if err := c.Start(ctx, id); err != nil {
		return "", err
	}
	// 等退出：容器不存在（已被 --rm 语义删除）或状态为 exited 即视为结束。
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return "", fmt.Errorf("等待一次性容器 %s 超时（%s）", spec.Name, timeout)
		}
		state, err := c.Inspect(ctx, spec.Name)
		if err != nil {
			return "", err
		}
		if state == nil || (!state.Running && state.Status == "exited") {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	logs, code, err := c.Logs(ctx, spec.Name)
	if err != nil {
		return logs, err
	}
	if code != 0 {
		return logs, fmt.Errorf("一次性容器退出码 %d：%s", code, strings.TrimSpace(logs))
	}
	return logs, nil
}

// Logs 读取容器日志与退出码（tail 限制见 logTailLines）。
func (c *Client) Logs(ctx context.Context, name string) (string, int, error) {
	inspect, err := c.Inspect(ctx, name)
	if err != nil {
		return "", 0, err
	}
	if inspect == nil {
		return "", 0, fmt.Errorf("容器 %s 不存在", name)
	}
	query := fmt.Sprintf("?stdout=1&stderr=1&tail=%d", logTailLines)
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/logs"+query, nil)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	// Docker 的日志流带 8 字节帧头（stdout/stderr 交替），这里只做可读化处理。
	text := string(raw)
	if len(raw) > 8 && (raw[0] == 1 || raw[0] == 2) && raw[1] == 0 && raw[2] == 0 && raw[3] == 0 {
		text = stripDockerLogFrames(raw)
	}
	return text, inspect.ExitCode, nil
}

// stripDockerLogFrames 去掉 Docker multiplexed stream 的帧头。
func stripDockerLogFrames(raw []byte) string {
	var b strings.Builder
	for i := 0; i+8 <= len(raw); {
		size := int(raw[i+4])<<24 | int(raw[i+5])<<16 | int(raw[i+6])<<8 | int(raw[i+7])
		i += 8
		if size < 0 || i+size > len(raw) {
			b.Write(raw[i:])
			break
		}
		b.Write(raw[i : i+size])
		i += size
	}
	return b.String()
}

// TargetResolution 是「集成目标」的解析结果。
//
// 使用者在平台里只填一个地址（如 `interview-redis:6379`、`jd-mysql:3306`），
// 平台自己去 docker 里查这个名字对应哪个容器、容器在哪张网络上——
// 被管项目因此**不需要**为监控改任何配置：不用建互联网络、不用加别名、
// 不用把自己的 compose 文件交给平台。
type TargetResolution struct {
	// Host 是可在 Networks 上解析的主机名。优先取容器名：容器名在同一个
	// 用户自定义网络里一定能被内嵌 DNS 解析，比别名更可靠。
	Host string
	// Container 是匹配到的容器名；为空表示没找到。
	Container string
	// Networks 是该容器所在的真实 docker 网络名（如 `jd_jd-data`）。
	Networks []string
	// MatchedBy 说明匹配依据：container_name / compose_service / alias。
	MatchedBy string
	// Running 表示目标容器是否在运行。
	Running bool
	// Candidates 在没匹配到时给出已知的容器名（便于报错时提示正确写法）。
	Candidates []string
}

// targetInfo 是解析过程中的单个容器快照。
type targetInfo struct {
	name    string
	service string
	aliases []string
	nets    []string
	running bool
}

// ResolveTarget 把「用户填的名字」解析成「容器 + 容器所在的网络」。
//
// 匹配顺序：容器名 → compose 服务名 → 网络别名（并集所有命中者）。
// 这样无论用户填 `interview-redis`（容器名）、`redis`（服务名）
// 还是 `jd-redis`（别名），平台都能自己找到目标，并把 Exporter 接到
// 目标真正所在的网络上。
func (c *Client) ResolveTarget(ctx context.Context, nameOrAlias string) (*TargetResolution, error) {
	target := strings.TrimSpace(nameOrAlias)
	res := &TargetResolution{Host: target, Candidates: []string{}}
	if target == "" {
		return res, nil
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("列出容器失败：HTTP %d", resp.StatusCode)
	}
	var list []struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("解析容器列表失败: %w", err)
	}
	// 只看前 200 个运行中的容器：容器很多的主机上不必做成百次 inspect。
	if len(list) > 200 {
		list = list[:200]
	}
	seenNet := map[string]bool{}
	for _, item := range list {
		info, err := c.inspectTarget(ctx, item.ID)
		if err != nil || info == nil {
			continue
		}
		res.Candidates = append(res.Candidates, info.name)
		matched := ""
		switch {
		case info.name == target:
			matched = "container_name"
		case info.service != "" && info.service == target:
			matched = "compose_service"
		case containsString(info.aliases, target):
			matched = "alias"
		}
		if matched == "" {
			continue
		}
		if res.Container == "" {
			res.Container, res.MatchedBy, res.Running = info.name, matched, info.running
			// 容器名是最稳的可解析名；用容器名覆盖用户输入（如输入的是别名）。
			res.Host = info.name
		}
		for _, network := range info.nets {
			if network == "" || seenNet[network] {
				continue
			}
			seenNet[network] = true
			res.Networks = append(res.Networks, network)
		}
	}
	sort.Strings(res.Networks)
	sort.Strings(res.Candidates)
	return res, nil
}

// inspectTarget 读取容器名、compose 服务名、网络别名与网络列表。
func (c *Client) inspectTarget(ctx context.Context, id string) (*targetInfo, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("查询容器失败：HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Networks map[string]struct {
				Aliases []string `json:"Aliases"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	info := &targetInfo{
		name:    strings.TrimPrefix(payload.Name, "/"),
		service: payload.Config.Labels["com.docker.compose.service"],
		running: payload.State.Running,
	}
	for network, detail := range payload.NetworkSettings.Networks {
		info.nets = append(info.nets, network)
		info.aliases = append(info.aliases, detail.Aliases...)
	}
	sort.Strings(info.nets)
	return info, nil
}

// containsString 判断字符串切片是否含某值（大小写敏感，docker 名本身区分大小写）。
func containsString(items []string, value string) bool {
	for _, item := range items {
		if item == value {
			return true
		}
	}
	return false
}

// FindAliasNetworks 找出「哪些 docker 网络里存在该别名」。
//
// 保留为兼容入口；新代码请用 ResolveTarget（它同时支持容器名/服务名）。
func (c *Client) FindAliasNetworks(ctx context.Context, alias string) ([]string, error) {
	trimmed := strings.TrimSpace(alias)
	if trimmed == "" {
		return nil, nil
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("列出容器失败：HTTP %d", resp.StatusCode)
	}
	var list []struct {
		ID string `json:"Id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("解析容器列表失败: %w", err)
	}
	// 只看前 100 个容器，避免在容器很多的主机上做上百次 inspect。
	if len(list) > 100 {
		list = list[:100]
	}
	found := make(map[string]bool)
	for _, item := range list {
		detail, err := c.inspectNetworks(ctx, item.ID)
		if err != nil {
			continue
		}
		for network, aliases := range detail {
			for _, candidate := range aliases {
				if candidate == trimmed {
					found[network] = true
				}
			}
		}
	}
	out := make([]string, 0, len(found))
	for name := range found {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// inspectNetworks 返回「网络名 → 该容器在该网络上的别名列表」。
func (c *Client) inspectNetworks(ctx context.Context, id string) (map[string][]string, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("查询容器失败：HTTP %d", resp.StatusCode)
	}
	var payload struct {
		NetworkSettings struct {
			Networks map[string]struct {
				Aliases []string `json:"Aliases"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(payload.NetworkSettings.Networks))
	for name, network := range payload.NetworkSettings.Networks {
		out[name] = network.Aliases
	}
	return out, nil
}

// InspectDetail 返回容器的详细配置（网络、挂载、环境变量）。
//
// 平台据此**反查**被管项目的日志位置：容器把日志写在哪个卷/宿主目录、
// 挂载到容器内哪个路径。使用者不需要知道这些，平台自己从 docker 配置里读。
func (c *Client) InspectDetail(ctx context.Context, name string) (*ContainerDetail, error) {
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("查询容器 %s 失败：HTTP %d", name, resp.StatusCode)
	}
	var payload struct {
		ID     string `json:"Id"`
		Name   string `json:"Name"`
		Config struct {
			Image string   `json:"Image"`
			Env   []string `json:"Env"`
		} `json:"Config"`
		Mounts          []Mount `json:"Mounts"`
		NetworkSettings struct {
			Networks map[string]struct {
				Aliases []string `json:"Aliases"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析容器详情失败: %w", err)
	}
	detail := &ContainerDetail{
		ID: payload.ID, Name: strings.TrimPrefix(payload.Name, "/"),
		Image: payload.Config.Image, Env: payload.Config.Env, Mounts: payload.Mounts,
	}
	for network := range payload.NetworkSettings.Networks {
		detail.Networks = append(detail.Networks, network)
	}
	sort.Strings(detail.Networks)
	return detail, nil
}

// do 执行一次 Engine API 请求。
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("构造 docker 请求失败: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 docker 失败（%s）：%w", c.baseURL, err)
	}
	return resp, nil
}
