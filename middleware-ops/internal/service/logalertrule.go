package service

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
)

// 本文件实现「日志告警规则」：把"谁能触发、合并多久、冷却多久、要不要通知/分析"从代码里
// 搬到页面可配置（用户明确要求："规则也是需要设置对应的去重窗口，冷却期等"）。
//
// 判定输入与指标告警规则不同：指标比数值，日志比**服务 + 错误指纹 + 级别**。
// 因此单独一张 log_alert_rules 表；但"去重窗口 / 冷却期 / 通知渠道 / AI 开关"四个概念
// 与指标规则同名同语义，使用者在两个页面看到的是同一套心智模型。

// 可用通知渠道（与 notifier 支持的渠道一致；留空表示用平台默认渠道）。
var logRuleChannels = map[string]bool{"feishu": true, "wecom": true, "dingtalk": true, "email": true}

// LogAlertRuleInput 是规则的写入入参。
type LogAlertRuleInput struct {
	Name        string `json:"name" binding:"required,min=1,max=128"`
	Description string `json:"description"`
	// ServiceName 为空表示匹配任意服务。
	ServiceName string `json:"service_name"`
	// SignaturePattern 为空表示匹配任意指纹；`/re/` 形式按正则，其余按子串。
	SignaturePattern string `json:"signature_pattern"`
	// MinSeverity 为空表示不限级别（INFO/WARN/ERROR/FATAL）。
	MinSeverity string `json:"min_severity"`
	// DedupWindow 为去重窗口（分钟）；0 表示不合并。
	//
	// 用指针是为了区分"没传"和"显式传 0"：更新规则时若把省略当成 0，
	// 会悄悄把去重关掉（下一次日志风暴就把人淹了），而使用者只改了规则名。
	DedupWindow *int `json:"dedup_window"`
	// Cooldown 为冷却期（分钟）；0 表示不冷却（每条都通知/分析）。
	Cooldown       *int     `json:"cooldown"`
	NotifyChannels []string `json:"notify_channels"`
	// AIEnabled / Enabled 用指针：未传时新建用默认值（开），更新时保持原值。
	AIEnabled *bool `json:"ai_enabled"`
	Enabled   *bool `json:"enabled"`
	Priority  int   `json:"priority"`
}

// ---------------------------------------------------------------------------
// 匹配（纯函数，可单测）
// ---------------------------------------------------------------------------

// severityRank 把级别映射成可比较的序号（越大越严重）。
//
// 未知级别按 ERROR 处理：能进日志告警链路的本来就是错误（Filebeat 输入端默认只放行 ERROR+），
// 把它当成 INFO 会让规则静默失效，那是最难查的一类问题。
func severityRank(level string) int {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "INFO", "DEBUG", "TRACE":
		return 1
	case "WARN", "WARNING":
		return 2
	case "ERROR", "SEVERE", "EXCEPTION":
		return 3
	case "FATAL", "PANIC", "CRITICAL":
		return 4
	default:
		return 3
	}
}

// signatureMatches 判断指纹是否匹配规则里的 pattern。
//
//	""        → 任意
//	"/re/"    → 正则（大小写不敏感；编译失败按子串处理，绝不 panic——
//	            规则写错不该让整条日志链路挂掉）
//	其它      → 子串（大小写不敏感：使用者很难记住指纹的确切大小写）
func signatureMatches(pattern, signature string) bool {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return true
	}
	if len(pattern) >= 2 && strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") {
		expr := pattern[1 : len(pattern)-1]
		if re, err := regexp.Compile("(?i)" + expr); err == nil {
			return re.MatchString(signature)
		}
		// 正则非法：退化为子串匹配（并保留斜杠外的原文），比"规则失效"更符合直觉。
		pattern = strings.Trim(pattern, "/")
	}
	return strings.Contains(strings.ToLower(signature), strings.ToLower(pattern))
}

// MatchLogAlertRule 取第一条命中的启用规则。
//
// 优先级的语义：priority 数字**小**的优先；同优先级按 id 升序（先建的先匹配）。
// 调用方传入的 rules 通常已按此顺序排好（仓储层保证了顺序），这里再排一次，
// 使这个函数不依赖调用顺序——它是纯函数，单测里可以随便给乱序切片。
func MatchLogAlertRule(rules []model.LogAlertRule, service, signature, level string) (model.LogAlertRule, bool) {
	ordered := make([]model.LogAlertRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Enabled {
			ordered = append(ordered, rule)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority < ordered[j].Priority
		}
		return ordered[i].ID < ordered[j].ID
	})

	want := severityRank(level)
	for _, rule := range ordered {
		if serviceFilter := strings.TrimSpace(rule.ServiceName); serviceFilter != "" &&
			!strings.EqualFold(serviceFilter, strings.TrimSpace(service)) {
			continue
		}
		if rule.MinSeverity != "" && want < severityRank(rule.MinSeverity) {
			continue
		}
		if !signatureMatches(rule.SignaturePattern, signature) {
			continue
		}
		return rule, true
	}
	return model.LogAlertRule{}, false
}

