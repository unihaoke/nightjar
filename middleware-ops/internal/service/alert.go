package service

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/engine"
	"middleware-ops/internal/model"
	"middleware-ops/internal/monitor"
	"middleware-ops/internal/pkg/cache"
	"middleware-ops/internal/repository"
	"middleware-ops/internal/utils"
)

// AlertService 提供告警治理能力（4.4）。
//
// 收敛分级：
//   - 规则级（实时，主链路）：错误指纹 + 时间窗口去重 + 冷却期；
//   - 语义聚类（离线，辅助）：向量相似度聚类，仅合并展示、不修改状态；
//   - 因果收敛（二期）：依赖服务拓扑，一期不实现。
type AlertService struct {
	rules      *repository.AlertRuleRepository
	alerts     *repository.AlertRepository
	embeddings *repository.AlertEmbeddingRepository
	instances  *repository.InstanceRepository
	store      cache.Store
	monitor    monitor.Client
	engine     engine.Engine
	// diagnoser 是告警触发的自动 AI 诊断入口（3.1 事件驱动）。
	// 为 nil 表示未装配（如最小化部署/单测），此时自动降级为「只发通知、不诊断」。
	diagnoser  *DiagnoseService
	notifier   *NotifierService
	audit      *AuditService
	log        *zap.Logger
}

// NewAlertService 构造告警服务。
func NewAlertService(
	rules *repository.AlertRuleRepository,
	alerts *repository.AlertRepository,
	embeddings *repository.AlertEmbeddingRepository,
	instances *repository.InstanceRepository,
	store cache.Store,
	mon monitor.Client,
	eng engine.Engine,
	diag *DiagnoseService,
	notifier *NotifierService,
	audit *AuditService,
	log *zap.Logger,
) *AlertService {
	return &AlertService{
		rules: rules, alerts: alerts, embeddings: embeddings, instances: instances,
		store: store, monitor: mon, engine: eng, diagnoser: diag,
		notifier: notifier, audit: audit, log: log,
	}
}

// EvaluationResult 是一次规则评估的统计结果。
type EvaluationResult struct {
	// Evaluated 为参与评估的规则数。
	Evaluated int `json:"evaluated"`
	// Triggered 为触发（或合并）的告警数。
	Triggered int `json:"triggered"`
	// Merged 为被窗口去重合并的次数。
	Merged int `json:"merged"`
	// Suppressed 为处于冷却期被静默的次数。
	Suppressed int `json:"suppressed"`
	// Skipped 为因数据缺失跳过的规则数。
	Skipped int `json:"skipped"`
	// Duration 为评估耗时。
	Duration time.Duration `json:"duration"`
}

