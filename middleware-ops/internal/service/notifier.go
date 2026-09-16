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
	cfg    *config.NotifyConfig
	appURL string
	store  cache.Store
	repo   *repository.NotificationLogRepository
	client *http.Client
	log    *zap.Logger
}

// NewNotifierService 构造通知服务。
func NewNotifierService(cfg *config.NotifyConfig, appURL string, store cache.Store, repo *repository.NotificationLogRepository, log *zap.Logger) *NotifierService {
	return &NotifierService{
		cfg: cfg, appURL: appURL, store: store, repo: repo,
		client: &http.Client{Timeout: 8 * time.Second},
		log:    log,
	}
}

// ChannelStatus 描述各渠道配置状态（不泄露 webhook 地址）。
func (s *NotifierService) ChannelStatus() []map[string]any {
	return []map[string]any{
		{"channel": "feishu", "enabled": s.cfg.Enabled && s.cfg.Feishu.Enabled && s.cfg.Feishu.Webhook != ""},
		{"channel": "wecom", "enabled": s.cfg.Enabled && s.cfg.WeCom.Enabled && s.cfg.WeCom.Webhook != ""},
		{"channel": "dingtalk", "enabled": s.cfg.Enabled && s.cfg.DingTalk.Enabled && s.cfg.DingTalk.Webhook != ""},
		{"channel": "email", "enabled": s.cfg.Enabled && s.cfg.Email.Enabled && s.cfg.Email.Host != ""},
	}
}

// NotifyAlert 发送告警通知。
func (s *NotifierService) NotifyAlert(ctx context.Context, alert *model.Alert, rule model.AlertRule, metric monitor.Metric) {
	if alert == nil || s.cfg == nil || !s.cfg.Enabled {
		return
	}
	title := fmt.Sprintf("【%s】%s", strings.ToUpper(defaultString(alert.AlertLevel, "warning")), instanceLabel(alert))
	content := alert.AlertMessage
	if alert.Count > 1 {
		content = fmt.Sprintf("%s\n（窗口内已合并 %d 次重复告警）", content, alert.Count)
	}
	detailURL := fmt.Sprintf("%s%s?alert_id=%d", s.appURL, defaultString(s.cfg.CardConfirmPath, "/alerts"), alert.ID)

	channels := rule.NotifyChannels
	if len(channels) == 0 {
		channels = model.JSONStringSlice{"feishu", "wecom"}
	}
	for _, channel := range channels {
		if !s.dedup(ctx, fmt.Sprintf("notify:%d:%s", alert.ID, channel)) {
			continue
		}
		s.send(ctx, AlertNotification{
			Channel:     string(channel),
			Title:       title,
			Content:     content,
			Level:       alert.AlertLevel,
			AlertID:     alert.ID,
			DetailURL:   detailURL,
			MetricName:  metric.DisplayName,
			MetricValue: fmt.Sprintf("%.4f%s", metric.Latest, metric.Unit),
		})
	}
}

// AlertNotification 是一条通知内容。
type AlertNotification struct {
	Channel     string `json:"channel"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	Level       string `json:"level"`
	AlertID     int64  `json:"alert_id"`
	DetailURL   string `json:"detail_url"`
	MetricName  string `json:"metric_name"`
	MetricValue string `json:"metric_value"`
}

// NotifyApproval 发送审批相关通知。
func (s *NotifierService) NotifyApproval(ctx context.Context, ticket *model.Approval, state string) {
	if ticket == nil || s.cfg == nil || !s.cfg.Enabled {
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

// sendFeishu 发送飞书消息卡片。
func (s *NotifierService) sendFeishu(ctx context.Context, n AlertNotification) error {
	cfg := s.cfg.Feishu
	if !cfg.Enabled || cfg.Webhook == "" {
		return fmt.Errorf("飞书渠道未启用")
	}
	template := "blue"
	if n.Level == model.AlertLevelCritical {
		template = "red"
	}
	payload := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"config": map[string]any{"wide_screen_mode": true},
			"header": map[string]any{
				"title":    map[string]any{"tag": "plain_text", "content": n.Title},
				"template": template,
			},
			"elements": []map[string]any{
				{"tag": "div", "text": map[string]any{"tag": "lark_md", "content": n.Content}},
				{"tag": "hr"},
				{"tag": "action", "actions": []map[string]any{
					{"tag": "button", "text": map[string]any{"tag": "plain_text", "content": "查看详情 / 确认"},
						"type": "primary", "url": n.DetailURL},
				}},
				{"tag": "note", "elements": []map[string]any{
					{"tag": "plain_text", "content": "高危操作不支持在 IM 中一键执行，请前往平台完成审批与执行"},
				}},
			},
		},
	}
	return s.postJSON(ctx, cfg.Webhook, payload, cfg.Secret, "feishu")
}

// sendWeCom 发送企业微信 markdown 消息。
func (s *NotifierService) sendWeCom(ctx context.Context, n AlertNotification) error {
	cfg := s.cfg.WeCom
	if !cfg.Enabled || cfg.Webhook == "" {
		return fmt.Errorf("企业微信渠道未启用")
	}
	content := fmt.Sprintf("**%s**\n> %s\n\n[查看详情并确认](%s)\n\n<font color=\"comment\">高危操作请在平台内审批执行</font>",
		n.Title, strings.ReplaceAll(n.Content, "\n", "\n> "), n.DetailURL)
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
	cfg := s.cfg.DingTalk
	if !cfg.Enabled || cfg.Webhook == "" {
		return fmt.Errorf("钉钉渠道未启用")
	}
	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]any{
			"title": n.Title,
			"text":  fmt.Sprintf("### %s\n\n%s\n\n[查看详情](%s)", n.Title, n.Content, n.DetailURL),
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
	cfg := s.cfg.Email
	if !cfg.Enabled || cfg.Host == "" {
		return fmt.Errorf("邮件渠道未启用")
	}
	if len(cfg.To) == 0 {
		return fmt.Errorf("邮件收件人为空")
	}
	subject := fmt.Sprintf("=?UTF-8?B?%s?=", base64.StdEncoding.EncodeToString([]byte(n.Title)))
	body := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n%s\r\n\r\n详情：%s\r\n",
		cfg.From, strings.Join(cfg.To, ","), subject, n.Content, n.DetailURL)
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	var auth smtp.Auth
	if cfg.Username != "" {
		auth = smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	}
	return smtp.SendMail(addr, auth, cfg.From, cfg.To, []byte(body))
}

// instanceLabel 生成告警对象标签。
func instanceLabel(alert *model.Alert) string {
	if alert.MWType != "" {
		return fmt.Sprintf("%s 实例 #%d", strings.ToUpper(alert.MWType), alert.InstanceID)
	}
	return fmt.Sprintf("实例 #%d", alert.InstanceID)
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
