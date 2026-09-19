package service

import (
	"context"
	"regexp"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
)

// 本文件实现「日志告警屏蔽项」：把"这类错误我不想收到"从"改代码/改配置"搬到页面上。
//
// 与日志告警规则的职责分工：
//   - 规则回答"命中之后怎么处理"（去重窗口 / 冷却期 / 通知渠道 / 是否 AI 分析）；
//   - 屏蔽项回答"这类错误根本不该成为告警"——命中即丢弃，连事件都不落库。
//
// 典型用法是屏蔽框架噪音，例如 Spring 打印的 "Request method 'GET' is not supported"：
// 它不是故障，但会按正常规则触发通知与 AI 分析，把真正的问题淹没在噪音里。
// 屏蔽项可配多条，按 id 顺序逐条判定，第一条命中即生效。

// exclusionMaxPattern 是屏蔽文本的长度上限（与表定义 size:512 对齐）。
const exclusionMaxPattern = 512

// LogAlertExclusionInput 是屏蔽项的写入入参。
type LogAlertExclusionInput struct {
	// Name 为备注（可空）。
	Name string `json:"name"`
	// ServiceName 为空表示对任意服务生效。
	ServiceName string `json:"service_name"`
	// Pattern 为匹配文本（必填）：普通文本按子串匹配，`/re/` 形式按正则匹配。
	Pattern string `json:"pattern"`
	// Enabled 用指针：未传时新建默认为启用、更新时保持原值。
	Enabled *bool `json:"enabled"`
}

// MatchLogAlertExclusion 取第一条命中的启用屏蔽项。
//
// 按 id 升序（先加的先判定）：屏蔽项通常只有几十条，顺序足够稳定且便于在页面上对照。
// 传入的 items 可以包含停用项与乱序项——它是纯函数，单测里随便给。
func MatchLogAlertExclusion(items []model.LogAlertExclusion, service, message string) (model.LogAlertExclusion, bool) {
	for _, item := range items {
		if !item.Enabled {
			continue
		}
		if svc := strings.TrimSpace(item.ServiceName); svc != "" &&
			!strings.EqualFold(svc, strings.TrimSpace(service)) {
			continue
		}
		// 与规则共用消息匹配语法（子串 / `/re/` 正则 / 非法正则退化为子串），
		// 使用者只需要记住一套写法：在规则里怎么配，在屏蔽里就怎么配。
		if !messageMatches(item.Pattern, message) {
			continue
		}
		return item, true
	}
	return model.LogAlertExclusion{}, false
}

// ListExclusions 分页检索屏蔽项。
func (s *LogAlertService) ListExclusions(ctx context.Context, keyword string, limit, offset int) ([]model.LogAlertExclusion, int64, error) {
	if s.exclusions == nil {
		return nil, 0, apperr.New(apperr.CodeInternal, "日志告警屏蔽项仓储未装配")
	}
	items, total, err := s.exclusions.List(ctx, keyword, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// CreateExclusion 新增屏蔽项。
func (s *LogAlertService) CreateExclusion(ctx context.Context, in LogAlertExclusionInput, operator Operator) (*model.LogAlertExclusion, error) {
	item, err := s.buildExclusion(model.LogAlertExclusion{Enabled: true}, in)
	if err != nil {
		return nil, err
	}
	if err := s.exclusions.Create(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.invalidateExclusionCache()
	s.recordExclusion(ctx, operator, "log_alert_exclusion_create", item)
	return item, nil
}

// UpdateExclusion 更新屏蔽项。
func (s *LogAlertService) UpdateExclusion(ctx context.Context, id int64, in LogAlertExclusionInput, operator Operator) (*model.LogAlertExclusion, error) {
	existing, err := s.exclusions.Get(ctx, id)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, err)
	}
	item, err := s.buildExclusion(*existing, in)
	if err != nil {
		return nil, err
	}
	item.ID = id
	if err := s.exclusions.Update(ctx, item); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.invalidateExclusionCache()
	s.recordExclusion(ctx, operator, "log_alert_exclusion_update", item)
	return item, nil
}

// DeleteExclusion 删除屏蔽项（删除后同类错误重新开始告警）。
func (s *LogAlertService) DeleteExclusion(ctx context.Context, id int64, operator Operator) error {
	if err := s.exclusions.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeNotFound, err)
	}
	s.invalidateExclusionCache()
	s.recordExclusion(ctx, operator, "log_alert_exclusion_delete", &model.LogAlertExclusion{Base: model.Base{ID: id}})
	return nil
}