// ---------------------------------------------------------------------------
// 规则读写
// ---------------------------------------------------------------------------

// ListRules 分页检索规则。
func (s *LogAlertService) ListRules(ctx context.Context, keyword string, limit, offset int) ([]model.LogAlertRule, int64, error) {
	if s.rules == nil {
		return nil, 0, apperr.New(apperr.CodeInternal, "日志告警规则仓储未装配")
	}
	items, total, err := s.rules.List(ctx, keyword, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// CreateRule 新建规则。
func (s *LogAlertService) CreateRule(ctx context.Context, in LogAlertRuleInput, operator Operator) (*model.LogAlertRule, error) {
	// 新建时的基线：开关默认开、窗口与冷却给一组合理初值——
	// 这样"只填名字就保存"也能得到一条语义合理的规则，而不是 window=0/cooldown=0 的"每条都通知"。
	base := model.LogAlertRule{DedupWindow: 5, Cooldown: 10, AIEnabled: true, Enabled: true}
	rule, err := s.buildRule(base, in)
	if err != nil {
		return nil, err
	}
	if err := s.rules.Create(ctx, rule); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.invalidateRuleCache()
	s.recordRule(ctx, operator, "log_alert_rule_create", rule)
	return rule, nil
}

// UpdateRule 更新规则（未传的开关保持原值，避免"编辑一次把 AI 悄悄关了"）。
func (s *LogAlertService) UpdateRule(ctx context.Context, id int64, in LogAlertRuleInput, operator Operator) (*model.LogAlertRule, error) {
	existing, err := s.rules.Get(ctx, id)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, err)
	}
	rule, err := s.buildRule(*existing, in)
	if err != nil {
		return nil, err
	}
	rule.ID = id
	if err := s.rules.Update(ctx, rule); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.invalidateRuleCache()
	s.recordRule(ctx, operator, "log_alert_rule_update", rule)
	return rule, nil
}

// DeleteRule 删除规则。
func (s *LogAlertService) DeleteRule(ctx context.Context, id int64, operator Operator) error {
	if err := s.rules.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeNotFound, err)
	}
	s.invalidateRuleCache()
	s.recordRule(ctx, operator, "log_alert_rule_delete", &model.LogAlertRule{Base: model.Base{ID: id}})
	return nil
}

// buildRule 校验入参并组装规则（base 为原值，更新时用于保留未传字段）。
func (s *LogAlertService) buildRule(base model.LogAlertRule, in LogAlertRuleInput) (*model.LogAlertRule, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "规则名不能为空")
	}
	if len(name) > 128 {
		return nil, apperr.New(apperr.CodeInvalidParam, "规则名不能超过 128 个字符")
	}
	if in.DedupWindow != nil && *in.DedupWindow < 0 {
		return nil, apperr.New(apperr.CodeInvalidParam, "去重窗口不能为负数（0 表示不合并）")
	}
	if in.Cooldown != nil && *in.Cooldown < 0 {
		return nil, apperr.New(apperr.CodeInvalidParam, "冷却期不能为负数（0 表示不冷却）")
	}
	severity := strings.ToUpper(strings.TrimSpace(in.MinSeverity))
	switch severity {
	case "", "INFO", "WARN", "ERROR", "FATAL":
	default:
		return nil, apperr.Newf(apperr.CodeInvalidParam,
			"最低级别 %q 非法（可选 INFO/WARN/ERROR/FATAL，留空表示不限）", in.MinSeverity)
	}
	if pattern := strings.TrimSpace(in.SignaturePattern); strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") && len(pattern) > 2 {
		if _, err := regexp.Compile(pattern[1 : len(pattern)-1]); err != nil {
			return nil, apperr.Newf(apperr.CodeInvalidParam, "指纹正则非法：%v（也可直接填普通文本做子串匹配）", err)
		}
	}
	for _, channel := range in.NotifyChannels {
		if !logRuleChannels[strings.TrimSpace(channel)] {
			return nil, apperr.Newf(apperr.CodeInvalidParam,
				"通知渠道 %q 非法（可选 feishu/wecom/dingtalk/email）", channel)
		}
	}

	priority := in.Priority
	if priority <= 0 {
		priority = 100 // 默认优先级：不写就用它，保证"特例规则"可以填更小的值压过通用规则
	}
	aiEnabled := base.AIEnabled
	if in.AIEnabled != nil {
		aiEnabled = *in.AIEnabled
	}
	enabled := base.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	// 窗口/冷却：未传则沿用原值（更新场景），显式传 0 才表示"关闭去重/冷却"。
	dedupWindow := base.DedupWindow
	if in.DedupWindow != nil {
		dedupWindow = *in.DedupWindow
	}
	cooldown := base.Cooldown
	if in.Cooldown != nil {
		cooldown = *in.Cooldown
	}

	return &model.LogAlertRule{
		Name:             name,
		Description:      strings.TrimSpace(in.Description),
		ServiceName:      strings.TrimSpace(in.ServiceName),
		SignaturePattern: strings.TrimSpace(in.SignaturePattern),
		MinSeverity:      severity,
		DedupWindow:      dedupWindow,
		Cooldown:         cooldown,
		NotifyChannels:   model.JSONStringSlice(normalizeChannelList(in.NotifyChannels)),
		AIEnabled:        aiEnabled,
		Enabled:          enabled,
		Priority:         priority,
	}, nil
}