// Evaluate 执行一轮阈值评估（定时任务调用）。
func (s *AlertService) Evaluate(ctx context.Context) (*EvaluationResult, error) {
	start := time.Now()
	rules, err := s.rules.AllEnabled(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	result := &EvaluationResult{}
	cacheByInstance := make(map[int64]*monitor.Snapshot)

	for i := range rules {
		rule := rules[i]
		if rule.InstanceID == 0 {
			result.Skipped++
			continue
		}
		snapshot, ok := cacheByInstance[rule.InstanceID]
		if !ok {
			item, instErr := s.instances.Get(ctx, rule.InstanceID)
			if instErr != nil {
				result.Skipped++
				continue
			}
			snap, snapErr := s.monitor.Snapshot(ctx, ToTarget(*item))
			if snapErr != nil {
				s.log.Warn("采集实例指标失败，跳过该规则",
					zap.Int64("rule_id", rule.ID), zap.Int64("instance_id", rule.InstanceID), zap.Error(snapErr))
				result.Skipped++
				continue
			}
			snapshot = snap
			cacheByInstance[rule.InstanceID] = snapshot
		}
		result.Evaluated++

		metric, exists := snapshot.Get(rule.MetricName)
		if !exists {
			result.Skipped++
			continue
		}
		if !s.Compare(metric.Latest, rule.Operator, rule.Threshold) {
			continue
		}
		merged, suppressed, triggerErr := s.Trigger(ctx, rule, metric, 0)
		if triggerErr != nil {
			s.log.Error("触发告警失败", zap.Int64("rule_id", rule.ID), zap.Error(triggerErr))
			continue
		}
		switch {
		case suppressed:
			result.Suppressed++
		case merged:
			result.Merged++
			result.Triggered++
		default:
			result.Triggered++
		}
	}
	result.Duration = time.Since(start)
	return result, nil
}

// Trigger 触发或合并一条告警。
//
// 返回值：
//   - merged=true 表示与窗口内既有告警合并（计数 +1）；
//   - suppressed=true 表示处于冷却期，仅计数不外发通知。
//
// aiEnabled 决定是否触发 AI 诊断（事件驱动，见 3.1）：由调用方注入回调，
// 此处只负责生成/合并告警并派发通知。
func (s *AlertService) Trigger(ctx context.Context, rule model.AlertRule, metric monitor.Metric, value float64) (merged bool, suppressed bool, err error) {
	if value == 0 && metric.Latest != 0 {
		value = metric.Latest
	}
	if value == 0 {
		value = metric.Latest
	}
	now := time.Now().UTC()
	fingerprint := utils.Fingerprint(rule.MetricName, fmt.Sprintf("%d", rule.InstanceID), rule.Level)

	// 规则级收敛：窗口内同指纹合并。
	windowStart := now.Add(-time.Duration(rule.TimeWindow) * time.Minute)
	existing, findErr := s.alerts.FindActiveByFingerprint(ctx, fingerprint, windowStart)
	if findErr == nil && existing != nil {
		if err := s.alerts.BumpCount(ctx, existing.ID, now); err != nil {
			return false, false, apperr.Wrap(apperr.CodeInternal, err)
		}
		// 冷却期判断：窗口内已合并过则不再重复通知。
		cooling, coolErr := s.inCooldown(ctx, fingerprint, rule.Cooldown, now)
		if coolErr != nil {
			s.log.Warn("冷却期判断失败", zap.Error(coolErr))
		}
		if cooling {
			return true, true, nil
		}
		mergedAlert := *existing
		mergedAlert.Count++
		if s.notifier != nil {
			s.notifier.NotifyAlert(ctx, &mergedAlert, rule, metric)
		}
		return true, false, nil
	}
	if findErr != nil && !repository.EnsureNotFound(findErr) {
		return false, false, apperr.Wrap(apperr.CodeInternal, findErr)
	}

	alert := &model.Alert{
		InstanceID:   rule.InstanceID,
		RuleID:       rule.ID,
		MWType:       rule.MWType,
		AlertLevel:   rule.Level,
		AlertMessage: buildAlertMessage(rule, metric, value),
		MetricValue:  value,
		Fingerprint:  fingerprint,
		Status:       model.AlertStatusActive,
		Count:        1,
		TriggeredAt:  now,
	}
	if err := s.alerts.Create(ctx, alert); err != nil {
		return false, false, apperr.Wrap(apperr.CodeInternal, err)
	}
	// 记录冷却与指纹状态。
	if err := s.markSent(ctx, fingerprint, now, rule.Cooldown); err != nil {
		s.log.Warn("写入告警指纹失败", zap.Error(err))
	}
	// 告警向量（离线语义聚类用，异步生成不阻塞主链路）。
	s.enqueueEmbedding(ctx, alert)
	// 通知（多渠道）。
	if s.notifier != nil {
		s.notifier.NotifyAlert(ctx, alert, rule, metric)
	}
	// 事件驱动自动 AI 诊断（3.1）：只有规则勾选 ai_enabled 才跑，避免每次抖动都烧 token。
	if rule.AIEnabled {
		s.runAutoDiagnose(alert, buildAutoDiagnoseQuestion(alert), rule.NotifyChannels)
	}
	return false, false, nil
}

// runAutoDiagnose 异步执行告警触发的自动诊断。
//
// 必须与主链路解耦，原因有二：
//  1. 不能用入参 ctx——Evaluate 的调度 ctx 与 Ingest 的请求 ctx 返回后会被 cancel，
//     诊断（含 LLM 调用）会在请求边界被中断，出现"告警有、诊断没有"的半截状态；
//  2. 诊断耗时秒级到分钟级，阻塞会让 Evaluate 超时、拖慢其余规则评估。
//
// 失败只记录日志：自动诊断是增强能力，不应回压告警主链路。
func (s *AlertService) runAutoDiagnose(alert *model.Alert, question string, channels []string) {
	if s.diagnoser == nil || alert == nil {
		return
	}
	// 拷贝后再交给协程：alert 是本方法的局部变量快照，防止调用方复用指针产生数据竞争。
	snapshot := *alert
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		resp, err := s.diagnoser.AutoDiagnose(bg, &snapshot, question)
		if err != nil {
			s.log.Warn("告警自动 AI 诊断失败",
				zap.Int64("alert_id", snapshot.ID), zap.Int64("instance_id", snapshot.InstanceID), zap.Error(err))
			return
		}
		s.log.Info("告警自动 AI 诊断完成",
			zap.Int64("alert_id", snapshot.ID), zap.Int64("diagnosis_id", resp.DiagnosisID))
		// 是否外发只看「规则有没有勾选渠道」：没勾选就不外发（结论已入库，可在平台查看）。
		// ai_enabled 只控制要不要跑诊断，不构成发送凭据——两个开关互不兜底。
		if len(channels) == 0 {
			s.log.Debug("规则未勾选通知渠道，诊断结论仅入库不外发", zap.Int64("alert_id", snapshot.ID))
			return
		}
		// 结论续发到同一批渠道：第一条告警消息此时已经发出，无法塞进同一张卡片。
		if s.notifier != nil {
			s.notifier.NotifyAlertDiagnosis(bg, &snapshot, channels, toDiagnosisBrief(resp))
		}
	}()
}

