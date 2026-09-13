// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// write_ops.go 是**写面的域操作 API**（D-2 第 3 片）：上传（`WriteFile`）。
//
// 与 `write.go` 的分工（同 read_ops.go）：本文件承载领域逻辑（路径校验 → 并发互斥 →
// 重复/版本 → 卷路由与双账本 → 原子写与哈希 → 结算），`write.go` 的处理器只做
// 「解析 multipart/头 → 调域方法 → 写状态码与响应体」。
//
// 为什么必须抽出：远程写（Y 二期）与任何新表面都要复用**同一份**写语义——checksum 门禁、
// mtime、原子改名、版本保存、配额双账本、文件级锁、卷路由。若各写一份，这些不变量必然分叉。
package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// WriteFileInput 是上传（写文件）的领域入参。**不含任何 HTTP 类型**：`Mtime` 由调用方从
// `X-File-MTime` 头解析后传入（0 = 不设置）。
type WriteFileInput struct {
	// Owner 是操作主体（空 → anonymous，本方法内归一）。
	Owner string
	// RemotePath 是用户可见相对路径（含子目录）。
	RemotePath string
	// ExplicitVol 是 `?volume=` 显式卷（空 = 按 ACL/placement 自动路由）。
	ExplicitVol string
	// ExpectedChecksum 是客户端声明的 SHA-256（**必填**；不符即 400 并删除已写内容）。
	ExpectedChecksum string
	// ClientSize 是客户端声明的大小（用于成功文案与路由预检）。
	ClientSize int64
	// Mtime 是写入后要设置的文件修改时间（UnixNano；0 = 不设置）。
	Mtime int64
}

// WriteFileResult 是上传的领域结果。
type WriteFileResult struct {
	// VolumeName 是落盘卷名（空 = 无卷语义；调用方按需回 `X-Volume` 头）。
	VolumeName string
	// Checksum 是服务端计算的 SHA-256（幂等命中时为客户端声明的值）。
	Checksum string
	// Size 是落盘字节数（幂等命中时为已存在文件的大小）。
	Size int64
	// Idempotent 表示「同名同 checksum 已存在、未写盘」（历史语义：200 成功）。
	Idempotent bool
	// Message 是面向客户端的成功文案。
	//
	// **刻意由域侧给出**：两条成功路径的历史文案不同（幂等为「文件已上传成功，size: N」，
	// 正常为「文件上传成功，size: N」），把它们留在域侧可避免调用方各自拼文案而分叉。
	Message string
}

