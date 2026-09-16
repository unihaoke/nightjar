package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/engine/guardrail"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// 修复动作类型（白名单）。
const (
	ActionViewMetrics    = "view_metrics"
	ActionRunReadonlySQL = "run_readonly_sql"
	ActionAckAlert       = "ack_alert"
	ActionCleanRedisKey  = "clean_redis_key"
	ActionRestartService = "restart_service"
	ActionUpdateConfig   = "update_config"
	ActionSQLWrite       = "sql_write"
)

// Executor 是修复动作的实际执行接口。
//
// 平台默认只提供「预演 + 只读」能力；真实执行需要接入被管中间件客户端后由
// 部署方实现本接口并注入（设计文档 4.6：执行结果回填复核）。
type Executor interface {
	// Name 返回执行器名称。
	Name() string
	// DryRun 仅校验参数并返回影响预览，不产生副作用。
	DryRun(ctx context.Context, instance model.MiddlewareInstance, actionType string, params map[string]any) (map[string]any, error)
	// Execute 执行动作。
	Execute(ctx context.Context, instance model.MiddlewareInstance, actionType string, params map[string]any) (map[string]any, error)
}

// dryRunExecutor 是默认执行器：只支持预演与只读动作。
//
// 该实现显式拒绝所有 L2 写操作，避免「未接入真实客户端却显示执行成功」的误导，
// 同时让审批链路与审计链路可以完整跑通。
type dryRunExecutor struct{}

// Name 返回执行器名称。
func (dryRunExecutor) Name() string { return "dry-run" }

// DryRun 返回影响预览。
func (e dryRunExecutor) DryRun(_ context.Context, instance model.MiddlewareInstance, actionType string, params map[string]any) (map[string]any, error) {
	return map[string]any{
		"executor":     e.Name(),
		"action":       actionType,
		"instance":     instance.Name,
		"environment":  instance.Environment,
		"params":       params,
		"reversible":   isReversible(actionType),
		"impact":       describeImpact(instance, actionType),
		"will_execute": false,
		"note":         "当前执行器为预演实现：仅返回影响预览，不对被管中间件产生副作用",
	}, nil
}

// Execute 拒绝执行非只读动作。
func (e dryRunExecutor) Execute(_ context.Context, instance model.MiddlewareInstance, actionType string, params map[string]any) (map[string]any, error) {
	level := ActionLevel(actionType)
	if level == LevelHigh {
		return nil, apperr.Newf(apperr.CodeForbidden,
			"执行器 %s 未接入真实的 %s 客户端，L2 动作 %s 不会被执行（审批通过后需人工执行或接入执行器）",
			e.Name(), instance.MWType, actionType)
	}
	return map[string]any{
		"executor": e.Name(), "action": actionType, "instance": instance.Name,
		"params": params, "status": "noop",
		"note": "只读动作无需真实执行，平台已通过监控与诊断链路提供结果",
	}, nil
}

// FixService 实现修复建议与执行模块（4.6）。
type FixService struct {
	instances *repository.InstanceRepository
	fixes     *repository.FixRepository
	approvals *ApprovalService
	audit     *AuditService
	sqlGuard  *guardrail.SQLGuard
	registry  *guardrail.ToolRegistry
	executor  Executor
	log       *zap.Logger
}

// NewFixService 构造修复服务。
func NewFixService(
	instances *repository.InstanceRepository,
	fixes *repository.FixRepository,
	approvals *ApprovalService,
	audit *AuditService,
	sqlGuard *guardrail.SQLGuard,
	registry *guardrail.ToolRegistry,
	executor Executor,
	log *zap.Logger,
) *FixService {
	if executor == nil {
		executor = dryRunExecutor{}
	}
	return &FixService{
		instances: instances, fixes: fixes, approvals: approvals, audit: audit,
		sqlGuard: sqlGuard, registry: registry, executor: executor, log: log,
	}
}

