package service

import (
	"context"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"middleware-ops/internal/apperr"
	"middleware-ops/internal/model"
	"middleware-ops/internal/repository"
)

// KnowledgeService 提供知识库能力（4.5）。
//
// 质量闭环：auto 来源（诊断沉淀）先入 draft，人工确认后转为 published 才参与检索；
// 低采纳率条目在检索时降权。
type KnowledgeService struct {
	repo      *repository.KnowledgeRepository
	diagnoses *repository.DiagnosisRepository
	audit     *AuditService
	log       *zap.Logger
}

// NewKnowledgeService 构造知识库服务。
func NewKnowledgeService(repo *repository.KnowledgeRepository, diagnoses *repository.DiagnosisRepository, audit *AuditService, log *zap.Logger) *KnowledgeService {
	return &KnowledgeService{repo: repo, diagnoses: diagnoses, audit: audit, log: log}
}

// KnowledgeInput 是知识条目入参。
type KnowledgeInput struct {
	Title   string   `json:"title" binding:"required,max=255"`
	Content string   `json:"content" binding:"required"`
	MWType  string   `json:"mw_type"`
	Tags    []string `json:"tags"`
	Status  string   `json:"status"`
}

// List 分页检索知识条目。
func (s *KnowledgeService) List(ctx context.Context, f repository.KnowledgeFilter, limit, offset int) ([]model.KnowledgeBase, int64, error) {
	items, total, err := s.repo.List(ctx, f, limit, offset)
	if err != nil {
		return nil, 0, apperr.Wrap(apperr.CodeInternal, err)
	}
	return items, total, nil
}

// Get 查询条目详情。
func (s *KnowledgeService) Get(ctx context.Context, id int64) (*model.KnowledgeBase, error) {
	item, err := s.repo.Get(ctx, id)
	if err != nil {
		if repository.EnsureNotFound(err) {
			return nil, apperr.New(apperr.CodeNotFound, "知识条目不存在")
		}
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	return item, nil
}

// Create 新增条目（手工录入默认 published，AI 沉淀走 DiagnoseService）。
func (s *KnowledgeService) Create(ctx context.Context, in KnowledgeInput, operator Operator) (*model.KnowledgeBase, error) {
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Content) == "" {
		return nil, apperr.New(apperr.CodeInvalidParam, "标题与内容不能为空")
	}
	status := in.Status
	if status == "" {
		status = model.KnowledgeStatusPublished
	}
	if !validKnowledgeStatus(status) {
		return nil, apperr.Newf(apperr.CodeInvalidParam, "状态 %q 非法（可选 draft/published/deprecated）", status)
	}
	entry := &model.KnowledgeBase{
		Title: strings.TrimSpace(in.Title), Content: in.Content, MWType: in.MWType,
		Tags: model.JSONStringSlice(in.Tags), Source: "manual", Status: status, AuthorID: operator.UserID,
	}
	if err := s.repo.Create(ctx, entry); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	s.record(ctx, operator, "knowledge_create", map[string]any{"id": entry.ID, "title": entry.Title, "status": status})
	return entry, nil
}

// Update 更新条目（含草稿转正：draft → published）。
func (s *KnowledgeService) Update(ctx context.Context, id int64, in KnowledgeInput, operator Operator) (*model.KnowledgeBase, error) {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) != "" {
		entry.Title = strings.TrimSpace(in.Title)
	}
	if in.Content != "" {
		entry.Content = in.Content
	}
	if in.MWType != "" {
		entry.MWType = in.MWType
	}
	if in.Tags != nil {
		entry.Tags = model.JSONStringSlice(in.Tags)
	}
	statusChanged := false
	if in.Status != "" {
		if !validKnowledgeStatus(in.Status) {
			return nil, apperr.Newf(apperr.CodeInvalidParam, "状态 %q 非法", in.Status)
		}
		statusChanged = entry.Status != in.Status
		entry.Status = in.Status
	}
	if err := s.repo.Update(ctx, entry); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	detail := map[string]any{"id": entry.ID, "status": entry.Status}
	if statusChanged {
		detail["status_changed"] = true
	}
	s.record(ctx, operator, "knowledge_update", detail)
	return entry, nil
}

// Adopt 采纳条目（质量闭环：采纳次数参与检索权重与采纳率统计）。
func (s *KnowledgeService) Adopt(ctx context.Context, id int64, operator Operator) error {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := s.repo.BumpAdopt(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if entry.DiagnosisID > 0 && s.diagnoses != nil {
		if err := s.diagnoses.UpdateFeedback(ctx, entry.DiagnosisID, model.FeedbackAdopted); err != nil {
			s.log.Warn("更新诊断采纳状态失败", zap.Int64("diagnosis_id", entry.DiagnosisID), zap.Error(err))
		}
	}
	s.record(ctx, operator, "knowledge_adopt", map[string]any{"id": id, "title": entry.Title})
	return nil
}

// Delete 删除条目（L1）。
func (s *KnowledgeService) Delete(ctx context.Context, id int64, operator Operator) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.repo.Delete(ctx, id); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	s.record(ctx, operator, "knowledge_delete", map[string]any{"id": id})
	return nil
}

