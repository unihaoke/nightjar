package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/smtp"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/config"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/pkg/cache"
	"middleware-ops/internal/repository"
)

// NotifierService 实现多渠道通知（4.4）。
//
// 边界（6.2）：IM 卡片只做「通知 + 确认/驳回 + 查看详情」，
// **不做一键执行**，执行统一回 Web 端并叠加二次确认。
type NotifierService struct {
	// cfg 指向「当前生效的那一份通知配置」。
	//
	// 为什么是 atomic.Pointer 而不是直接持有 *config.NotifyConfig：「通知渠道」搬到平台界面后，
	// 保存动作会在**运行期**整份替换通知配置（见 UpdateConfig），而告警发送链路在其它 goroutine
	// 上读它。若就地逐字段改写，发送方可能拷到「半个旧配置 + 半个新配置」——Go 的 string/切片
	// 拷贝都不是原子的——轻则用新 webhook 配旧签名密钥（必然 401），重则把消息发到刚被换掉的
	// 群。改成「整份替换 + 原子读指针」后，读方永远拿到一份自洽的配置快照。
	cfg    atomic.Pointer[config.NotifyConfig]
	appURL string
	store  cache.Store
	repo   *repository.NotificationLogRepository
	client *http.Client
	log    *zap.Logger
}

// NewNotifierService 构造通知服务。
//
// 传进来的 cfg 会**被复制**后持有：服务只认自己这一份快照，不与 config.Config 里的字段共享
// 内存，避免"谁都能改到它"的隐性耦合（要改必须走 UpdateConfig）。
func NewNotifierService(cfg *config.NotifyConfig, appURL string, store cache.Store, repo *repository.NotificationLogRepository, log *zap.Logger) *NotifierService {
	s := &NotifierService{
		appURL: appURL, store: store, repo: repo,
		client: &http.Client{Timeout: 8 * time.Second},
		log:    log,
	}
	if cfg != nil {
		s.UpdateConfig(*cfg)
	} else {
		s.UpdateConfig(config.NotifyConfig{})
	}
	return s
}

// UpdateConfig 替换当前生效的通知配置（设置页保存后调用，立即生效，不需要重启）。
func (s *NotifierService) UpdateConfig(cfg config.NotifyConfig) {
	// To/Mentions 是切片：整份替换的是"快照指针"，但切片底层数组仍可能与调用方共享。
	// 拷贝一份，保证快照之后不会被任何人从外部改写。
	cfg.Email.To = append([]string(nil), cfg.Email.To...)
	cfg.Feishu.Mentions = append([]string(nil), cfg.Feishu.Mentions...)
	cfg.WeCom.Mentions = append([]string(nil), cfg.WeCom.Mentions...)
	cfg.DingTalk.Mentions = append([]string(nil), cfg.DingTalk.Mentions...)
	s.cfg.Store(&cfg)
}

// current 取当前生效配置；未初始化时返回零值（等价于「所有渠道未启用」），调用方不必判 nil。
func (s *NotifierService) current() config.NotifyConfig {
	if cfg := s.cfg.Load(); cfg != nil {
		return *cfg
	}
	return config.NotifyConfig{}
}

// ChannelStatus 描述各渠道配置状态（不泄露 webhook 地址）。
func (s *NotifierService) ChannelStatus() []map[string]any {
	cfg := s.current()
	return []map[string]any{
		{"channel": "feishu", "enabled": cfg.Enabled && cfg.Feishu.Enabled && cfg.Feishu.Webhook != ""},
		{"channel": "wecom", "enabled": cfg.Enabled && cfg.WeCom.Enabled && cfg.WeCom.Webhook != ""},
		{"channel": "dingtalk", "enabled": cfg.Enabled && cfg.DingTalk.Enabled && cfg.DingTalk.Webhook != ""},
		{"channel": "email", "enabled": cfg.Enabled && cfg.Email.Enabled && cfg.Email.Host != ""},
	}
}