// PreviewResult 是修复预览结果（L0）。
type PreviewResult struct {
	InstanceID  int64  `json:"instance_id"`
	ActionType  string `json:"action_type"`
	Level       string `json:"level"`
	Environment string `json:"environment"`
	// RequiresApproval 为 true 表示该动作在当前环境下必须走审批。
	RequiresApproval bool           `json:"requires_approval"`
	HighRisk         bool           `json:"high_risk"`
	HighRiskReason   string         `json:"high_risk_reason"`
	Impact           map[string]any `json:"impact"`
	// NormalizedSQL 为 AI 生成 SQL 经规则校验后的可执行语句（5.5）。
	NormalizedSQL string   `json:"normalized_sql,omitempty"`
	SQLNotes      []string `json:"sql_notes,omitempty"`
	Warnings      []string `json:"warnings"`
}

// FixRequest 是修复预览/执行入参。
type FixRequest struct {
	InstanceID int64          `json:"instance_id" binding:"required"`
	ActionType string         `json:"action_type" binding:"required"`
	Params     map[string]any `json:"params"`
	// Reason 为申请理由（L2 审批必填）。
	Reason string `json:"reason"`
	// DryRun 为 true 时仅预演。
	DryRun bool `json:"dry_run"`
	// TicketID 为审批通过后执行时携带的工单号。
	TicketID string `json:"ticket_id"`
}

// Preview 生成修复预览并返回操作级别判定（L0）。
func (s *FixService) Preview(ctx context.Context, in FixRequest, operator Operator, scope Scope) (*PreviewResult, error) {
	instance, err := s.instance(ctx, in.InstanceID, scope)
	if err != nil {
		return nil, err
	}
	if !validAction(in.ActionType) {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "不支持的动作类型 %q", in.ActionType)
	}
	level := ActionLevel(in.ActionType)
	highRisk, reason := detectHighRisk(in)

	preview := &PreviewResult{
		InstanceID:     instance.ID,
		ActionType:     in.ActionType,
		Level:          level,
		Environment:    instance.Environment,
		HighRisk:       highRisk,
		HighRiskReason: reason,
		Warnings:       make([]string, 0, 2),
	}
	// prod 环境 L2 强制审批；dev/staging 也走审批以保证审计闭环（可由配置放宽）。
	if level == LevelHigh {
		preview.RequiresApproval = true
		if instance.Environment == model.EnvProd {
			preview.Warnings = append(preview.Warnings, "生产环境高危操作：必须经审批通过后由人工执行")
		}
	}

	// AI 生成 SQL 的规则校验（5.5）。
	if sqlText, ok := in.Params["sql"].(string); ok && strings.TrimSpace(sqlText) != "" {
		normalized, notes, sqlErr := s.sqlGuard.Validate(sqlText)
		if sqlErr != nil {
			preview.Warnings = append(preview.Warnings, "SQL 未通过只读校验："+sqlErr.Error())
			preview.NormalizedSQL = ""
		} else {
			preview.NormalizedSQL = normalized
			preview.SQLNotes = notes
			if in.ActionType == ActionRunReadonlySQL {
				level = LevelRead
				preview.Level = LevelRead
			}
		}
	}

	impact, err := s.executor.DryRun(ctx, *instance, in.ActionType, in.Params)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	impact["level"] = level
	impact["requires_approval"] = preview.RequiresApproval
	preview.Impact = impact

	s.auditRecord(ctx, operator, instance.ID, "fix_preview", LevelRead, map[string]any{
		"action_type": in.ActionType, "level": level, "high_risk": highRisk, "params": in.Params,
	})
	return preview, nil
}

// ExecuteResult 是执行结果。
type ExecuteResult struct {
	Status   string         `json:"status"`
	Level    string         `json:"level"`
	TicketID string         `json:"ticket_id,omitempty"`
	FixID    int64          `json:"fix_id"`
	Result   map[string]any `json:"result"`
	Message  string         `json:"message"`
}