// BumpUse 累加条目被引用次数（诊断过程引用后回填）。
func (s *KnowledgeService) BumpUse(ctx context.Context, ids []int64) error {
	if err := s.repo.BumpUse(ctx, ids); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	return nil
}

// Stats 返回知识库统计（大盘使用）。
func (s *KnowledgeService) Stats(ctx context.Context) (map[string]any, error) {
	counts, err := s.repo.CountByStatus(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	var total int64
	for _, v := range counts {
		total += v
	}
	// 采纳率：已发布条目中 adopt_count > 0 的占比。
	published, _, err := s.repo.List(ctx, repository.KnowledgeFilter{Status: model.KnowledgeStatusPublished}, 500, 0)
	adopted := 0
	for _, item := range published {
		if item.AdoptCount > 0 {
			adopted++
		}
	}
	rate := 0.0
	if len(published) > 0 {
		rate = float64(adopted) / float64(len(published))
	}
	return map[string]any{
		"total": total, "by_status": counts, "adoption_rate": roundFloat(rate, 3),
		"published_sampled": len(published),
	}, nil
}

// TopAdopted 返回采纳率最高的条目（质量基线观测）。
func (s *KnowledgeService) TopAdopted(ctx context.Context, limit int) ([]model.KnowledgeBase, error) {
	items, _, err := s.repo.List(ctx, repository.KnowledgeFilter{Status: model.KnowledgeStatusPublished}, limit, 0)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].AdoptCount > items[j].AdoptCount })
	return items, nil
}

// validKnowledgeStatus 校验知识条目状态。
func validKnowledgeStatus(status string) bool {
	switch status {
	case model.KnowledgeStatusDraft, model.KnowledgeStatusPublished, model.KnowledgeStatusDeprecated:
		return true
	default:
		return false
	}
}

// record 写审计。
func (s *KnowledgeService) record(ctx context.Context, op Operator, action string, detail map[string]any) {
	if s.audit == nil {
		return
	}
	s.audit.RecordAsync(ctx, AuditEntry{
		UserID: op.UserID, Username: op.Username, ActionType: action, Level: LevelLow,
		IPAddress: op.IP, UserAgent: op.Agent, Detail: detail,
	})
}

// Feedback 记录诊断反馈（有用/没用，5.6 质量基线）。
func (s *KnowledgeService) Feedback(ctx context.Context, diagnosisID int64, feedback string, operator Operator) error {
	switch feedback {
	case model.FeedbackUseful, model.FeedbackUseless, model.FeedbackAdopted:
	default:
		return apperr.Newf(apperr.CodeInvalidParam, "反馈类型 %q 非法（可选 useful/useless/adopted）", feedback)
	}
	if s.diagnoses == nil {
		return apperr.New(apperr.CodeInternal, "诊断仓储未初始化")
	}
	if _, err := s.diagnoses.Get(ctx, diagnosisID); err != nil {
		if repository.EnsureNotFound(err) {
			return apperr.New(apperr.CodeNotFound, "诊断记录不存在")
		}
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	if err := s.diagnoses.UpdateFeedback(ctx, diagnosisID, feedback); err != nil {
		return apperr.Wrap(apperr.CodeInternal, err)
	}
	s.record(ctx, operator, "ai_feedback", map[string]any{"diagnosis_id": diagnosisID, "feedback": feedback})
	return nil
}

// QualityStats 返回质量护栏指标（采纳率/反馈分布/平均耗时）。
func (s *KnowledgeService) QualityStats(ctx context.Context, since time.Time) (map[string]any, error) {
	feedback, err := s.diagnoses.CountFeedback(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	total, avgDuration, tokens, degraded, err := s.diagnoses.Stats(ctx, since)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, err)
	}
	useful := feedback[model.FeedbackUseful] + feedback[model.FeedbackAdopted]
	rated := useful + feedback[model.FeedbackUseless]
	adoption := 0.0
	if rated > 0 {
		adoption = float64(useful) / float64(rated)
	}
	return map[string]any{
		"diagnosis_total": total,
		"avg_duration_ms": roundFloat(avgDuration, 1),
		"total_tokens":    tokens,
		"degraded_count":  degraded,
		"feedback":        feedback,
		"adoption_rate":   roundFloat(adoption, 3),
		"rated_count":     rated,
	}, nil
}