// NotifyAlert 发送告警通知。
func (s *NotifierService) NotifyAlert(ctx context.Context, alert *model.Alert, rule model.AlertRule, metric monitor.Metric) {
	cfg := s.current()
	if alert == nil || !cfg.Enabled {
		return
	}
	// 未勾选任何通知渠道 = 不发送（空即静默），不再兜底飞书/企微。
	// 平台「通知渠道」页面负责配置各渠道，规则只决定「分配哪些渠道」，
	// 两者解耦：管理员清空勾选即表示这条规则不需要 IM 通知。
	channels := rule.NotifyChannels
	if len(channels) == 0 {
		return
	}
	n := s.alertNotification(alert, rule, metric)
	for _, channel := range channels {
		if !s.dedup(ctx, fmt.Sprintf("notify:%d:%s", alert.ID, channel)) {
			continue
		}
		n.Channel = string(channel)
		s.send(ctx, n)
	}
}

// NotifyAlertDiagnosis 在自动 AI 诊断完成后，按规则勾选的渠道追发诊断结论。
//
// 为什么不合并进第一条告警消息：诊断要跑一次 LLM（秒级到分钟级），而告警通知必须秒级触达，
// 让 Ingest/Evaluate 阻塞等待会导致告警通道整体变慢甚至超时。因此拆成「先诊断、后外发」，
// 诊断跑完才轮到这里的渠道判断。
//
// 两个开关职责互斥，此处只认第二个：
//   - ai_enabled     → 决定是否跑诊断（在 AlertService.Trigger 里判断）；
//   - notify_channels → 决定跑完之后是否外发（在这里判断）。
//
// 渠道来源一律是规则上的勾选结果，「没勾选 = 静默」，不做任何兜底/默认渠道。
func (s *NotifierService) NotifyAlertDiagnosis(ctx context.Context, alert *model.Alert, channels []string, brief *DiagnosisBrief) {
	cfg := s.current()
	if alert == nil || brief == nil || !cfg.Enabled {
		return
	}
	// 未勾选任何渠道：诊断结论只入库、不外发。
	if len(channels) == 0 {
		return
	}
	n := AlertNotification{
		Title:     fmt.Sprintf("【AI 诊断】%s", instanceLabel(alert)),
		Content:   alertContent(alert),
		Level:     alert.AlertLevel,
		AlertID:   alert.ID,
		DetailURL: s.alertDetailURL(alert.ID),
		Diagnosis: brief,
	}
	for _, channel := range channels {
		// 去重键必须与告警通知区分：notify:<id> 已被第一条消息用掉且 TTL 10 分钟，
		// 沿用会被判定成重复而静默丢弃，结论就永远发不出去。
		if !s.dedup(ctx, fmt.Sprintf("notify-diag:%d:%s", alert.ID, channel)) {
			continue
		}
		n.Channel = string(channel)
		s.send(ctx, n)
	}
}

// alertNotification 构造一条告警通知（各渠道共用）。
func (s *NotifierService) alertNotification(alert *model.Alert, rule model.AlertRule, metric monitor.Metric) AlertNotification {
	title := fmt.Sprintf("【%s】%s", levelLabel(alert.AlertLevel), instanceLabel(alert))
	return AlertNotification{
		Title:     title,
		Content:   alertContent(alert),
		Level:     alert.AlertLevel,
		AlertID:   alert.ID,
		DetailURL: s.alertDetailURL(alert.ID),
		Fields:    buildAlertFields(alert, rule, metric),
	}
}

// alertDetailURL 生成告警详情地址。
func (s *NotifierService) alertDetailURL(alertID int64) string {
	cfg := s.current()
	return fmt.Sprintf("%s%s?alert_id=%d", s.appURL, defaultString(cfg.CardConfirmPath, "/alerts"), alertID)
}

// alertContent 生成告警正文（含窗口合并提示）。
func alertContent(alert *model.Alert) string {
	content := alert.AlertMessage
	if alert.Count > 1 {
		content = fmt.Sprintf("%s\n（窗口内已合并 %d 次重复告警）", content, alert.Count)
	}
	return content
}