// Execute 执行修复动作（L1 直接执行，L2 转审批）。
func (s *FixService) Execute(ctx context.Context, in FixRequest, session *Session, operator Operator, scope Scope) (*ExecuteResult, error) {
	instance, err := s.instance(ctx, in.InstanceID, scope)
	if err != nil {
		return nil, err
	}
	if !validAction(in.ActionType) {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "不支持的动作类型 %q", in.ActionType)
	}
	level := ActionLevel(in.ActionType)

	// L0/L1 需要角色具备对应操作级别；L2 需要审批工单。
	if level != LevelHigh {
		if err := authFromSession(session).RequireLevel(session, level); err != nil {
			return nil, err
		}
	}

	// L2：创建审批工单（prod 强制，30min 超时自动拒绝）。
	if level == LevelHigh {
		if in.TicketID == "" {
			ticket, ticketErr := s.approvals.Create(ctx, ApprovalRequest{
				InstanceID:   instance.ID,
				Environment:  instance.Environment,
				ActionType:   in.ActionType,
				ActionDetail: in.Params,
				Reason:       in.Reason,
			}, operator)
			if ticketErr != nil {
				return nil, ticketErr
			}
			s.auditRecord(ctx, operator, instance.ID, "fix_approval_submit", LevelHigh, map[string]any{
				"ticket_id": ticket.TicketID, "action_type": in.ActionType,
			})
			return &ExecuteResult{
				Status:   "pending_approval",
				Level:    level,
				TicketID: ticket.TicketID,
				Message:  fmt.Sprintf("该操作为 L2 高危操作，已创建审批工单 %s（%s 内未审批将自动拒绝）", ticket.TicketID, "30 分钟"),
			}, nil
		}
		ticket, ticketErr := s.approvals.GetByTicket(ctx, in.TicketID)
		if ticketErr != nil {
			return nil, ticketErr
		}
		if ticket.Status != "approved" {
			return nil, apperr.Newf(apperr.CodeApprovalRequired, "工单 %s 当前状态为 %s，未获批准不允许执行", ticket.TicketID, ticket.Status)
		}
	}

	start := time.Now()
	result := map[string]any{}
	status := "success"
	var execErr error
	if in.DryRun {
		result, execErr = s.executor.DryRun(ctx, *instance, in.ActionType, in.Params)
		result["dry_run"] = true
		status = "dry_run"
	} else {
		result, execErr = s.executor.Execute(ctx, *instance, in.ActionType, in.Params)
	}
	if execErr != nil {
		status = "failed"
		result = map[string]any{"error": execErr.Error(), "executor": s.executor.Name()}
	}

	record := &model.FixRecord{
		TicketID: in.TicketID, UserID: operator.UserID, InstanceID: instance.ID,
		ActionType: in.ActionType, Level: level,
		Command:      stringOf(in.Params["command"]),
		ActionDetail: model.JSONMap(in.Params),
		Status:       status,
		DryRun:       in.DryRun,
		DurationMS:   time.Since(start).Milliseconds(),
	}
	if payload, jsonErr := toJSONMap(result); jsonErr == nil {
		record.Result = mustMarshal(payload)
	}
	if s.fixes != nil {
		if err := s.fixes.Create(ctx, record); err != nil {
			s.log.Warn("写入修复记录失败", zap.Error(err))
		}
	}
	if in.TicketID != "" && s.approvals != nil {
		if err := s.approvals.MarkExecuted(ctx, in.TicketID, status, result); err != nil {
			s.log.Warn("回填工单执行结果失败", zap.Error(err))
		}
	}
	s.auditRecord(ctx, operator, instance.ID, "fix_execute", level, map[string]any{
		"action_type": in.ActionType, "status": status, "ticket_id": in.TicketID,
		"dry_run": in.DryRun, "params": in.Params,
	})

	message := "执行完成"
	if execErr != nil {
		message = "执行失败：" + execErr.Error()
	}
	return &ExecuteResult{
		Status: status, Level: level, TicketID: in.TicketID, FixID: record.ID,
		Result: result, Message: message,
	}, nil
}

// History 分页查询修复历史。
func (s *FixService) History(ctx context.Context, instanceID int64, limit, offset int) ([]model.FixRecord, int64, error) {
	items, total, err := s.fixes.List(ctx, instanceID, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// instance 查询实例并校验数据权限。
func (s *FixService) instance(ctx context.Context, id int64, scope Scope) (*model.MiddlewareInstance, error) {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "实例不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	if !inScope(item, scope) {
		return nil, apperr.New(apperr.CodeScopeDenied, "该实例不在你的数据权限范围内")
	}
	return item, nil
}

// auditRecord 写审计。
func (s *FixService) auditRecord(ctx context.Context, op Operator, instanceID int64, action, level string, detail map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: op.UserID, Username: op.Username, InstanceID: instanceID,
		ActionType: action, Level: level, IPAddress: op.IP, UserAgent: op.Agent, Detail: detail,
	})
}

