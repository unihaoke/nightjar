// Command agent 是轻量日志采集 Agent（设计文档 4.8.1）。
//
// 职责：tail 日志文件增量读取，记录偏移量避免重复，按批上报到平台 /api/hooks/logs。
// 特点：零依赖（仅标准库）、单文件、可交叉编译后直接投放到应用服务器。
//
// 构建：
//
//	cd middleware-ops && go build -o mwops-agent ./cmd/agent
//
// 运行：
//
//	./mwops-agent -config agent.yaml
//
// 配置文件（agent.yaml）示例：
//
//	platform_url: http://127.0.0.1:8080
//	hook_token: "<MWOPS_HOOK_TOKEN>"
//	service: order-service
//	environment: prod
//	position_file: ./data/agent-position.json
//	files:
//	  - path: /var/log/order-service/error.log
//	    alert_type: error
//	    service: order-service
//	  - path: /var/log/order-service/gc.log
//	    alert_type: gc
//	    service: order-service
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// AgentConfig 是 Agent 配置。
type AgentConfig struct {
	PlatformURL  string `json:"platform_url"`
	HookToken    string `json:"hook_token"`
	Service      string `json:"service"`
	Environment  string `json:"environment"`
	ServerName   string `json:"server_name"`
	PositionFile string `json:"position_file"`
	// ContextLines 为错误前后采集的行数（设计文档 5.2 默认 20 行）。
	ContextLines int `json:"context_lines"`
	// MaxBatch 为单次上报的最大条数。
	MaxBatch int         `json:"max_batch"`
	Files    []LogTarget `json:"files"`
}

// LogTarget 描述一个被采集的日志文件。
type LogTarget struct {
	Path string `json:"path"`
	// AlertType 取值 error / gc / stack。
	AlertType string `json:"alert_type"`
	Service   string `json:"service"`
	// LevelFilter 为最低采集等级（ERROR / WARN / INFO）。
	LevelFilter string `json:"level_filter"`
}

// LogReport 与平台 /api/hooks/logs 契约一致。
type LogReport struct {
	ServerName   string `json:"server_name,omitempty"`
	Service      string `json:"service"`
	Level        string `json:"level"`
	Message      string `json:"message"`
	Stacktrace   string `json:"stacktrace,omitempty"`
	ContextLines string `json:"context_lines,omitempty"`
	Timestamp    string `json:"timestamp,omitempty"`
	AlertType    string `json:"alert_type,omitempty"`
}

// position 记录每个文件的读取偏移。
type position map[string]int64

func main() {
	configPath := flag.String("config", "agent.yaml", "Agent 配置文件路径")
	once := flag.Bool("once", false, "只执行一轮采集后退出（用于调试）")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败: %v\n", err)
		os.Exit(1)
	}

	agent := &agent{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
		logger: os.Stdout,
	}
	if err := agent.loadPosition(); err != nil {
		fmt.Fprintf(os.Stderr, "读取偏移量失败: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		fmt.Fprintln(os.Stderr, "收到退出信号，停止采集…")
		cancel()
	}()

	interval := 5 * time.Second
	if *once {
		interval = 0
	}
	agent.run(ctx, interval)
}

// agent 是采集器实例。
type agent struct {
	cfg      *AgentConfig
	client   *http.Client
	logger   io.Writer
	position position
}

// loadConfig 读取 YAML 配置。
//
// 依赖最小化：Agent 面向客户服务器，避免引入第三方 YAML 库，
// 因此使用一个足够覆盖本配置结构的手写解析器。
func loadConfig(path string) (*AgentConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &AgentConfig{
		ContextLines: 20,
		MaxBatch:     50,
		PositionFile: "./data/agent-position.json",
	}
	var current *LogTarget
	inFiles := false

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		if strings.HasPrefix(trimmed, "files:") {
			inFiles = true
			continue
		}
		if inFiles && strings.HasPrefix(trimmed, "- ") {
			cfg.Files = append(cfg.Files, LogTarget{})
			current = &cfg.Files[len(cfg.Files)-1]
			applyKV(&current.Path, strings.TrimPrefix(trimmed, "- "))
			continue
		}
		if inFiles && indent >= 4 && current != nil {
			applyKV(&current.Path, trimmed)
			// 复用同一解析路径：按 key 分发
			parseTargetField(current, trimmed)
			continue
		}
		inFiles = false
		parseGlobalField(cfg, trimmed)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if cfg.PlatformURL == "" {
		return nil, errors.New("platform_url 未配置")
	}
	if len(cfg.Files) == 0 {
		return nil, errors.New("files 未配置")
	}
	// 归一化：service 缺省时使用全局 service。
	for i := range cfg.Files {
		if cfg.Files[i].Service == "" {
			cfg.Files[i].Service = cfg.Service
		}
		if cfg.Files[i].AlertType == "" {
			cfg.Files[i].AlertType = "error"
		}
	}
	return cfg, nil
}