// toDiagnosisBrief 把诊断结果压缩成通知可携带的摘要。
//
// 只取结论类字段（根因/建议/影响面/置信度），不搬运原始证据与工具调用明细——
// IM 里放这些既看不完，也容易让人误以为可以直接据其执行。
func toDiagnosisBrief(resp *Response) *DiagnosisBrief {
	if resp == nil || resp.Report == nil {
		return nil
	}
	r := resp.Report
	suggestions := make([]string, 0, len(r.Suggestions))
	for _, item := range r.Suggestions {
		if item.Action != "" {
			suggestions = append(suggestions, item.Action)
		}
	}
	return &DiagnosisBrief{
		DiagnosisID: resp.DiagnosisID,
		RootCause:   r.RootCause,
		Confidence:  r.Confidence,
		Suggestions: suggestions,
		ImpactScope: r.ImpactScope,
		EngineUsed:  resp.EngineUsed,
		DurationMS:  resp.DurationMS,
		// AIAvailable=false 表示护栏已判定 AI 不可用、结论来自规则引擎降级，
		// 必须在通知里显式标注，否则降级结论会被当成 AI 结论信任。
		Degraded:    !r.AIAvailable,
		Speculative: r.Speculative,
	}
}

// buildAutoDiagnoseQuestion 由告警内容构造自动诊断问题。
func buildAutoDiagnoseQuestion(alert *model.Alert) string {
	return fmt.Sprintf("实例触发告警：%s，请分析可能的根因并给出处置建议", alert.AlertMessage)
}