// ActionLevel 返回动作对应的操作级别（4.6 操作分级）。
func ActionLevel(action string) string {
	switch action {
	case ActionViewMetrics, ActionRunReadonlySQL:
		return LevelRead
	case ActionAckAlert:
		return LevelLow
	case ActionCleanRedisKey, ActionRestartService, ActionUpdateConfig, ActionSQLWrite:
		return LevelHigh
	default:
		// 未知动作按最高风险处理，强制走审批。
		return LevelHigh
	}
}

// validAction 校验动作是否在白名单内。
func validAction(action string) bool {
	switch action {
	case ActionViewMetrics, ActionRunReadonlySQL, ActionAckAlert,
		ActionCleanRedisKey, ActionRestartService, ActionUpdateConfig, ActionSQLWrite:
		return true
	default:
		return false
	}
}

// isReversible 判断动作是否可回滚。
func isReversible(action string) bool {
	switch action {
	case ActionViewMetrics, ActionRunReadonlySQL, ActionAckAlert, ActionUpdateConfig:
		return true
	default:
		return false
	}
}

// describeImpact 生成影响范围描述。
func describeImpact(instance model.MiddlewareInstance, action string) string {
	switch action {
	case ActionCleanRedisKey:
		return fmt.Sprintf("将删除 %s 上的指定 key，可能造成缓存穿透并抬升下游数据库负载", instance.Name)
	case ActionRestartService:
		return fmt.Sprintf("将重启 %s，重启期间该实例不可用（预计 10-60s）", instance.Name)
	case ActionUpdateConfig:
		return fmt.Sprintf("将修改 %s 的配置，部分参数需要重启生效；错误配置可能导致启动失败", instance.Name)
	case ActionSQLWrite:
		return "将对目标库执行写操作，可能影响线上数据，需人工确认最小影响范围"
	case ActionRunReadonlySQL:
		return "只读查询，对线上无副作用（已强制 LIMIT 与只读账号）"
	default:
		return "只读操作，无副作用"
	}
}

// detectHighRisk 识别高危特征（6.6）。
func detectHighRisk(in FixRequest) (bool, string) {
	if sqlText, ok := in.Params["sql"].(string); ok {
		if risky, reason := guardrail.IsHighRisk(sqlText); risky {
			return true, reason
		}
	}
	if command, ok := in.Params["command"].(string); ok {
		if risky, reason := guardrail.IsHighRisk(command); risky {
			return true, reason
		}
	}
	if ActionLevel(in.ActionType) == LevelHigh {
		return true, "动作类型属于 L2 高危操作"
	}
	return false, ""
}

// stringOf 安全取字符串。
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// mustMarshal 序列化（失败返回空串）。
func mustMarshal(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(out)
}

// IsHighRiskSQL 判定 SQL 是否属于高危操作（供 handler 与审批链路复用）。
func IsHighRiskSQL(sql string) (bool, string) {
	return guardrail.IsHighRisk(sql)
}

// ActionCatalog 返回动作目录（供前端渲染操作面板）。
func ActionCatalog() []map[string]any {
	return []map[string]any{
		{"action": ActionViewMetrics, "label": "查看指标", "level": ActionLevel(ActionViewMetrics), "desc": "只读查看实例指标"},
		{"action": ActionRunReadonlySQL, "label": "只读 SQL 查询", "level": ActionLevel(ActionRunReadonlySQL), "desc": "强制只读 + 强制 LIMIT"},
		{"action": ActionAckAlert, "label": "确认告警", "level": ActionLevel(ActionAckAlert), "desc": "确认告警并留痕"},
		{"action": ActionCleanRedisKey, "label": "清理 Redis key", "level": ActionLevel(ActionCleanRedisKey), "desc": "高危：需审批"},
		{"action": ActionRestartService, "label": "重启服务", "level": ActionLevel(ActionRestartService), "desc": "高危：需审批"},
		{"action": ActionUpdateConfig, "label": "修改配置", "level": ActionLevel(ActionUpdateConfig), "desc": "高危：需审批"},
		{"action": ActionSQLWrite, "label": "SQL 写操作", "level": ActionLevel(ActionSQLWrite), "desc": "高危：需审批"},
	}
}
