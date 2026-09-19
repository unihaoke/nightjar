package service

import (
	"context"
	"errors"
	"strings"

	"go.uber.org/zap"

	"middleware-ops/internal/model"
	"middleware-ops/internal/repo"
	"middleware-ops/internal/repository"
)

// errCredentialCipherMissing 表示平台没有加密能力（主密钥未配置）。
//
// 这种情况下**拒绝保存凭据**而不是退化成明文：使用者看不到差别，但泄露风险是真实的。
var errCredentialCipherMissing = errors.New(
	"平台未配置加密密钥（security.master_key / MWOPS_SECURITY_MASTER_KEY），无法安全保存仓库凭据：" +
		"请先配置主密钥后重启平台，或改用公开仓库地址")

// 本文件收拢「代码仓库凭据」的读写规则（INC-031）。
//
// 一句话规则：**入库的 repo_url 不含凭据，凭据单独加密存放，只在执行 git 的那一刻拼回去。**
// 之所以集中在这里而不是分散在两个服务（日志告警 / 代码分析）里：
// 读取路径有两处（页面 CRUD、事件后处理的拉取），任何一处漏掉"解密"或"拆分"，
// 表现都是"保存了令牌但拉不下来"或"令牌又一次被写进库"，很难从现象倒推。

// decryptRepoCredential 取出仓库的明文凭据（用于拼接 git URL）。
//
// 两种来源都要认：
//  1. 新数据：CredentialEncrypted 里的密文；
//  2. 旧数据：凭据内嵌在 RepoURL 里（本设计之前就是这样教使用者填的）。
//     这里**只读取不迁移**，迁移交给 migrateRepoCredential（需要写库，得由持有仓储的一侧做）。
func decryptRepoCredential(cipher cipherCodec, item *model.CodeRepo) string {
	if item == nil {
		return ""
	}
	if enc := strings.TrimSpace(item.CredentialEncrypted); enc != "" && cipher != nil {
		if plain, err := cipher.Decrypt(enc); err == nil {
			return strings.TrimSpace(plain)
		}
	}
	if _, legacy := repo.SplitCredentials(item.RepoURL); legacy != "" {
		return legacy
	}
	return ""
}

// encryptRepoCredential 加密凭据；空凭据返回空串（表示"没有凭据"）。
func encryptRepoCredential(cipher cipherCodec, credential string) (string, error) {
	trimmed := strings.TrimSpace(credential)
	if trimmed == "" {
		return "", nil
	}
	if cipher == nil {
		// 没有加密能力时**拒绝保存**而不是退化成明文：这条链路上"存了但没加密"
		// 与"没存"在使用者眼里一样（拉不下代码时会来问为什么），而泄露风险却真实存在。
		return "", errCredentialCipherMissing
	}
	return cipher.Encrypt(trimmed)
}

// prepareRepoCredential 把"旧数据里内嵌的凭据"迁移成"干净 URL + 加密凭据"。
//
// 返回值 changed=true 表示**内存里的 item 已被修改**，调用方需要落库一次。
// 幂等：迁移过的数据再次调用不会再改动（URL 已干净、密文已存在）。
//
// 为什么在读取路径上做迁移而不是写一个一次性脚本：使用者的旧数据就在库里，
// 而升级步骤里最容易被跳过的就是"记得跑那个迁移脚本"。放在读取路径上，
// 第一次访问该记录时自动完成，且不需要停机窗口。
func prepareRepoCredential(cipher cipherCodec, item *model.CodeRepo) (changed bool, err error) {
	if item == nil {
		return false, nil
	}
	original := item.RepoURL
	cleanURL, embedded := repo.SplitCredentials(item.RepoURL)
	if embedded == "" {
		// 没有内嵌凭据：只做一次防御性净化（例如 URL 里带了 ?token= 而当时没能拆分）。
		if sanitized := repo.SanitizeURL(item.RepoURL); sanitized != item.RepoURL {
			item.RepoURL = sanitized
			return true, nil
		}
		return false, nil
	}
	item.RepoURL = cleanURL
	// 已有密文时不覆盖：使用者可能刚在页面上换过令牌，而旧 URL 因为别的原因还带着凭据。
	if strings.TrimSpace(item.CredentialEncrypted) == "" {
		encrypted, encErr := encryptRepoCredential(cipher, embedded)
		if encErr != nil {
			// 加密失败时**必须把原地址还原**：URL 里那份凭据是它唯一的副本，
			// 清掉地址又没存下密文，等于把使用者的令牌弄丢了（下次拉代码必然认证失败，
			// 而使用者无从知道"是平台把它删了"）。
			item.RepoURL = original
			return false, encErr
		}
		item.CredentialEncrypted = encrypted
	}
	return true, nil
}