// Ingest 接收外部系统推送的告警（Prometheus Alertmanager Webhook / 自定义 Hook）。
func (s *AlertService) Ingest(ctx context.Context, in IngestAlertInput) (*model.Alert, error) {
	if in.InstanceID == 0 && in.InstanceName == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "必须提供 instance_id 或 instance_name")
	}
	instanceID := in.InstanceID
	if instanceID == 0 {
		item, err := s.instances.FindByName(ctx, in.InstanceName)
		if err != nil {
			return nil, apperr.Newf(apperr.CodeNotFound, "实例 %s 不存在", in.InstanceName)
		}
		instanceID = item.ID
	}
	level := in.Level
	if level != model.AlertLevelWarning && level != model.AlertLevelCritical {
		level = model.AlertLevelWarning
	}
	metricName := defaultString(in.MetricName, "external")
	// 外部推送没有平台规则，ai_enabled / notify_channels 两个开关本来无处取值。
	// 这里按「实例 + 指标」回绑一条已有规则来继承意图（口径 3 + A）；查不到就取零值，
	// 即"不诊断、不外发"，**不做任何默认值兜底**——否则等于在没有规则的地方凭空发明意图。
	intent := s.inheritIntent(ctx, instanceID, metricName)
	// 阈值/操作符/窗口/冷却只在**一级精确命中**时继承：指标同名，这些数描述的是同一个量。
	// 二级降级命中的是别的指标，套它的阈值会造成误导，那种情形继续用推送原文。
	operator, threshold := ">", 0.0
	timeWindow, cooldown := 5, 10
	description := ""
	if intent.Exact {
		operator, threshold = defaultString(intent.Operator, ">"), intent.Threshold
		timeWindow, cooldown = defaultInt(intent.TimeWindow, 5), defaultInt(intent.Cooldown, 10)
		description = intent.Description
	}
	rule := model.AlertRule{
		Name:       defaultString(in.RuleName, "外部推送"),
		InstanceID: instanceID,
		MWType:     in.MWType,
		MetricName: metricName,
		Operator:   operator,
		Level:      level,
		TimeWindow: timeWindow,
		Cooldown:   cooldown,
		Threshold:  threshold,
		Enabled:    true,
		// RuleID 保持 0：这条告警是外部推送的，不能挂到被继承的规则名下，
		// 否则界面会把"外部推送"错误显示成"规则 X 触发"。
		AIEnabled:      intent.AIEnabled,
		NotifyChannels: intent.NotifyChannels,
		Description:    description,
	}
	metric := monitor.Metric{Name: rule.MetricName, DisplayName: rule.MetricName, Latest: in.Value}
	if in.Message != "" {
		metric.DisplayName = in.Message
	}
	if _, _, err := s.Trigger(ctx, rule, metric, in.Value); err != nil {
		return nil, err
	}
	return s.alerts.FindActiveByFingerprint(ctx,
		utils.Fingerprint(rule.MetricName, fmt.Sprintf("%d", instanceID), level),
		time.Now().UTC().Add(-time.Minute))
}

// externalIntent 是外部告警从已有规则继承来的意图配置。
//
// 为什么必须区分 Exact：一级命中时指标相同，规则里的阈值/操作符描述的就是同一个量，
// 可以完整继承；二级命中的是同一实例下的**其它**指标，那套阈值与本条告警无关，
// 照搬会把「Redis 内存 > 80%」套到 MySQL 告警上，属于实质性误导。
type externalIntent struct {
	// RuleID 为意图来源规则；0 表示两级都没命中，本条告警不诊断、不外发。
	RuleID int64
	// Exact 为 true 表示一级命中（实例 + 指标精确匹配）。
	Exact bool
	// 这两个开关两级命中都会继承：它们回答的是"要不要关心"，与指标是否同名无关。
	AIEnabled      bool
	NotifyChannels model.JSONStringSlice
	// 以下字段仅 Exact=true 时有意义。
	Operator    string
	Threshold   float64
	TimeWindow  int
	Cooldown    int
	Description string
}