// WriteFile 把 src 写入目标卷租户根内的 `input.RemotePath`（原子写 + 流式哈希）。
//
// 执行顺序（与既有实现逐字一致，勿重排——顺序本身是不变量）：
//
//  1. 路径校验（pathguard + UserRel）→ 400
//  2. 文件级互斥（同 owner 同 rel 并发写拒绝）→ 409
//  3. 写前 home 定位 + 重复检测/版本保存（幂等 200 / 冲突 409 / 版本化覆盖）
//  4. 卷路由 + 双账本预留（ACL/placement/容量）→ 403/409/507/500
//  5. 建中间目录 → 原子写入 → 校验和比对（不符即删并 400）→ 双账本结算
//  6. checksum 台账写入 + mtime 落地
//
// 失败一律以 `*HTTPError` 表达（含可选 `Checksum`：上传冲突时附带服务端实际 checksum，
// 方便客户端决策——历史契约的一部分），调用方据此写响应。
func (s *Service) WriteFile(ctx context.Context, input WriteFileInput, src io.Reader) (WriteFileResult, error) {
	owner := normalizeOwner(input.Owner)
	logger := s.rt.logger()

	remotePath, rel, err := s.resolveWritePath(owner, input.RemotePath)
	if err != nil {
		return WriteFileResult{}, err
	}
	logger.DebugContext(ctx, "上传路径", "remote_path", remotePath)

	// 并发上传防护：防止同一 owner 同 rel 被多个上传请求同时写入导致 OOM。
	// 键空间与 delete / move / 分块 complete 共用（FileLocks 实现负责归一 owner 与分隔符）。
	releaseUpload, acquired := s.rt.fileLocks().TryMark(owner, rel, uploadingLockUpload)
	if !acquired {
		logger.WarnContext(ctx, "文件正在上传中，拒绝并发上传", "file_name", remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusConflict, Message: "文件正在上传中"}
	}
	defer releaseUpload()

	out := WriteFileResult{}

	// 重复检测与版本管理（自动路由）：先做「写前视图定位」确认 rel 的真实 home 卷（F1-A/B），
	// **只在 home 命中时**对其根做幂等/冲突/版本检查——幂等同 checksum 重传在容量/配额检查前
	// 直接 200（不因卷满误拒）；命中 → 覆盖写必须 stay-home 到 home 卷（容量路由只用于新文件，
	// 否则同 rel 跨卷双份 + owner 双计）。显式 volume 不做此检查：其语义是新文件定位，同名已
	// 存在（含同 checksum）一律由 routeUpload 唯一性 409。
	//
	// F-1（AD-6 写侧闭合）：home 以 owner 卷视图为界（LocateOwnerFile 只搜 AllowedVolumes）。
	// locate miss（rel 不在 owner 视图任何卷，含「默认卷被 ACL 排除但仍留 owner 遗留」）→
	// 该 rel 不属于 owner 逻辑树，**跳过 dup-check 视为新文件**交容量路由——不得对无权卷的
	// 遗留做版本化覆盖写（泄漏到无权卷 version/）或假 409/假幂等（误伤新写入）。
	forceHomeVol := ""
	if input.ExplicitVol == "" {
		loc, found := s.rt.locateOwnerFile(owner, rel)
		if found && loc.Tenant != nil {
			forceHomeVol = loc.VolumeName
			idem, existed, dupErr := s.handleDuplicateFile(ctx, owner, loc.Tenant, rel, input.ExpectedChecksum, remotePath)
			if dupErr != nil {
				return WriteFileResult{}, dupErr
			}
			if idem != nil { // 幂等命中：已存在且 checksum 匹配，直接成功（不写盘）
				out.VolumeName = forceHomeVol
				out.Checksum = idem.Checksum
				out.Size = idem.Size
				out.Idempotent = true
				out.Message = idem.Message
				return out, nil
			}
			// existed=true = home 卷命中且继续（版本化覆盖写）→ stay-home（forceHomeVol 保留，
			// 容量不足 RouteUpload 直接 507 不换卷）。existed=false 是 locate 命中与 dup-check
			// 间 TOCTOU 的防御（理论竞态）→ 交容量路由。
			if !existed {
				forceHomeVol = ""
			}
		}
		// locate miss → 新文件：交容量路由（无 dup-check；VolSet==nil 唯一根下 stat miss
		// 等价旧行为——handleDuplicateFile 在 miss 时本就返回 false）。
	}

	// 卷路由 + 双账本预留（T4/T5）：RouteUpload 按 ACL/placement 选目标卷，在 owner 全局
	// Scope + 卷容量池双 TryReserve；显式 volume= 时做 ACL 校验与唯一性查重（403/409）；
	// forceHomeVol 非空时强制该 home 卷单候选（容量不足 507 不换卷）。
	route, routeErr := s.rt.routeUpload(owner, rel, input.ExplicitVol, input.ClientSize, forceHomeVol)
	if routeErr != nil {
		logger.WarnContext(ctx, "上传卷路由拒绝", "file_name", remotePath, "error", routeErr.Error())
		var he *HTTPError
		if errors.As(routeErr, &he) {
			return WriteFileResult{}, he
		}
		logger.ErrorContext(ctx, "上传卷路由失败", "file_name", remotePath, "error", routeErr.Error())
		return WriteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgSaveFailed}
	}
	if route.Tenant == nil || route.Tenant.Root() == nil {
		route.Release()
		return WriteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	out.VolumeName = route.VolumeName
	root := route.Tenant.Root()

	if mkdirErr := root.MkdirAll(filepath.Dir(rel), 0755); mkdirErr != nil {
		route.Release()
		logger.ErrorContext(ctx, "创建目录失败", "error", mkdirErr.Error())
		return WriteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: "创建目录失败"}
	}

	// 覆盖写场景先统计旧文件大小 prev（双 Adjust 差分用）。
	prev := int64(0)
	if stat, statErr := root.Stat(rel); statErr == nil {
		prev = stat.Size()
	}

	// 原子写入 + 流式哈希（目标卷 root）。
	serverChecksum, written, wErr := writeFileAtomicallyRoot(ctx, root, rel, src)
	if wErr != nil {
		route.Release()
		logger.ErrorContext(ctx, "保存文件失败", "error", wErr.Error(), "file_name", remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgSaveFailed}
	}

	if serverChecksum != input.ExpectedChecksum {
		// 清理已写入的校验失败文件，忽略错误（临时文件由 writeFileAtomicallyRoot 清理）
		_ = root.Remove(rel)
		route.Release()
		logger.WarnContext(ctx, "文件 SHA-256 校验失败", "server", serverChecksum, "client", input.ExpectedChecksum, "file_name", remotePath)
		return WriteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "文件 SHA-256 校验失败"}
	}

	// 双账本结算：覆盖写 Adjust(prev, written) + Release（旧文件已占用 prev，差分收敛到
	// 新大小）；新文件 Commit(written)。owner 全局 Scope 与卷容量池同语义。
	route.Commit(prev, written)

	// 成功后的副作用：checksum 台账写入 + mtime 落地（原 setUploadResponseHeaders 的领域部分）。
	s.recordUploadSuccess(root, owner, remotePath, rel, serverChecksum, input.Mtime, logger)

	out.Checksum = serverChecksum
	out.Size = written
	out.Message = fmt.Sprintf("文件上传成功, size: %d", input.ClientSize)
	return out, nil
}