// parseGlobalField 解析顶层字段。
func parseGlobalField(cfg *AgentConfig, line string) {
	key, value := splitKV(line)
	switch key {
	case "platform_url":
		cfg.PlatformURL = value
	case "hook_token":
		cfg.HookToken = value
	case "service":
		cfg.Service = value
	case "environment":
		cfg.Environment = value
	case "server_name":
		cfg.ServerName = value
	case "position_file":
		cfg.PositionFile = value
	case "context_lines":
		cfg.ContextLines = atoiDefault(value, 20)
	case "max_batch":
		cfg.MaxBatch = atoiDefault(value, 50)
	}
}

// parseTargetField 解析文件条目字段。
func parseTargetField(target *LogTarget, line string) {
	key, value := splitKV(line)
	switch key {
	case "path":
		target.Path = value
	case "alert_type":
		target.AlertType = value
	case "service":
		target.Service = value
	case "level_filter":
		target.LevelFilter = value
	}
}

// applyKV 处理 "- path: xxx" 形式。
func applyKV(field *string, line string) {
	key, value := splitKV(line)
	if key == "path" {
		*field = value
	}
}

// splitKV 解析 "key: value" 并去除引号。
func splitKV(line string) (string, string) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return "", ""
	}
	key := strings.TrimSpace(line[:idx])
	value := strings.TrimSpace(line[idx+1:])
	value = strings.Trim(value, `"'`)
	return key, value
}

// atoiDefault 解析整数。
func atoiDefault(raw string, fallback int) int {
	var v int
	if _, err := fmt.Sscanf(raw, "%d", &v); err != nil {
		return fallback
	}
	return v
}

// loadPosition 读取偏移量文件（记录偏移避免重复采集）。
func (a *agent) loadPosition() error {
	a.position = position{}
	raw, err := os.ReadFile(a.cfg.PositionFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return json.Unmarshal(raw, &a.position)
}

// savePosition 持久化偏移量（原子替换，避免崩溃导致偏移丢失）。
func (a *agent) savePosition() error {
	if err := os.MkdirAll(filepath.Dir(a.cfg.PositionFile), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(a.position, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.cfg.PositionFile + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, a.cfg.PositionFile)
}

// run 启动采集循环。
func (a *agent) run(ctx context.Context, interval time.Duration) {
	a.collect(ctx)
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.collect(ctx)
		}
	}
}

// collect 执行一轮采集。
func (a *agent) collect(ctx context.Context) {
	for _, target := range a.cfg.Files {
		reports, err := a.readNewLines(target)
		if err != nil {
			fmt.Fprintf(a.logger, "[warn] 读取 %s 失败: %v\n", target.Path, err)
			continue
		}
		if len(reports) == 0 {
			continue
		}
		for _, report := range reports {
			if err := a.report(ctx, report); err != nil {
				fmt.Fprintf(a.logger, "[warn] 上报失败: %v\n", err)
				// 上报失败不回退偏移：下轮重新读取同一批数据，保证不丢日志。
				return
			}
		}
		if err := a.savePosition(); err != nil {
			fmt.Fprintf(a.logger, "[warn] 保存偏移量失败: %v\n", err)
		}
	}
}