// buildExclusion 校验入参并组装屏蔽项（base 为原值，更新时用于保留未传字段）。
func (s *LogAlertService) buildExclusion(base model.LogAlertExclusion, in LogAlertExclusionInput) (*model.LogAlertExclusion, error) {
	pattern := strings.TrimSpace(in.Pattern)
	if pattern == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "屏蔽内容不能为空（填错误消息里的文本或 /正则/）")
	}
	if len(pattern) > exclusionMaxPattern {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "屏蔽内容不能超过 %d 个字符", exclusionMaxPattern)
	}
	// 正则形式的语法错误必须在保存时挡住：等到运行期才发现，
	// 表现是"配了屏蔽却还在告警"，而且没人会想到去查一条已保存的配置。
	if strings.HasPrefix(pattern, "/") && strings.HasSuffix(pattern, "/") && len(pattern) > 2 {
		if _, err := regexp.Compile(pattern[1 : len(pattern)-1]); err != nil {
			return nil, apperr.Newf(apperr.CodeInvalidParam, "屏蔽正则非法：%v（也可直接填普通文本做子串匹配）", err)
		}
	}
	enabled := base.Enabled
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	return &model.LogAlertExclusion{
		Name:        strings.TrimSpace(in.Name),
		ServiceName: strings.TrimSpace(in.ServiceName),
		Pattern:     pattern,
		Enabled:     enabled,
	}, nil
}

// enabledExclusions 取启用屏蔽项（与规则同一套 TTL 缓存，理由见 enabledRules）。
func (s *LogAlertService) enabledExclusions(ctx context.Context) ([]model.LogAlertExclusion, error) {
	if s.exclusions == nil {
		return nil, nil
	}
	s.ruleCacheMu.RLock()
	cached, cachedAt := s.exclCache, s.exclCacheAt
	s.ruleCacheMu.RUnlock()
	if cached != nil && time.Since(cachedAt) < ruleCacheTTL {
		return cached, nil
	}
	items, err := s.exclusions.ListEnabled(ctx)
	if err != nil {
		// 读失败时沿用上一次的缓存：屏蔽项失效的后果是"噪音恢复"，
		// 而误判成"没有屏蔽项"同样只是恢复告警——两者都比查询报错更安全，取缓存即可。
		if cached != nil {
			return cached, err
		}
		return nil, err
	}
	s.ruleCacheMu.Lock()
	s.exclCache, s.exclCacheAt = items, time.Now()
	s.ruleCacheMu.Unlock()
	return items, nil
}

// exclusionFor 判断这条日志是否该被屏蔽。
func (s *LogAlertService) exclusionFor(ctx context.Context, service, message string) (model.LogAlertExclusion, bool) {
	items, err := s.enabledExclusions(ctx)
	if err != nil {
		s.log.Warn("读取日志告警屏蔽项失败，本次按不屏蔽处理", zap.Error(err))
	}
	return MatchLogAlertExclusion(items, service, message)
}

// invalidateExclusionCache 让屏蔽缓存立即失效（增删改后调用：改完马上就要验证效果）。
func (s *LogAlertService) invalidateExclusionCache() {
	s.ruleCacheMu.Lock()
	s.exclCache, s.exclCacheAt = nil, time.Time{}
	s.ruleCacheMu.Unlock()
}

// recordExclusion 写审计（屏蔽项决定"哪些错误永远不会打扰人"，必须留痕）。
func (s *LogAlertService) recordExclusion(ctx context.Context, operator Operator, action string, item *model.LogAlertExclusion) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: operator.UserID, Username: operator.Username, ActionType: action, Level: LevelLow,
		IPAddress: operator.IP, UserAgent: operator.Agent,
		Detail: map[string]any{
			"exclusion_id": item.ID, "name": item.Name,
			"service": item.ServiceName, "pattern": item.Pattern, "enabled": item.Enabled,
		},
	})
}