// inheritIntent 为外部告警推导「是否跑 AI 诊断 / 往哪些渠道发 / 收敛参数」的意图。
//
// 背景：外部告警没有平台规则，而这两个开关是规则承载的用户意图。这里按「实例 + 指标」
// 回绑一条已有规则继承其开关值；回绑不到就返回零值（不诊断、不外发），由 Trigger 统一处理。
//
// 为什么不在这里给默认值：默认值会让"没配置任何规则"和"配置了但关闭"变得不可区分，
// 排查时无法回答"这条外部告警为什么会被诊断"，也会把刚废掉的隐式兜底重新请回来。
//
// 注意：外部告警必须带着平台认识的 metric_name 才能回绑成功；携带来的是 "external"
// 这类占位值时查不到，会静默降级为不诊断不外发（见 Ingest 的 metricName 取值）。
func (s *AlertService) inheritIntent(ctx context.Context, instanceID int64, metricName string) externalIntent {
	if s.rules == nil {
		return externalIntent{}
	}
	// 一级：「实例 + 指标」精确回绑。命中即返回，不再往下走降级。
	matched, err := s.rules.FindByInstanceMetric(ctx, instanceID, metricName)
	if err != nil && !repository.EnsureNotFound(err) {
		// 查询本身出错只记录，不能阻断告警入库：外部告警丢了比漏一次诊断严重得多。
		s.log.Warn("外部告警回绑规则失败",
			zap.Int64("instance_id", instanceID), zap.String("metric", metricName), zap.Error(err))
		return externalIntent{}
	}
	if matched != nil {
		intent := externalIntent{
			RuleID:         matched.ID,
			Exact:          true,
			AIEnabled:      matched.AIEnabled,
			NotifyChannels: append(model.JSONStringSlice(nil), matched.NotifyChannels...),
			Operator:       matched.Operator,
			Threshold:      matched.Threshold,
			TimeWindow:     matched.TimeWindow,
			Cooldown:       matched.Cooldown,
			Description:    matched.Description,
		}
		s.log.Info("外部告警按实例+指标回绑规则，继承其意图与阈值配置",
			zap.Int64("instance_id", instanceID), zap.String("metric", metricName),
			zap.Int64("rule_id", intent.RuleID), zap.Bool("ai_enabled", intent.AIEnabled),
			zap.String("operator", intent.Operator), zap.Float64("threshold", intent.Threshold),
			zap.Int("channels", len(intent.NotifyChannels)))
		return intent
	}

	// 二级：降级到实例级。推送方常拿不到平台的指标名（占位值 "external"），
	// 精确匹配必然落空；此时只用实例上最近更新的规则兜住"是否关注该实例"的意图，
	// **不继承阈值类配置**（指标不同，阈值无意义）。
	fallback, fbErr := s.rules.FindLatestByInstance(ctx, instanceID)
	if fbErr != nil {
		if !repository.EnsureNotFound(fbErr) {
			s.log.Warn("外部告警实例级回绑失败",
				zap.Int64("instance_id", instanceID), zap.String("metric", metricName), zap.Error(fbErr))
		} else {
			// 两级都没命中 = 平台上不存在该实例的任何意图 → 静默（只入库，不诊断不外发）。
			s.log.Debug("外部告警未匹配到任何规则，跳过 AI 诊断与通知",
				zap.Int64("instance_id", instanceID), zap.String("metric", metricName))
		}
		return externalIntent{}
	}
	if fallback == nil {
		return externalIntent{}
	}
	channels := append(model.JSONStringSlice(nil), fallback.NotifyChannels...)
	s.log.Info("外部告警未命中同名指标，降级继承该实例最近更新规则的意图（不继承阈值）",
		zap.Int64("instance_id", instanceID),
		zap.String("pushed_metric", metricName), zap.String("rule_metric", fallback.MetricName),
		zap.Int64("rule_id", fallback.ID), zap.Bool("ai_enabled", fallback.AIEnabled),
		zap.Int("channels", len(channels)))
	return externalIntent{
		RuleID:         fallback.ID,
		Exact:          false,
		AIEnabled:      fallback.AIEnabled,
		NotifyChannels: channels,
	}
}

// IngestAlertInput 是外部告警入参。
type IngestAlertInput struct {
	InstanceID   int64   `json:"instance_id"`
	InstanceName string  `json:"instance_name"`
	MWType       string  `json:"mw_type"`
	RuleName     string  `json:"rule_name"`
	MetricName   string  `json:"metric_name"`
	Level        string  `json:"level"`
	Value        float64 `json:"value"`
	Message      string  `json:"message"`
}

// Ack 确认告警（L1 操作，留痕）。
func (s *AlertService) Ack(ctx context.Context, id int64, operator Operator) error {
	alert, err := s.alerts.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "告警不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if alert.Status != model.AlertStatusActive {
		return apperr.Newf(apperr.CodeInvalidParam, "告警当前状态为 %s，无需确认", alert.Status)
	}
	if err := s.alerts.Ack(ctx, id, operator.UserID); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, InstanceID: alert.InstanceID,
			ActionType: "alert_ack", Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"alert_id": id, "level": alert.AlertLevel},
		})
	}
	return nil
}