// buildAlertFields 生成告警的结构化字段。
//
// IM 里要能「一眼判断要不要处理」：对象、级别、触发条件、当前值、持续时间缺一不可，
// 否则值班同学还得点进平台查一遍，通知就失去了意义。
func buildAlertFields(alert *model.Alert, rule model.AlertRule, metric monitor.Metric) []NotificationField {
	unit := metric.Unit
	fields := []NotificationField{
		{Key: "告警对象", Value: instanceLabel(alert), Short: true},
		{Key: "级别", Value: levelLabel(alert.AlertLevel), Short: true},
	}
	if rule.Name != "" {
		fields = append(fields, NotificationField{Key: "触发规则", Value: rule.Name, Short: true})
	}
	name := metric.DisplayName
	if name == "" {
		name = metric.Name
	}
	if name != "" {
		fields = append(fields, NotificationField{
			Key:   "触发条件",
			Value: fmt.Sprintf("%s %s %g%s", name, rule.Operator, rule.Threshold, unit),
			Short: true,
		})
	}
	fields = append(fields, NotificationField{
		Key: "当前值", Value: fmt.Sprintf("%.4f%s", metric.Latest, unit), Short: true,
	})
	if alert.Count > 1 {
		fields = append(fields, NotificationField{
			Key: "窗口合并", Value: fmt.Sprintf("%d 次", alert.Count), Short: true,
		})
	}
	if alert.ID > 0 {
		fields = append(fields, NotificationField{
			Key: "告警 ID", Value: fmt.Sprintf("#%d", alert.ID), Short: true,
		})
	}
	if !alert.TriggeredAt.IsZero() {
		fields = append(fields, NotificationField{
			Key: "触发时间", Value: alert.TriggeredAt.Local().Format("2006-01-02 15:04:05"), Short: true,
		})
	}
	return fields
}

// levelLabel 把告警级别翻译成中文结论标签（IM 里不暴露英文字面量）。
func levelLabel(level string) string {
	switch level {
	case model.AlertLevelCritical:
		return "严重"
	case model.AlertLevelWarning:
		return "警告"
	default:
		return level
	}
}

// AlertNotification 是一条通知内容。
type AlertNotification struct {
	Channel   string              `json:"channel"`
	Title     string              `json:"title"`
	Content   string              `json:"content"`
	Level     string              `json:"level"`
	AlertID   int64               `json:"alert_id"`
	DetailURL string              `json:"detail_url"`
	Fields    []NotificationField `json:"fields,omitempty"`
	// Diagnosis 非空表示这条消息携带 AI 诊断结论（告警触发的自动诊断产出）。
	Diagnosis *DiagnosisBrief `json:"diagnosis,omitempty"`
}

// NotificationField 是卡片里的一行结构化字段。
type NotificationField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Short 为 true 时与相邻的 short 字段并排成双列（飞书 is_short），false 独占整行。
	Short bool `json:"short"`
}

// DiagnosisBrief 是随通知外发的 AI 诊断摘要。
//
// 边界（6.2）：只携带结论数据用于展示，不携带可执行动作——修复执行必须回平台走审批。
type DiagnosisBrief struct {
	DiagnosisID int64    `json:"diagnosis_id"`
	RootCause   string   `json:"root_cause"`
	Confidence  float64  `json:"confidence"`
	Suggestions []string `json:"suggestions"`
	ImpactScope string   `json:"impact_scope"`
	EngineUsed  string   `json:"engine_used"`
	DurationMS  int64    `json:"duration_ms"`
	// Degraded 为 true 表示 AI 引擎不可用，结论来自规则引擎降级。
	Degraded bool `json:"degraded"`
	// Speculative 为 true 表示结论缺少证据支撑，属推测（质量护栏标注）。
	Speculative bool `json:"speculative"`
}