// normalizeChannelList 去重、去空白并保持稳定顺序（通知渠道顺序会影响 IM 群里的展示顺序）。
func normalizeChannelList(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, item := range in {
		channel := strings.ToLower(strings.TrimSpace(item))
		if channel == "" || seen[channel] {
			continue
		}
		seen[channel] = true
		out = append(out, channel)
	}
	sort.Strings(out)
	return out
}

// recordRule 写审计（规则改动会影响"哪些告警会打扰人"，必须留痕）。
func (s *LogAlertService) recordRule(ctx context.Context, operator Operator, action string, rule *model.LogAlertRule) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: operator.UserID, Username: operator.Username, ActionType: action, Level: LevelLow,
		IPAddress: operator.IP, UserAgent: operator.Agent,
		Detail: map[string]any{
			"rule_id": rule.ID, "name": rule.Name, "service": rule.ServiceName,
			"pattern": rule.SignaturePattern, "dedup_window": rule.DedupWindow,
			"cooldown": rule.Cooldown, "ai_enabled": rule.AIEnabled, "enabled": rule.Enabled,
		},
	})
}

// enabledRules 取启用规则（带 TTL 缓存，见 LogAlertService 的字段注释）。
func (s *LogAlertService) enabledRules(ctx context.Context) ([]model.LogAlertRule, error) {
	if s.rules == nil {
		return nil, nil
	}
	s.ruleCacheMu.RLock()
	cached, cachedAt := s.ruleCache, s.ruleCacheAt
	s.ruleCacheMu.RUnlock()
	if cached != nil && time.Since(cachedAt) < ruleCacheTTL {
		return cached, nil
	}
	rules, err := s.rules.ListEnabled(ctx)
	if err != nil {
		// 读失败时退回上一次的缓存（比"当成没有规则"安全：后者会让所有日志都走默认参数）。
		if cached != nil {
			return cached, err
		}
		return nil, err
	}
	s.ruleCacheMu.Lock()
	s.ruleCache, s.ruleCacheAt = rules, time.Now()
	s.ruleCacheMu.Unlock()
	return rules, nil
}

// invalidateRuleCache 让缓存立即失效（规则增删改后调用）。
//
// 为什么必须显式失效而不是等 TTL：使用者改完规则马上就会去验证效果，
// "等 10 秒才生效"会被当成"规则不生效"。
func (s *LogAlertService) invalidateRuleCache() {
	s.ruleCacheMu.Lock()
	s.ruleCache, s.ruleCacheAt = nil, time.Time{}
	s.ruleCacheMu.Unlock()
}

// matchRule 取出这次日志命中的规则；没有命中就是"不告警"。
//
// 刻意**不回落任何默认规则**：告警只能由平台上新增的规则触发（产品要求），
// 否则一个从没配过的服务也会持续产出通知，而没人说得清它依据什么在告警。
// 规则读取失败时同样按未命中处理并记 warn——读不到规则时的"静默告警"
// 和"凭空告警"一样都是不可接受的静默失效，至少要在日志里留下痕迹。
func (s *LogAlertService) matchRule(ctx context.Context, service, signature, level string) (model.LogAlertRule, bool) {
	rules, err := s.enabledRules(ctx)
	if err != nil {
		s.log.Warn("读取日志告警规则失败，本次按未命中处理", zap.Error(err))
	}
	return MatchLogAlertRule(rules, service, signature, level)
}