// Resolve 标记告警已恢复。
func (s *AlertService) Resolve(ctx context.Context, id int64, operator Operator) error {
	if _, err := s.alerts.Get(ctx, id); err != nil {
		if repository.EnsureNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "告警不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	now := time.Now().UTC()
	if err := s.alerts.SetStatus(ctx, id, model.AlertStatusResolved, &now); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username,
			ActionType: "alert_resolve", Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"alert_id": id},
		})
	}
	return nil
}

// List 分页检索告警历史。
func (s *AlertService) List(ctx context.Context, f repository.AlertFilter, limit, offset int) ([]model.Alert, int64, error) {
	items, total, err := s.alerts.List(ctx, f, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// ListRules 分页检索告警规则。
func (s *AlertService) ListRules(ctx context.Context, instanceID int64, limit, offset int) ([]model.AlertRule, int64, error) {
	items, total, err := s.rules.List(ctx, instanceID, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// RuleInput 是告警规则入参。
type RuleInput struct {
	Name       string  `json:"name" binding:"required"`
	InstanceID int64   `json:"instance_id" binding:"required"`
	MWType     string  `json:"mw_type"`
	MetricName string  `json:"metric_name" binding:"required"`
	Operator   string  `json:"operator" binding:"required"`
	Threshold  float64 `json:"threshold"`
	Level      string  `json:"level"`
	// TimeWindow 为去重窗口（分钟）；对外 JSON 字段与数据库列名统一为 time_window，
	// 避免沿用 window 这一 PostgreSQL 保留关键字（见 model.AlertRule）。
	TimeWindow     int      `json:"time_window"`
	Cooldown       int      `json:"cooldown"`
	NotifyChannels []string `json:"notify_channels"`
	Enabled        *bool    `json:"enabled"`
	AIEnabled      *bool    `json:"ai_enabled"`
	Description    string   `json:"description"`
}

// CreateRule 创建告警规则（L1）。
func (s *AlertService) CreateRule(ctx context.Context, in RuleInput, operator Operator) (*model.AlertRule, error) {
	if err := validateRule(in); err != nil {
		return nil, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	aiEnabled := true
	if in.AIEnabled != nil {
		aiEnabled = *in.AIEnabled
	}
	rule := &model.AlertRule{
		Name: in.Name, InstanceID: in.InstanceID, MWType: in.MWType,
		MetricName: in.MetricName, Operator: in.Operator, Threshold: in.Threshold,
		Level:      defaultString(in.Level, model.AlertLevelWarning),
		TimeWindow: defaultInt(in.TimeWindow, 5), Cooldown: defaultInt(in.Cooldown, 10),
		NotifyChannels: model.JSONStringSlice(in.NotifyChannels),
		Enabled:        enabled, AIEnabled: aiEnabled, Description: in.Description,
	}
	if err := s.rules.Create(ctx, rule); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, InstanceID: rule.InstanceID,
			ActionType: "alert_rule_create", Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"rule_id": rule.ID, "metric": rule.MetricName, "threshold": rule.Threshold},
		})
	}
	return rule, nil
}

// UpdateRule 更新告警规则（L1）。
func (s *AlertService) UpdateRule(ctx context.Context, id int64, in RuleInput, operator Operator) (*model.AlertRule, error) {
	rule, err := s.rules.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "规则不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := validateRule(in); err != nil {
		return nil, err
	}
	rule.Name = in.Name
	rule.InstanceID = in.InstanceID
	rule.MWType = in.MWType
	rule.MetricName = in.MetricName
	rule.Operator = in.Operator
	rule.Threshold = in.Threshold
	if in.Level != "" {
		rule.Level = in.Level
	}
	rule.TimeWindow = defaultInt(in.TimeWindow, rule.TimeWindow)
	rule.Cooldown = defaultInt(in.Cooldown, rule.Cooldown)
	rule.NotifyChannels = model.JSONStringSlice(in.NotifyChannels)
	if in.Enabled != nil {
		rule.Enabled = *in.Enabled
	}
	if in.AIEnabled != nil {
		rule.AIEnabled = *in.AIEnabled
	}
	rule.Description = in.Description
	if err := s.rules.Update(ctx, rule); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, InstanceID: rule.InstanceID,
			ActionType: "alert_rule_update", Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"rule_id": rule.ID, "enabled": rule.Enabled},
		})
	}
	return rule, nil
}