// NotifyApproval 发送审批相关通知。
func (s *NotifierService) NotifyApproval(ctx context.Context, ticket *model.Approval, state string) {
	cfg := s.current()
	if ticket == nil || !cfg.Enabled {
		return
	}
	stateLabel := map[string]string{
		"created":  "待审批",
		"approved": "已通过",
		"rejected": "已驳回",
	}[state]
	if stateLabel == "" {
		stateLabel = state
	}
	title := fmt.Sprintf("【审批·%s】%s", stateLabel, ticket.ActionType)
	content := fmt.Sprintf("工单号：%s\n环境：%s\n动作：%s\n申请理由：%s\n到期时间：%s",
		ticket.TicketID, ticket.Environment, ticket.ActionType, defaultString(ticket.Reason, "未填写"),
		ticket.ExpiresAt.Format("2006-01-02 15:04:05"))
	detailURL := fmt.Sprintf("%s/system/approvals?ticket=%s", s.appURL, ticket.TicketID)
	for _, channel := range []string{"feishu", "wecom"} {
		if !s.dedup(ctx, fmt.Sprintf("approval:%s:%s:%s", ticket.TicketID, state, channel)) {
			continue
		}
		s.send(ctx, AlertNotification{
			Channel: channel, Title: title, Content: content,
			Level: "critical", DetailURL: detailURL,
		})
	}
}

// send 按渠道发送消息。
func (s *NotifierService) send(ctx context.Context, n AlertNotification) {
	var err error
	switch n.Channel {
	case "feishu":
		err = s.sendFeishu(ctx, n)
	case "wecom":
		err = s.sendWeCom(ctx, n)
	case "dingtalk":
		err = s.sendDingTalk(ctx, n)
	case "email":
		err = s.sendEmail(n)
	default:
		err = fmt.Errorf("未知通知渠道 %s", n.Channel)
	}
	s.recordLog(ctx, n, err)
}

// recordLog 记录通知结果。
func (s *NotifierService) recordLog(ctx context.Context, n AlertNotification, sendErr error) {
	status := "success"
	errText := ""
	if sendErr != nil {
		status = "failed"
		errText = sendErr.Error()
		s.log.Warn("通知发送失败", zap.String("channel", n.Channel), zap.Error(sendErr))
	}
	if s.repo == nil {
		return
	}
	now := time.Now().UTC()
	entry := &model.NotificationLog{
		AlertID: n.AlertID, Channel: n.Channel, Target: n.DetailURL,
		Content: n.Title + "\n" + n.Content, Status: status, Error: errText, SentAt: &now,
	}
	if err := s.repo.Create(ctx, entry); err != nil {
		s.log.Warn("写入通知日志失败", zap.Error(err))
	}
}

// dedup 通知去重（同一告警同一渠道 10 分钟内只发一次）。
func (s *NotifierService) dedup(ctx context.Context, key string) bool {
	if s.store == nil {
		return true
	}
	exists, err := s.store.Exists(ctx, key)
	if err == nil && exists {
		return false
	}
	if err := s.store.Set(ctx, key, "1", 10*time.Minute); err != nil {
		s.log.Warn("写入通知去重键失败", zap.Error(err))
	}
	return true
}