// markRepoCredential 刷新"是否已配置令牌"这个只读派生字段（对外响应里只暴露布尔量）。
func markRepoCredential(item *model.CodeRepo) *model.CodeRepo {
	if item == nil {
		return nil
	}
	item.HasCredential = strings.TrimSpace(item.CredentialEncrypted) != "" ||
		repo.HasCredential(item.RepoURL)
	return item
}

// migrateRepoCredentialRow 在读取路径上迁移一条记录里的旧凭据，并在真的改动时落库。
//
// 返回**同一条记录**（已就地修改）；调用方拿到的是"迁移后可用于拉取"的版本。
// 失败只告警不阻断：拿旧 URL 去 clone 通常仍能成功（地址里本来就带着令牌），
// 而"因为一次迁移失败就不给拉代码"是更差的结果。
func migrateRepoCredentialRow(
	ctx context.Context, repos *repository.CodeRepoRepository, cipher cipherCodec,
	log *zap.Logger, item *model.CodeRepo,
) *model.CodeRepo {
	if item == nil {
		return nil
	}
	changed, err := prepareRepoCredential(cipher, item)
	if err != nil {
		log.Warn("迁移仓库凭据失败（本次仍按原地址尝试）",
			zap.String("service", item.ServiceName), zap.Error(err))
		return markRepoCredential(item)
	}
	if !changed || repos == nil {
		return markRepoCredential(item)
	}
	if upErr := repos.Update(ctx, item); upErr != nil {
		log.Warn("回写迁移后的仓库凭据失败（本次仍按内存里的结果使用）",
			zap.String("service", item.ServiceName), zap.Error(upErr))
		return markRepoCredential(item)
	}
	log.Info("已把仓库地址里内嵌的凭据迁移为加密存储（地址已净化，页面与接口不再回显令牌）",
		zap.String("service", item.ServiceName), zap.String("repo_url", repo.SanitizeURL(item.RepoURL)))
	return markRepoCredential(item)
}

// applyCodeRepoInput 把表单入参写进记录，返回"凭据是否发生变化"。
//
// 密钥类字段的三条规矩（与 AI 设置的密钥处理一致，见 INC-018）：
//   - 留空 = 不修改（页面不回显密文，因此"没动这个框"必须等价于"保持原样"）；
//   - 显式清空 = ClearCredential（没有这个开关就永远删不掉令牌）；
//   - 地址里带的凭据照样会被拆出来加密（兼容使用者直接粘贴带令牌的 URL）。
func applyCodeRepoInput(cipher cipherCodec, item *model.CodeRepo, in CodeRepoInput) (credentialChanged bool, err error) {
	item.ServiceName = strings.TrimSpace(in.ServiceName)
	item.Branch = defaultString(strings.TrimSpace(in.Branch), "main")
	item.LocalPath = strings.TrimSpace(in.LocalPath)
	item.Language = strings.TrimSpace(in.Language)
	item.AllowThirdParty = in.AllowThirdParty

	// 地址先净化：无论来自"带令牌的粘贴"还是"页面回填的干净地址"，入库的都是干净地址。
	cleanURL, embedded := repo.SplitCredentials(in.RepoURL)
	item.RepoURL = cleanURL

	switch {
	case in.ClearCredential:
		item.CredentialEncrypted = ""
		return true, nil
	case strings.TrimSpace(in.Credential) != "":
		encrypted, encErr := encryptRepoCredential(cipher, in.Credential)
		if encErr != nil {
			return false, encErr
		}
		item.CredentialEncrypted = encrypted
		return true, nil
	case embedded != "":
		// 使用者在地址里直接写了令牌：按"设置新凭据"处理。
		encrypted, encErr := encryptRepoCredential(cipher, embedded)
		if encErr != nil {
			return false, encErr
		}
		item.CredentialEncrypted = encrypted
		return true, nil
	}
	return false, nil
}