// DeleteRule 删除告警规则（L1）。
func (s *AlertService) DeleteRule(ctx context.Context, id int64, operator Operator) error {
	rule, err := s.rules.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "规则不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := s.rules.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if s.audit != nil {
		s.audit.RecordAsync(ctx, AuditEntry{
			UserID: operator.UserID, Username: operator.Username, InstanceID: rule.InstanceID,
			ActionType: "alert_rule_delete", Level: LevelLow, IPAddress: operator.IP, UserAgent: operator.Agent,
			Detail: map[string]any{"rule_id": id, "name": rule.Name},
		})
	}
	return nil
}

// ClusterResult 是语义聚类结果。
type ClusterResult struct {
	Processed int            `json:"processed"`
	Clusters  []AlertCluster `json:"clusters"`
	// Native 表示是否使用了 pgvector 原生检索。
	Native bool `json:"native"`
}

// AlertCluster 是一组语义相近的告警。
type AlertCluster struct {
	ClusterID string  `json:"cluster_id"`
	Label     string  `json:"label"`
	Count     int     `json:"count"`
	AlertIDs  []int64 `json:"alert_ids"`
}

// Cluster 执行离线语义聚类（4.4 辅助能力，不修改告警状态）。
func (s *AlertService) Cluster(ctx context.Context, threshold float64) (*ClusterResult, error) {
	if threshold <= 0 {
		threshold = 0.78
	}
	pending, err := s.embeddings.ListUnclustered(ctx, 500)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	result := &ClusterResult{Native: false}
	type clusterState struct {
		id    string
		vec   model.Vector
		ids   []int64
		label string
	}
	var clusters []*clusterState

	for i := range pending {
		item := pending[i]
		vec := model.Vector(item.Embedding)
		if len(vec) == 0 {
			continue
		}
		matched := false
		for _, c := range clusters {
			if cosine(vec, c.vec) >= threshold {
				c.ids = append(c.ids, item.AlertID)
				matched = true
				break
			}
		}
		if !matched {
			label := item.AlertContent
			if len(label) > 60 {
				label = label[:60]
			}
			clusters = append(clusters, &clusterState{
				id:    fmt.Sprintf("c%d", len(clusters)+1),
				vec:   vec,
				ids:   []int64{item.AlertID},
				label: label,
			})
		}
		result.Processed++
	}

	marked := make([]int64, 0, len(pending))
	for _, item := range pending {
		marked = append(marked, item.ID)
	}
	if err := s.embeddings.MarkClustered(ctx, marked); err != nil {
		s.log.Warn("标记聚类状态失败", zap.Error(err))
	}
	for _, c := range clusters {
		if len(c.ids) < 2 {
			continue
		}
		clusterID := utils.Fingerprint(c.id, c.label)[:12]
		for _, alertID := range c.ids {
			if err := s.alerts.SetCluster(ctx, alertID, clusterID); err != nil {
				s.log.Warn("标注告警聚类失败", zap.Int64("alert_id", alertID), zap.Error(err))
			}
		}
		result.Clusters = append(result.Clusters, AlertCluster{
			ClusterID: clusterID, Label: c.label, Count: len(c.ids), AlertIDs: c.ids,
		})
	}
	return result, nil
}

// Compare 依据操作符比较指标值与阈值。
func (s *AlertService) Compare(value float64, operator string, threshold float64) bool {
	switch strings.TrimSpace(operator) {
	case ">":
		return value > threshold
	case ">=":
		return value >= threshold
	case "<":
		return value < threshold
	case "<=":
		return value <= threshold
	case "==", "=":
		return value == threshold
	case "!=":
		return value != threshold
	default:
		return false
	}
}

