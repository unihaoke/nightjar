package service

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"middleware-ops/internal/model"
)

// 本文件处理「一次尝试的开始/结束」语义，以及"下一步该点哪个按钮"的判定。
//
// 背景（真实反馈）：
//  1. 「重新应用」在远程集成上必然失败——它不带 SSH 凭据就发起安装，于是留下一条
//     "远程安装需要 SSH 凭据" 的失败记录；使用者编辑集成、填好私钥保存后，保存接口返回的
//     `last_error` **仍然是上一次的旧错误**（新的尝试在后台跑，清错误发生在成功之后），
//     界面于是先弹一句"已保存，但存在待处理项：远程安装需要 SSH 凭据"，看起来像修复没生效。
//  2. 界面有两个入口都能"重来一次"：「重新应用」（只重做部署）与「重试建号」（账号 + 连接 + 部署），
//     但待处理项横幅一律把人引到账号弹窗，让人怀疑两者重复。

// NextAction 是「待处理项」横幅应该引导使用者点哪个按钮。
type NextAction string

const (
	// NextActionReapply：部署类失败（安装/连接目标机/端口不通等）→ 点「重新应用」（远程会先问 SSH 凭据）。
	NextActionReapply NextAction = "reapply"
	// NextActionRetryAccount：账号类失败（未建号/口令不一致/权限不足）→ 去「重试建号」。
	NextActionRetryAccount NextAction = "retry_account"
	// NextActionInvestigate：原因不明确（如 Exporter up=0 的自检结论）→ 引导看详情。
	NextActionInvestigate NextAction = "investigate"
)

// nextActionLabel 返回按钮文案（前端不再自己判断文案，避免两处入口说法不一致）。
func nextActionLabel(action NextAction) string {
	switch action {
	case NextActionReapply:
		return "重新应用（远程会先问 SSH 凭据）"
	case NextActionRetryAccount:
		return "去重试建号"
	case NextActionInvestigate:
		return "查看诊断"
	default:
		return ""
	}
}

// accountFailureKeywords 是"账号类失败"的特征词：命中就该去「重试建号」，而不是反复重新应用。
var accountFailureKeywords = []string{
	"账号", "建号", "口令不一致", "认证失败", "WRONGPASS", "NOAUTH", "access denied",
	"permission denied", "授权", "GRANT", "SQLSTATE", "管理员凭据", "尚未由平台创建",
}

// deployFailureKeywords 是"部署类失败"的特征词：命中就点「重新应用」。
var deployFailureKeywords = []string{
	"远程安装", "SSH", "ansible", "Ansible", "安装失败", "Exporter", "exporter",
	"镜像", "docker", "Docker", "端口", "wait_for", "探活", "探测", "sshpass",
}

// classifyNextAction 按最近一次失败原因给出下一步动作。
//
// 优先级：账号类关键词优先于部署类——"口令不一致"这类原因里常同时出现 Exporter 字样
// （例如"Exporter 未跑通：认证失败"），此时正确的动作是去改账号而不是重装。
func classifyNextAction(lastError string) NextAction {
	text := strings.TrimSpace(lastError)
	if text == "" {
		return ""
	}
	for _, keyword := range accountFailureKeywords {
		if strings.Contains(text, keyword) {
			return NextActionRetryAccount
		}
	}
	for _, keyword := range deployFailureKeywords {
		if strings.Contains(text, keyword) {
			return NextActionReapply
		}
	}
	return NextActionInvestigate
}

// beginAttempt 记录"新的一次尝试已经开始"：写下进度说明并**清掉上一次的失败**。
//
// 为什么必须清：保存/重新应用都是**异步**的，接口立刻返回视图。如果不清，
// 界面拿到的 last_error 还是上一次的旧原因，使用者会以为刚填的凭据没生效（见文件头背景）。
// 新的失败会在后台任务结束时由 markError 重新写入——不会丢失信息。
//
// 返回值是**清理后的实例**：调用方必须用它组装响应视图，否则视图里的 last_error
// 仍来自调用方手上那份旧快照，等于白清（真实反馈就是这样漏掉的）。
func (s *IntegrationService) beginAttempt(ctx context.Context, id int64, what string) *model.MiddlewareInstance {
	item, err := s.instances.Get(ctx, id)
	if err != nil {
		s.log.Warn("集成：读取实例失败，无法清理上次失败原因", zap.Int64("id", id), zap.Error(err))
		return nil
	}
	s.writeMeta(ctx, item, func(meta *IntegrationMeta) {
		meta.DeployNote = pendingNote(what)
		meta.LastError = ""
	})
	return item
}