// sendFeishu 发送飞书交互卡片。
//
// 卡片结构：告警正文 → 结构化字段（双列）→ AI 诊断结论（可选）→ 操作按钮 → 边界说明。
// 之所以用 fields 而不是把全部信息拼成一坨 lark_md 文本：前者在飞书里对齐成两列，
// 值班同学在手机上一屏能看完「对象/级别/触发条件/当前值」，不用横向拖动。
func (s *NotifierService) sendFeishu(ctx context.Context, n AlertNotification) error {
	cfg := s.current().Feishu
	if !cfg.Enabled || cfg.Webhook == "" {
		return fmt.Errorf("飞书渠道未启用")
	}
	template := "blue"
	switch n.Level {
	case model.AlertLevelCritical:
		template = "red"
	case model.AlertLevelWarning:
		// warning 走橙色：原来非 critical 一律用 blue，严重与警告在群里颜色相同，
		// 扫一眼分不出轻重。
		template = "orange"
	default:
		template = "blue"
	}

	elements := []map[string]any{
		{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": n.Content}},
	}
	elements = append(elements, buildFeishuFields(n.Fields)...)
	if n.Diagnosis != nil {
		elements = append(elements, buildFeishuDiagnosis(n.Diagnosis)...)
	}

	actions := []map[string]any{
		{"tag": "button", "text": map[string]any{"tag": "plain_text", "content": "查看详情 / 确认"},
			"type": "primary", "url": n.DetailURL},
	}
	note := "IM 卡片仅做通知与确认；高危操作请前往平台审批后执行"
	if n.Diagnosis != nil {
		note = "诊断结论仅供参考，请人工复核后决策；高危操作需回平台走审批"
	}
	elements = append(elements,
		map[string]any{"tag": "hr"},
		map[string]any{"tag": "action", "actions": actions},
		map[string]any{"tag": "note", "elements": []map[string]any{
			{"tag": "plain_text", "content": note},
		}},
	)

	payload := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config": map[string]any{"wide_screen_mode": true},
			"header": map[string]any{
				"title":    map[string]any{"tag": "plain_text", "content": n.Title},
				"template": template,
			},
			"elements": elements,
		},
	}
	return s.postJSON(ctx, cfg.Webhook, payload, cfg.Secret, "feishu")
}

// buildFeishuFields 把结构化字段切成若干 div.fields。
//
// 飞书 fields 元素最多两列，且 is_short 必须成组出现：奇数个 short 字段会被拉伸成整行，
// 与其他行的列宽不一致。这里统一两个一组切分，保证列宽稳定。
func buildFeishuFields(fields []NotificationField) []map[string]any {
	var (
		elements []map[string]any
		pending  []map[string]any
	)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		elements = append(elements, map[string]any{"tag": "div", "fields": pending})
		pending = nil
	}
	for _, f := range fields {
		if f.Value == "" {
			continue
		}
		item := map[string]any{
			"is_short": f.Short,
			"text": map[string]any{
				"tag":     "lark_md",
				"content": fmt.Sprintf("**%s**\n%s", f.Key, f.Value),
			},
		}
		if !f.Short {
			flush()
			elements = append(elements, map[string]any{"tag": "div", "fields": []map[string]any{item}})
			continue
		}
		pending = append(pending, item)
		if len(pending) == 2 {
			flush()
		}
	}
	flush()
	return elements
}

// buildFeishuDiagnosis 渲染 AI 诊断结论区块。
func buildFeishuDiagnosis(d *DiagnosisBrief) []map[string]any {
	head := fmt.Sprintf("🤖 **AI 诊断结论**（置信度 %.0f%%", d.Confidence*100)
	if d.EngineUsed != "" {
		head += fmt.Sprintf(" · %s", d.EngineUsed)
	}
	if d.DurationMS > 0 {
		head += fmt.Sprintf(" · %.1fs", float64(d.DurationMS)/1000)
	}
	head += "）"

	elements := []map[string]any{
		{"tag": "hr"},
		{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": head}},
		{"tag": "div", "text": map[string]any{"tag": "lark_md",
			"content": fmt.Sprintf("**根因**：%s", truncateRunes(d.RootCause, 300))}},
	}
	if d.ImpactScope != "" {
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md",
			"content": fmt.Sprintf("**影响范围**：%s", truncateRunes(d.ImpactScope, 200))}})
	}
	if list := formatSuggestions(d.Suggestions); list != "" {
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": list}})
	}
	// 降级与推测必须显式标注：不让值班同学把不可靠结论当成事实去执行。
	switch {
	case d.Degraded:
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md",
			"content": "⚠️ AI 引擎不可用，以上为规则引擎降级结论"}})
	case d.Speculative:
		elements = append(elements, map[string]any{"tag": "div", "text": map[string]any{"tag": "lark_md",
			"content": "⚠️ 结论缺少证据支撑，属推测，请人工复核"}})
	}
	return elements
}