// inCooldown 判断指纹是否处于冷却期。
func (s *AlertService) inCooldown(ctx context.Context, fingerprint string, cooldownMinutes int, now time.Time) (bool, error) {
	if cooldownMinutes <= 0 || s.store == nil {
		return false, nil
	}
	raw, err := s.store.Get(ctx, "alert:cd:"+fingerprint)
	if err != nil {
		if err == cache.ErrNotFound {
			return false, nil
		}
		return false, err
	}
	var last time.Time
	if err := json.Unmarshal([]byte(raw), &last); err != nil {
		return false, nil
	}
	return now.Sub(last) < time.Duration(cooldownMinutes)*time.Minute, nil
}

// markSent 记录最近发送时间。
func (s *AlertService) markSent(ctx context.Context, fingerprint string, now time.Time, cooldownMinutes int) error {
	if s.store == nil {
		return nil
	}
	payload, err := json.Marshal(now)
	if err != nil {
		return err
	}
	ttl := time.Duration(cooldownMinutes) * time.Minute * 2
	if ttl <= 0 {
		ttl = 20 * time.Minute
	}
	return s.store.Set(ctx, "alert:cd:"+fingerprint, string(payload), ttl)
}

// enqueueEmbedding 异步生成告警向量。
func (s *AlertService) enqueueEmbedding(ctx context.Context, alert *model.Alert) {
	if s.engine == nil || s.embeddings == nil {
		return
	}
	content := fmt.Sprintf("%s %s %s", alert.MWType, alert.AlertLevel, alert.AlertMessage)
	go func() {
		bg, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		vecs, err := s.engine.Embed(bg, []string{content})
		if err != nil || len(vecs) == 0 {
			return
		}
		item := &model.AlertEmbedding{
			AlertID:      alert.ID,
			AlertContent: content,
			Embedding:    model.Vector(vecs[0]),
		}
		if err := s.embeddings.Create(bg, item); err != nil {
			s.log.Warn("写入告警向量失败", zap.Int64("alert_id", alert.ID), zap.Error(err))
		}
	}()
}

// buildAlertMessage 生成告警消息。
func buildAlertMessage(rule model.AlertRule, metric monitor.Metric, value float64) string {
	name := metric.DisplayName
	if name == "" {
		name = metric.Name
	}
	// 阈值只输出一次：这里原先同时用 %s（formatThreshold）和 %g（原始值）占位，
	// 渲染成「Redis 内存使用率 > 80 阈值 80」，数字重复一遍。
	msg := fmt.Sprintf("[%s] %s %s 阈值 %s（当前 %.4f%s）",
		rule.Level, name, rule.Operator, formatThreshold(rule.Threshold), value, metric.Unit)
	if rule.Description != "" {
		msg += " — " + rule.Description
	}
	if len([]rune(msg)) > 500 {
		msg = string([]rune(msg)[:500])
	}
	return msg
}

// formatThreshold 格式化阈值展示。
func formatThreshold(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%g", v)
}

// validateRule 校验规则入参。
func validateRule(in RuleInput) error {
	if strings.TrimSpace(in.Name) == "" {
		return apperr.New(apperr.CodeInvalidParam, "规则名称不能为空")
	}
	if in.InstanceID <= 0 {
		return apperr.New(apperr.CodeInvalidParam, "必须选择实例")
	}
	if strings.TrimSpace(in.MetricName) == "" {
		return apperr.New(apperr.CodeInvalidParam, "指标名不能为空")
	}
	switch in.Operator {
	case ">", ">=", "<", "<=", "==", "=", "!=":
	default:
		return apperr.Newf(apperr.CodeInvalidParam, "操作符 %q 非法", in.Operator)
	}
	if in.Level != "" && in.Level != model.AlertLevelWarning && in.Level != model.AlertLevelCritical {
		return apperr.Newf(apperr.CodeInvalidParam, "告警级别 %q 非法", in.Level)
	}
	return nil
}

// defaultInt 返回非零默认值。
func defaultInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// cosine 计算两个向量的余弦相似度。
func cosine(a, b model.Vector) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