// readNewLines 从上次偏移继续读取，返回需要上报的报告列表。
func (a *agent) readNewLines(target LogTarget) ([]LogReport, error) {
	file, err := os.Open(target.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	offset := a.position[target.Path]
	// 文件被轮转（变小）时从头开始读。
	if info.Size() < offset {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var (
		reports   []LogReport
		window    []string
		pending   string
		pendingLv string
		readAny   int64
	)
	for scanner.Scan() {
		line := scanner.Text()
		readAny += int64(len(line)) + 1
		window = append(window, line)
		if len(window) > a.cfg.ContextLines*2 {
			window = window[len(window)-a.cfg.ContextLines*2:]
		}
		if pending != "" && strings.HasPrefix(strings.TrimSpace(line), "at ") {
			// 续接堆栈行。
			pending += "\n" + line
			continue
		}
		if pending != "" {
			reports = append(reports, a.buildReport(target, pending, pendingLv, window))
			pending = ""
			pendingLv = ""
			if len(reports) >= a.cfg.MaxBatch {
				break
			}
		}
		if matched, level := matchLevel(line, target.LevelFilter); matched {
			pending = line
			pendingLv = level
		}
	}
	if pending != "" && len(reports) < a.cfg.MaxBatch {
		reports = append(reports, a.buildReport(target, pending, pendingLv, window))
	}
	if err := scanner.Err(); err != nil {
		return reports, err
	}
	// 仅当整文件读完（未因批量上限中断）时推进偏移。
	if len(reports) < a.cfg.MaxBatch {
		a.position[target.Path] = offset + readAny
	} else {
		a.position[target.Path] = offset + readAny
	}
	return reports, nil
}

// buildReport 组装上报体，附带错误前后 N 行上下文。
func (a *agent) buildReport(target LogTarget, message, level string, window []string) LogReport {
	if level == "" {
		level = "ERROR"
	}
	contextLines := window
	if len(contextLines) > a.cfg.ContextLines {
		contextLines = contextLines[len(contextLines)-a.cfg.ContextLines:]
	}
	report := LogReport{
		ServerName:   a.cfg.ServerName,
		Service:      target.Service,
		Level:        level,
		Message:      message,
		ContextLines: strings.Join(contextLines, "\n"),
		Timestamp:    time.Now().UTC().Format(time.RFC3339),
		AlertType:    target.AlertType,
	}
	if target.AlertType == "stack" || strings.HasPrefix(strings.TrimSpace(message), "at ") {
		report.Stacktrace = message
	} else {
		report.Stacktrace = strings.Join(contextLines, "\n")
	}
	// 单条消息截断，避免超出平台字段长度限制。
	if len(report.Message) > 4000 {
		report.Message = report.Message[:4000]
	}
	return report
}

// matchLevel 判断日志行是否达到采集等级，并返回识别到的等级。
//
// level_filter 的语义是「最低采集等级」：
//   - 缺省 / ERROR：只采集 ERROR / FATAL / PANIC / 异常堆栈 / Full GC；
//   - WARN：在上一档基础上额外采集 WARN；
//   - INFO / DEBUG：采集全部非空行——gc.log 这类日志本身没有等级字段，
//     必须用 INFO 才能采到。
//
// 早期实现只判断了 WARN 分支，level_filter: INFO 被静默忽略，导致
// agent.yaml 里配了 gc.log 却永远没有 GC 日志上报（配置写了不生效）。
// 同时上报体的 level 被硬编码为 ERROR，GC 日志会被误标成错误。
func matchLevel(line, filter string) (bool, string) {
	upper := strings.ToUpper(line)
	for _, level := range []string{"FATAL", "PANIC", "ERROR", "EXCEPTION"} {
		if strings.Contains(upper, level) {
			return true, level
		}
	}
	if strings.Contains(upper, "FULL GC") || strings.Contains(upper, "OUTOFMEMORY") {
		return true, "FATAL"
	}
	switch strings.ToUpper(strings.TrimSpace(filter)) {
	case "WARN":
		if strings.Contains(upper, "WARN") {
			return true, "WARN"
		}
	case "INFO", "DEBUG", "TRACE", "ALL":
		if strings.TrimSpace(line) != "" {
			return true, "INFO"
		}
	}
	return false, ""
}

// report 上报单条日志。
func (a *agent) report(ctx context.Context, report LogReport) error {
	payload, err := json.Marshal(report)
	if err != nil {
		return err
	}
	url := strings.TrimRight(a.cfg.PlatformURL, "/") + "/api/hooks/logs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if a.cfg.HookToken != "" {
		req.Header.Set("X-Hook-Token", a.cfg.HookToken)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("平台返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	fmt.Fprintf(a.logger, "[info] 上报成功 service=%s signature=%s\n", report.Service, extract("signature", body))
	return nil
}

// extract 从响应 JSON 中取字段（避免为 Agent 引入结构体依赖）。
func extract(key string, body []byte) string {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "-"
	}
	data, ok := parsed["data"].(map[string]any)
	if !ok {
		return "-"
	}
	if value, ok := data[key].(string); ok {
		return value
	}
	return "-"
}