// formatSuggestions 渲染处置建议列表（最多 5 条，避免卡片过长被折叠）。
func formatSuggestions(suggestions []string) string {
	max := 5
	if len(suggestions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**处置建议**：")
	for i, item := range suggestions {
		if i >= max {
			break
		}
		b.WriteString(fmt.Sprintf("\n%d. %s", i+1, truncateRunes(item, 160)))
	}
	return b.String()
}

// sendWeCom 发送企业微信 markdown 消息。
func (s *NotifierService) sendWeCom(ctx context.Context, n AlertNotification) error {
	cfg := s.current().WeCom
	if !cfg.Enabled || cfg.Webhook == "" {
		return fmt.Errorf("企业微信渠道未启用")
	}
	// warning 色（橙）用于严重级别，让群聊里一眼能区分轻重。
	color := "comment"
	if n.Level == model.AlertLevelCritical {
		color = "warning"
	}
	content := fmt.Sprintf("**%s**\n> %s\n\n[查看详情并确认](%s)\n\n<font color=\"%s\">高危操作请在平台内审批执行</font>",
		n.Title, strings.ReplaceAll(n.renderText(), "\n", "\n> "), n.DetailURL, color)
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"content": content,
		},
	}
	return s.postJSON(ctx, cfg.Webhook, payload, cfg.Secret, "wecom")
}

// sendDingTalk 发送钉钉 markdown 消息（备选渠道）。
func (s *NotifierService) sendDingTalk(ctx context.Context, n AlertNotification) error {
	cfg := s.current().DingTalk
	if !cfg.Enabled || cfg.Webhook == "" {
		return fmt.Errorf("钉钉渠道未启用")
	}
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"title": n.Title,
			"text":  fmt.Sprintf("### %s\n\n%s\n\n[查看详情](%s)", n.Title, n.renderText(), n.DetailURL),
		},
	}
	return s.postJSON(ctx, cfg.Webhook, payload, cfg.Secret, "dingtalk")
}

// postJSON 发送 JSON 请求，并按渠道附加签名。
func (s *NotifierService) postJSON(ctx context.Context, endpoint string, payload any, secret, channel string) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	url := endpoint
	if secret != "" {
		switch channel {
		case "feishu":
			// 飞书自定义机器人签名：以 secret 为密钥，对 "<timestamp>\n<secret>" 做 HMAC-SHA256 后 base64。
			ts := time.Now().Unix()
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(fmt.Sprintf("%d\n%s", ts, secret)))
			url = fmt.Sprintf("%s&timestamp=%d&sign=%s", endpoint, ts,
				base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		case "dingtalk":
			ts := time.Now().UnixMilli()
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write([]byte(fmt.Sprintf("%d\n%s", ts, secret)))
			url = fmt.Sprintf("%s&timestamp=%d&sign=%s", endpoint, ts,
				base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s 返回状态码 %d", channel, resp.StatusCode)
	}
	return nil
}

// sendEmail 发送邮件通知（net/smtp）。
func (s *NotifierService) sendEmail(n AlertNotification) error {
	cfg := s.current().Email
	if !cfg.Enabled || cfg.Host == "" {
		return fmt.Errorf("邮件渠道未启用")
	}
	if len(cfg.To) == 0 {
		return fmt.Errorf("邮件收件人为空")
	}
	subject := fmt.Sprintf("=?UTF-8?B?%s?=", base64.StdEncoding.EncodeToString([]byte(n.Title)))
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n\r\n详情：%s\r\n",
		cfg.From, strings.Join(cfg.To, ","), subject, n.renderText(), n.DetailURL)
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	return smtp.SendMail(addr, auth, cfg.From, cfg.To, []byte(body))
}

// renderText 渲染统一正文（企微/钉钉/邮件共用）。
//
// 这些渠道没有卡片能力，结构化字段降级为「key：value」逐行罗列，诊断结论降级为分节文本，
// 保证无论走哪个渠道，值班同学拿到的信息面是一致的。
func (n AlertNotification) renderText() string {
	var b strings.Builder
	b.WriteString(n.Content)
	if len(n.Fields) > 0 {
		b.WriteString("\n")
		for _, f := range n.Fields {
			if f.Value == "" {
				continue
			}
			b.WriteString(fmt.Sprintf("\n%s：%s", f.Key, f.Value))
		}
	}
	b.WriteString(diagnosisText(n.Diagnosis))
	return b.String()
}

