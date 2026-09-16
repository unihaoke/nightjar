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
	notifier *NotifierService,
	audit *AuditService,
	log *zap.Logger,
) *AlertService {
	return &AlertService{
		rules: rules, alerts: alerts, embeddings: embeddings, instances: instances,
		store: store, monitor: mon, engine: eng, notifier: notifier, audit: audit, log: log,
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
	windowStart := now.Add(-time.Duration(rule.Window) * time.Minute)
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
	return false, false, nil
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
	rule := model.AlertRule{
		Name:       defaultString(in.RuleName, "外部推送"),
		InstanceID: instanceID,
		MWType:     in.MWType,
		MetricName: defaultString(in.MetricName, "external"),
		Operator:   ">",
		Level:      level,
		Window:     5,
		Cooldown:   10,
		Enabled:    true,
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
	Name           string   `json:"name" binding:"required"`
	InstanceID     int64    `json:"instance_id" binding:"required"`
	MWType         string   `json:"mw_type"`
	MetricName     string   `json:"metric_name" binding:"required"`
	Operator       string   `json:"operator" binding:"required"`
	Threshold      float64  `json:"threshold"`
	Level          string   `json:"level"`
	Window         int      `json:"window"`
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
		Level:  defaultString(in.Level, model.AlertLevelWarning),
		Window: defaultInt(in.Window, 5), Cooldown: defaultInt(in.Cooldown, 10),
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
	rule.Window = defaultInt(in.Window, rule.Window)
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
	msg := fmt.Sprintf("[%s] %s %s %s 阈值 %g（当前 %.4f%s）",
		rule.Level, name, rule.Operator, formatThreshold(rule.Threshold), rule.Threshold, value, metric.Unit)
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
