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
	title := fmt.Sprintf("【%s】%s", strings.ToUpper(defaultString(alert.AlertLevel, "warning")), instanceLabel(alert))
	content := alert.AlertMessage
	if alert.Count > 1 {
		content = fmt.Sprintf("%s\n（窗口内已合并 %d 次重复告警）", content, alert.Count)
	}
	detailURL := fmt.Sprintf("%s%s?alert_id=%d", s.appURL, defaultString(cfg.CardConfirmPath, "/alerts"), alert.ID)

	// 未勾选任何通知渠道 = 不发送（空即静默），不再兜底飞书/企微。
	// 平台「通知渠道」页面负责配置各渠道，规则只决定「分配哪些渠道」，
	// 两者解耦：管理员清空勾选即表示这条规则不需要 IM 通知。
	channels := rule.NotifyChannels
	if len(channels) == 0 {
		return
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

// sendFeishu 发送飞书消息卡片。
func (s *NotifierService) sendFeishu(ctx context.Context, n AlertNotification) error {
	cfg := s.current().Feishu
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
	cfg := s.current().WeCom
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
	cfg := s.current().DingTalk
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
	cfg := s.current().Email
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