// resolveWritePath 校验并映射写路径（pathguard + UserRel），失败以 *HTTPError{400} 表达。
//
// 与 read_ops 的差异：写路径的非法错误**沿用 pathguard 的原始文案**（历史契约：客户端看到
// 的是 ValidateFilePath 的具体原因），而 UserRel 失败回统一 errMsgInvalidPath。
func (s *Service) resolveWritePath(owner, filename string) (remotePath, rel string, err error) {
	remotePath, vErr := pathguard.ValidateFilePath(filename)
	if vErr != nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: vErr.Error()}
	}
	tnt := s.rt.tenantOf(owner)
	if tnt == nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	// 服务端内部目录访问防护收敛到 UserRel（逐段 ValidSegmentName 拒绝 .__ 前缀）：
	// 用户显式上传/重命名不得落到服务端内部目录，user/ 桶内 .__ 前缀段已被
	// ValidSegmentName 拒绝，无需再单独内部目录守卫。
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	return remotePath, rel, nil
}

// duplicateOutcome 是「同名已存在且 checksum 匹配」的幂等结果（非 nil 表示已处理完、无需写盘）。
type duplicateOutcome struct {
	Checksum string
	Size     int64
	Message  string
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
//
// 返回：
//   - (非 nil, _, nil)：幂等命中（已存在且 checksum 匹配）——调用方直接回成功，不写盘；
//   - (nil, true, nil)：home 卷命中且应继续「覆盖写」（版本化启用时已保存旧版本）；
//   - (nil, false, nil)：未命中（新文件 / 非本卷 home）——继续正常上传；
//   - (nil, _, err)：冲突且 versioning 关闭 → *HTTPError{409}（可能附服务端 checksum）。
//
// homeTnt 为已定位的 home 卷租户；rel 为租户根内相对路径。
func (s *Service) handleDuplicateFile(ctx context.Context, owner string, homeTnt *storage.Tenant, rel, expectedChecksum, remotePath string) (*duplicateOutcome, bool, error) {
	if homeTnt == nil || homeTnt.Root() == nil {
		return nil, false, nil
	}
	root := homeTnt.Root()
	stat, statErr := root.Stat(rel)
	if statErr != nil {
		return nil, false, nil // 文件不存在，继续正常上传（新文件或非本卷 home）
	}
	if verifyFileWithChecksumRoot(root, rel, expectedChecksum) {
		// 幂等上传：文件已存在且 checksum 匹配，直接返回成功（不保存版本）
		return &duplicateOutcome{
			Checksum: expectedChecksum,
			Size:     stat.Size(),
			Message:  fmt.Sprintf("文件已上传成功, size: %d", stat.Size()),
		}, true, nil
	}
	// 与基线 `cfg := h.cfgPtr.Load(); if cfg.Versioning.Enabled` 逐字同源（接缝的
	// VersioningEnabled 即该形态，cfg 未装配时同样 panic——见 pkg/server/handlers.go 的
	// 接缝注释），故本处**无控制流残差**。
	if s.rt.versioningEnabled() {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本（homeTnt 即旧文件所在卷）
		s.saveVersionBeforeOverwrite(owner, remotePath, homeTnt)
		// 审查 I-3：覆盖动作记审计（含旧版本已保存的信息）。
		s.rt.recordFileAudit(ctx, "overwrite", remotePath, auditResultSuccess, "覆盖现有文件（版本已保存）")
		return nil, true, nil // 继续执行写入流程，用新内容覆盖现有文件（home=本卷）
	}
	// checksum 不匹配：冲突，需保留现有文件
	s.rt.logger().WarnContext(ctx, "文件已存在，但校验失败", "file_name", remotePath)
	// 审查 I-3：versioning 关闭时同名覆盖是静默数据丢失，记审计（当前走冲突拒绝
	// 分支——保留现有文件，不覆盖；此处为 denied 留痕）。
	s.rt.recordFileAudit(ctx, "overwrite", remotePath, auditResultDenied, "文件已存在且 checksum 不匹配（versioning 关闭，拒绝覆盖）")
	// 附带服务端文件的实际 checksum，方便客户端决策
	he := &HTTPError{Status: http.StatusConflict, Message: "文件已存在，但校验失败"}
	if serverCS, csErr := fileChecksumRoot(root, rel); csErr == nil {
		he.Checksum = serverCS
	}
	return nil, true, he
}

// recordUploadSuccess 记录上传成功后的领域副作用：checksum 台账写入 + mtime 落地。
//
// 抽自原 setUploadResponseHeaders（其"写响应头"部分留回处理器，领域只保留台账与落盘副作用）。
func (s *Service) recordUploadSuccess(root *storage.Root, owner, remotePath, rel, serverChecksum string, mtime int64, logger *slog.Logger) {
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.Set(rel, serverChecksum)
	} else {
		logger.WarnContext(context.Background(), "per-tenant checksum store 不可用，跳过记录", "file_name", remotePath)
	}
	if mtime > 0 {
		modTime := time.Unix(0, mtime)
		if err := root.Chtimes(rel, modTime, modTime); err != nil {
			logger.WarnContext(context.Background(), "设置文件时间戳失败", "file_name", remotePath, "error", err)
		}
	}
}