// diagnosisText 渲染诊断摘要（文本渠道版，限制建议条数与长度避免消息被截断）。
func diagnosisText(d *DiagnosisBrief) string {
	if d == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n—— AI 诊断结论 ——")
	b.WriteString(fmt.Sprintf("\n置信度：%.0f%%", d.Confidence*100))
	if d.EngineUsed != "" {
		b.WriteString(fmt.Sprintf(" · 引擎：%s", d.EngineUsed))
	}
	if d.DurationMS > 0 {
		b.WriteString(fmt.Sprintf(" · 耗时 %.1fs", float64(d.DurationMS)/1000))
	}
	b.WriteString(fmt.Sprintf("\n根因：%s", truncateRunes(d.RootCause, 300)))
	if d.ImpactScope != "" {
		b.WriteString(fmt.Sprintf("\n影响范围：%s", truncateRunes(d.ImpactScope, 200)))
	}
	for i, item := range d.Suggestions {
		if i >= 5 {
			break
		}
		b.WriteString(fmt.Sprintf("\n%d. %s", i+1, truncateRunes(item, 160)))
	}
	if d.Degraded {
		b.WriteString("\n⚠️ AI 引擎不可用，以上为规则引擎降级结论")
	} else if d.Speculative {
		b.WriteString("\n⚠️ 结论缺少证据支撑，属推测，请人工复核")
	}
	return b.String()
}

// instanceLabel 生成告警对象标签。
func instanceLabel(alert *model.Alert) string {
	if alert.MWType != "" {
		return fmt.Sprintf("%s 实例 #%d", strings.ToUpper(alert.MWType), alert.InstanceID)
	}
	return fmt.Sprintf("实例 #%d", alert.InstanceID)
}

// ReadyForTest 预检渠道是否具备发送条件（设置页「测试」按钮前的自检）。
//
// 为什么需要单独一个预检：send() 把发送失败写进通知日志后**吞掉错误**（告警通知不能因为
// 某一个渠道失败而中断整条链路），于是 SendTest 永远返回 nil——界面点「测试」会显示成功，
// 实际一条都没发出去。这里把「渠道没启用 / 没配 webhook / 收件人为空」这类确定性配置问题
// 提前暴露给配置人。
func (s *NotifierService) ReadyForTest(channel string) error {
	cfg := s.current()
	if !cfg.Enabled {
		return fmt.Errorf("通知总开关未启用：请先在通知设置里打开「启用通知」")
	}
	switch channel {
	case "feishu":
		if !cfg.Feishu.Enabled || cfg.Feishu.Webhook == "" {
			return fmt.Errorf("飞书渠道未启用或未配置 webhook")
		}
	case "wecom":
		if !cfg.WeCom.Enabled || cfg.WeCom.Webhook == "" {
			return fmt.Errorf("企业微信渠道未启用或未配置 webhook")
		}
	case "dingtalk":
		if !cfg.DingTalk.Enabled || cfg.DingTalk.Webhook == "" {
			return fmt.Errorf("钉钉渠道未启用或未配置 webhook")
		}
	case "email":
		if !cfg.Email.Enabled || cfg.Email.Host == "" {
			return fmt.Errorf("邮件渠道未启用或未配置 SMTP 主机")
		}
		if len(cfg.Email.To) == 0 {
			return fmt.Errorf("邮件收件人为空")
		}
	default:
		return fmt.Errorf("未知通知渠道 %s", channel)
	}
	return nil
}

// SendTest 发送测试消息（系统配置页自检）。
func (s *NotifierService) SendTest(ctx context.Context, channel string) error {
	s.send(ctx, AlertNotification{
		Channel: channel, Title: "【测试】中间件智能问题解决平台通知自检",
		Content:   "如果你收到这条消息，说明该渠道配置正确。",
		Level:     "warning",
		DetailURL: s.appURL,
	})
	return nil
}
