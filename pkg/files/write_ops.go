// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// write_ops.go 是**写面的域操作 API**：上传（`WriteFile`）、目录（`MakeDir` / `RemoveDir`）、
// 重命名（`RenameFile`）、删除（`DeleteFile`）。
//
// 与 `write.go` / `dirs.go` / `rename.go` / `delete.go` 的分工（同 read_ops.go）：本文件承载
// 领域逻辑（路径校验 → 并发互斥 → 重复/版本 → 卷路由与双账本 → 原子写与哈希 → 结算；
// 以及重命名/删除的跨卷定位、checksum 门禁、配额与台账释放），四个 HTTP 文件只做
// 「解析请求/头 → 调域方法 → 写状态码与响应体」。
//
// 为什么必须抽出：远程写（Y 二期）与任何新表面都要复用**同一份**写语义——checksum 门禁、
// mtime、原子改名、版本保存、配额双账本、文件级锁、卷路由、审计。若各写一份，这些不变量必然分叉。
package files

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
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

// ---- 目录族域操作（mkdir / rmdir）----
//
// 命名说明：域操作用 MakeDir / RemoveDir（与 sync.FS.MakeDir 同词），HTTP 处理器仍叫
// Mkdir / Rmdir——前者是领域动作，后者是 HTTP 面，两者同名会互相遮蔽（且无法同时存在）。

// MakeDirResult 是建目录的领域结果。
type MakeDirResult struct {
	// RemotePath 是校验后的用户可见相对路径（调用方据此组文案/回包）。
	RemotePath string
}

// MakeDir 在 owner 视图内**首个卷**（默认卷优先）创建目录树。
//
// 错误语义（*HTTPError）：400 = dirname 为空 / 路径非法 / 租户不可用；500 = 创建失败。
func (s *Service) MakeDir(owner, dirname string) (MakeDirResult, error) {
	if dirname == "" {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "dirname 不能为空"}
	}
	remotePath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录名: " + err.Error()}
	}
	// 路径映射与卷无关（user/<path> 相对各卷租户根），用默认租户做纯路径校验；
	// 实际落盘目录选 owner 视图内首个卷（默认卷优先——默认卷开放时即默认租户，零回归；
	// 默认卷被 ACL 排除时落到视图卷，绝不经默认租户直写默认卷遗留，AD-6 闭合）。
	owner = normalizeOwner(owner)
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录路径"}
	}
	rel, ok := tnt0.UserRel(remotePath)
	if !ok {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录路径"}
	}
	target := s.primaryViewTenant(owner)
	if target == nil || target.Root() == nil {
		return MakeDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录路径"}
	}
	if mkErr := target.Root().MkdirAll(rel, 0755); mkErr != nil {
		s.rt.logger().Error(errMsgCreateDirFailed, "dir", remotePath, "error", mkErr)
		return MakeDirResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: errMsgCreateDirFailed}
	}
	s.rt.logger().Info("目录已创建", "dir", remotePath)
	return MakeDirResult{RemotePath: remotePath}, nil
}

// RemoveDirResult 是删目录的领域结果。
type RemoveDirResult struct {
	// RemotePath 是校验后的用户可见相对路径。
	RemotePath string
}

// RemoveDir 递归删除目录树（owner 视图内**每一个**存在该目录的卷都删）。
//
// 多卷语义：同一子目录可能因换卷在多个卷上并存（目录非文件，不受 AD-4 唯一性约束），
// 故逐卷定位、逐卷删除；文件级配额按 per-file rel 释放（跨卷合计正确），卷容量池按卷释放。
//
// 错误语义（*HTTPError）：
//   - 400：dirname 为空 / 路径非法 / 租户不可用 / 目标不是目录 / 目标为符号链接 / 未带 force
//   - 404：目录不存在（任何卷上都没有）
//   - 500：访问目录失败 / 删除失败
func (s *Service) RemoveDir(owner, dirname string, force bool) (RemoveDirResult, error) {
	if dirname == "" {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "dirname 不能为空"}
	}
	remotePath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录名: " + err.Error()}
	}
	// 归一 owner（空 → anonymous）：目录探测/删除与列表/写路径同键，未认证请求归属 anonymous。
	owner = normalizeOwner(owner)
	// 路径映射与卷无关，用默认租户做纯路径校验；卷感知只决定目录落到哪些卷的租户根。
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录路径"}
	}
	rel, ok := tnt0.UserRel(remotePath)
	if !ok {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的目录路径"}
	}

	// 收集 owner 视图内存在该目录的卷租户（默认卷优先；目录不存在于任何卷 → 404）。
	type rmTarget struct {
		volName string
		tnt     *storage.Tenant
	}
	var targets []rmTarget
	if s.rt.volSet() == nil {
		targets = append(targets, rmTarget{volName: "", tnt: tnt0})
	} else {
		view := volume.AllowedVolumes(s.rt.volSet().All(), owner)
		for _, v := range view {
			rt := s.rt.volSet().Root(v.Name)
			if rt == nil {
				continue
			}
			// 探测用卷根相对 <owner>/<rel>（不创建租户目录）；确认存在再取租户操作。
			if _, statErr := rt.Stat(owner + "/" + rel); statErr != nil {
				continue
			}
			tnt := s.rt.volumeTenant(v.Name, owner)
			if tnt == nil || tnt.Root() == nil {
				continue
			}
			targets = append(targets, rmTarget{volName: v.Name, tnt: tnt})
		}
	}
	if len(targets) == 0 {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusNotFound, Message: "目录不存在"}
	}

	// 符号链接 / 非目录检查与 TOCTOU 二次检查：在首个命中卷（默认卷优先）执行，错误语义与
	// 单卷一致（目录不存在 404 / 符号链接或非目录 400）。
	primary := targets[0]
	if vErr := validateRmdirTarget(primary.tnt.Root(), rel); vErr != nil {
		switch {
		case os.IsNotExist(vErr):
			return RemoveDirResult{}, &HTTPError{Status: http.StatusNotFound, Message: "目录不存在"}
		case errors.Is(vErr, errRmdirSymlink):
			return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "不允许删除符号链接"}
		case errors.Is(vErr, errRmdirNotDir):
			return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "指定路径不是目录"}
		default:
			return RemoveDirResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: "访问目录失败"}
		}
	}

	// force 必须为 true 才执行删除（避免误删）
	if !force {
		return RemoveDirResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "请使用 ?force=true 确认删除"}
	}

	// 逐卷删除存在该目录的子树；每卷删除前收集树内文件 {rel,size}（per-file 分键释放 owner
	// 全局 Scope——跨卷合计语义正确，文件 rel 唯一故不双计），并按所在卷释放卷容量池。
	var allFiles []rmdirFileStat
	for _, tg := range targets {
		root := tg.tnt.Root()
		var dirFiles []rmdirFileStat
		sumRootDirFiles(root, rel, &dirFiles)
		if rmErr := root.RemoveAll(rel); rmErr != nil {
			s.rt.logger().Error("删除目录失败", "dir", remotePath, "error", rmErr)
			return RemoveDirResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: "删除目录失败"}
		}
		allFiles = append(allFiles, dirFiles...)
		// 卷容量池释放：本卷被删字节 = dirFiles 之和。
		if tg.volName != "" && s.rt.volSet() != nil {
			if pool := s.rt.volSet().Pool(tg.volName); pool != nil {
				var volBytes int64
				for _, f := range dirFiles {
					volBytes += f.size
				}
				if volBytes > 0 {
					pool.ReleaseCommitted(volBytes)
				}
			}
		}
	}

	// 删除成功后按各文件实际子 Scope（按 rel 解析）释放配额占用。
	for _, f := range allFiles {
		if scope := s.rt.quotaScope(owner, f.rel); scope != nil {
			scope.ReleaseUsage(f.size)
		}
	}

	// 清理 per-tenant checksum store 中该目录下所有文件的记录（key = rel，无 owner 前缀）。
	// 使用 "/" 分隔符，与 ChecksumStore 的 key 格式约定保持一致（所有 key 使用 filepath.ToSlash 格式）。
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.DeletePrefix(rel + "/")
		// 清理目录自身的 checksum 记录（如果存在）
		cs.Delete(rel)
	}

	s.rt.logger().Info("目录已删除", "dir", remotePath)
	return RemoveDirResult{RemotePath: remotePath}, nil
}

// ---- 重命名族域操作（rename）----
//
// 命名说明：域操作 RenameFile，HTTP 处理器 Rename（批量族 BatchRename 在 P2-c 收敛到本方法）。

// RenameFileInput 是重命名的领域入参。**不含任何 HTTP 类型**。
type RenameFileInput struct {
	// Owner 是操作主体（空 → anonymous，本方法内归一）。
	Owner string
	// From / To 是用户可见相对路径（原始值；本方法内校验，非法 → 400）。
	From string
	To   string
	// ExpectedChecksum 是客户端声明的源文件 SHA-256（**必填**，不符即拒绝）。
	ExpectedChecksum string
	// ExplicitVol 是 `?volume=` 显式卷（空 = 按 owner 卷视图定位源）。
	ExplicitVol string
}

// RenameFileResult 是重命名的领域结果。
type RenameFileResult struct {
	// From / To 是**校验后**的用户可见路径（调用方据此组文案）。
	From string
	To   string
	// Checksum 是成功时回显的客户端 checksum；「同源同目标」分支留空（历史契约：该分支不携带 checksum）。
	Checksum string
	// Message 是面向客户端的成功文案（由域侧给出，两条成功路径文案不同）。
	Message string
}

// RenameFile 在源文件所在 home 卷内完成重命名（同卷移动；跨卷移动走 move API）。
//
// 执行顺序（与既有处理器逐字一致，勿重排——顺序本身是不变量）：
//  1. 路径校验（空 from/to → 400；pathguard 非法 → 400）
//  2. 同源同目标 → 直接成功（**在校验 checksum 之前**短路，且不回显 checksum）
//  3. checksum 头缺失 → 400
//  4. 租户 / UserRel 解析失败 → 400
//  5. 跨卷定位 home：显式卷未命中 / 默认卷被 ACL 排除 → 404（fail-closed，AD-6）
//  6. AD-4 唯一性：目标 rel 已在其它卷存在 → 409
//  7. renameInHome：源存在 → 目标不存在 → checksum 门禁 → 建父目录 → 配额转移 → 原子改名
//
// 失败一律以 *HTTPError 表达（状态码与文案即对外契约）。
func (s *Service) RenameFile(ctx context.Context, input RenameFileInput) (RenameFileResult, error) {
	logger := s.rt.logger()

	from, to, err := validateRenamePaths(input.From, input.To)
	if err != nil {
		return RenameFileResult{}, err
	}
	if from == to {
		return RenameFileResult{From: from, To: to, Message: "源与目标相同，无需移动"}, nil
	}
	if input.ExpectedChecksum == "" {
		return RenameFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgMissingChecksum}
	}

	owner := normalizeOwner(input.Owner)
	fromRel, toRel, tnt, ok := s.resolveRenamePaths(owner, from, to)
	if !ok || tnt == nil || tnt.Root() == nil {
		return RenameFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}

	// 跨卷定位源 home（任务 5）：文件可能因换卷落在非默认卷，rename 在 home 卷内完成
	// （同卷；跨卷移动走 T6 move API）。带显式 ?volume= 只在指定卷定位源（不在 → 404）。
	// 全视图未命中仅当默认卷对 owner 授权才回落默认租户（由 renameInHome 的 Stat 产出 404，
	// 与单卷一致）；默认卷被 ACL 排除时不得回落——否则 owner 可 rename 默认卷自身路径的
	// 遗留文件（ACL bypass，AD-6）。
	loc, found := s.locateForRead(owner, fromRel, input.ExplicitVol)
	var homeVol string
	var root *storage.Root
	switch {
	case found && loc.Tenant != nil:
		homeVol = loc.VolumeName
		root = loc.Tenant.Root()
	case input.ExplicitVol != "":
		return RenameFileResult{}, &HTTPError{Status: http.StatusNotFound, Message: "源文件不存在"}
	case !s.defaultVolumeAllows(owner):
		return RenameFileResult{}, &HTTPError{Status: http.StatusNotFound, Message: "源文件不存在"}
	default:
		root = tnt.Root()
	}

	// AD-4 唯一性：目标 rel 不得已存在于 owner 视图其它卷（否则 rename 后同逻辑路径跨卷
	// 双份）。目标已在同一 home 卷由 renameInHome 的 Stat 捕获（409）；目标在其它卷 →
	// 直接 409（跨卷移动非本任务语义）。单卷/无卷语义时 homeVol 空 → 跳过。
	if homeVol != "" && s.rt.volSet() != nil {
		if dstLoc, dstFound := s.rt.locateOwnerFile(owner, toRel); dstFound && dstLoc.VolumeName != homeVol {
			return RenameFileResult{}, &HTTPError{Status: http.StatusConflict, Message: "目标路径已存在"}
		}
	}

	if renErr := s.renameInHome(ctx, renameHomeArgs{
		owner:            owner,
		root:             root,
		fromRel:          fromRel,
		toRel:            toRel,
		from:             from,
		to:               to,
		expectedChecksum: input.ExpectedChecksum,
		logger:           logger,
	}); renErr != nil {
		return RenameFileResult{}, renErr
	}

	s.rt.recordFileAudit(ctx, "rename", from, auditResultSuccess, "to="+to)
	logger.InfoContext(ctx, "文件已重命名", "from", from, "to", to, "checksum", input.ExpectedChecksum)
	return RenameFileResult{
		From:     from,
		To:       to,
		Checksum: input.ExpectedChecksum,
		Message:  fmt.Sprintf("文件已重命名: %s -> %s", from, to),
	}, nil
}

// validateRenamePaths 校验 from/to 并返回**校验后**的路径（失败以 *HTTPError{400} 表达）。
// 三类文案与既有 parseRenameParams 逐字一致。
func validateRenamePaths(from, to string) (string, string, error) {
	if from == "" || to == "" {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: "from 和 to 都不能为空"}
	}
	vFrom, err := pathguard.ValidateFilePath(from)
	if err != nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: "无效的源路径"}
	}
	vTo, err := pathguard.ValidateFilePath(to)
	if err != nil {
		return "", "", &HTTPError{Status: http.StatusBadRequest, Message: "无效的目标路径"}
	}
	return vFrom, vTo, nil
}

// resolveRenamePaths 计算 from 和 to 在指定 owner 租户 user 桶下的相对路径。
// from 与 to 必须落在同一租户内（UserRel 保证 user/ 桶内）。返回租户与两条 rel。
func (s *Service) resolveRenamePaths(owner, from, to string) (fromRel, toRel string, tnt *storage.Tenant, ok bool) {
	tnt = s.rt.tenantOf(owner)
	if tnt == nil {
		return "", "", nil, false
	}
	var fok, tok bool
	fromRel, fok = tnt.UserRel(from)
	toRel, tok = tnt.UserRel(to)
	if !fok || !tok {
		return "", "", nil, false
	}
	return fromRel, toRel, tnt, true
}

// renameHomeArgs 是 renameInHome 的参数集合（go:S107：参数过多时收敛为结构体）。
// 只放**领域**入参——不含 http.ResponseWriter（域方法不写响应，失败以 *HTTPError 表达）。
type renameHomeArgs struct {
	owner            string
	root             *storage.Root
	fromRel          string
	toRel            string
	from             string
	to               string
	expectedChecksum string
	logger           *slog.Logger
}

// renameInHome 在给定 home 卷租户内完成一次重命名（含跨子目录配额对称转移）。
//
// 返回 nil 表示成功；失败返回 *HTTPError（状态码与文案即对外契约，由调用方写响应）。
// 除响应外的副作用一律保留：审计、业务日志、checksum 台账改名、配额对称转移。
func (s *Service) renameInHome(ctx context.Context, a renameHomeArgs) error {
	a.logger.InfoContext(ctx, "开始重命名", "from", a.fromRel, "to", a.toRel)
	if _, err := a.root.Stat(a.fromRel); os.IsNotExist(err) {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, "源文件不存在")
		return &HTTPError{Status: http.StatusNotFound, Message: "源文件不存在"}
	}
	// TODO: 此处存在 TOCTOU 竞态窗口（Stat 与 Rename 之间），后续优化为原子操作
	if _, err := a.root.Stat(a.toRel); err == nil {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultDenied, "目标路径已存在: "+a.to)
		// 审查 I-1：必须返回非 nil 错误——原 `return err`（err 恰为 nil）让调用方误判
		// 成功并追加一条假的 success 审计行（被拒绝的 rename 记为成功，破坏审计可信度）。
		return &HTTPError{Status: http.StatusConflict, Message: "目标路径已存在"}
	}
	if !verifyFileWithChecksumRoot(a.root, a.fromRel, a.expectedChecksum) {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultDenied, "checksum 不匹配")
		a.logger.WarnContext(ctx, "rename checksum 校验失败", "from", a.from)
		return &HTTPError{Status: http.StatusBadRequest, Message: errMsgSrcChecksumFailed}
	}
	if err := a.root.MkdirAll(filepath.Dir(a.toRel), 0755); err != nil {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, "创建父目录失败: "+a.to)
		a.logger.ErrorContext(ctx, errMsgCreateParentDirFailed, "to", a.to, "error", err.Error())
		return &HTTPError{Status: http.StatusInternalServerError, Message: errMsgCreateParentDirFailed}
	}
	// 配额：rename 在 user 桶内移动字节（总量不变，桶/租户级天然正确）。但跨 bucket_limits
	// 子目录时 committed 归属需对称转移——源目录键释放、目标目录键入账（子目录配额对 rename
	// 同样封顶，防止"先传受限目录外再 rename 进来"绕过）。非跨键（同目录/同键）零操作。
	// 两键相同时无需记账（释放+入账互相抵消）；装配层配额未装配（QuotaScopeFor 返回 nil）
	// 时退化为无配额记账（旧行为，仅总量正确）。
	if fromScope := s.rt.quotaScope(a.owner, a.fromRel); fromScope != nil {
		if toScope := s.rt.quotaScope(a.owner, a.toRel); toScope != nil && fromScope != toScope {
			if srcInfo, statErr := a.root.Stat(a.fromRel); statErr == nil {
				size := srcInfo.Size()
				// 目标键先 TryReserve（子目录/租户/全局逐级检查，配额不足拒绝移动避免超限），
				// 成功后再原子 Rename，最后源键 ReleaseUsage。若 Rename 失败则 Release 归还目标预留。
				toRes, err := toScope.TryReserve(size)
				if err != nil {
					s.rt.recordFileAudit(ctx, "rename", a.from, auditResultDenied, "目标目录配额不足: to="+a.to)
					a.logger.WarnContext(ctx, "rename 目标目录配额不足", "from", a.fromRel, "to", a.toRel, "size", size)
					return &HTTPError{Status: http.StatusInsufficientStorage, Message: "目标目录配额不足"}
				}
				if err := atomicRenameRoot(a.root, a.fromRel, a.toRel); err != nil {
					toRes.Release() // Rename 失败归还目标预留（源键未动）。
					s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, "重命名失败: "+a.to)
					a.logger.ErrorContext(ctx, "重命名失败", "from", a.from, "to", a.to, "error", err.Error())
					return &HTTPError{Status: http.StatusInternalServerError, Message: "重命名失败"}
				}
				toRes.Commit(size) // 目标键入账。
				fromScope.ReleaseUsage(size)
				if cs := s.rt.checksumStore(a.owner); cs != nil {
					cs.Rename(a.fromRel, a.toRel)
				}
				return nil
			}
			// 源文件 stat 失败（理论不可达：上方已 Stat 校验存在）：不记账，继续原链路。
		}
	}
	if err := atomicRenameRoot(a.root, a.fromRel, a.toRel); err != nil {
		s.rt.recordFileAudit(ctx, "rename", a.from, auditResultError, "重命名失败: "+a.to)
		a.logger.ErrorContext(ctx, "重命名失败", "from", a.from, "to", a.to, "error", err.Error())
		return &HTTPError{Status: http.StatusInternalServerError, Message: "重命名失败"}
	}
	if cs := s.rt.checksumStore(a.owner); cs != nil {
		cs.Rename(a.fromRel, a.toRel)
	}
	return nil
}

// ---- 删除族域操作（delete）----

// DeleteFileInput 是单文件删除的领域入参。**不含任何 HTTP 类型**。
//
// 注意与 `RemoveDir` 的分工：本方法删**单个文件**（checksum 门禁 + 幂等由调用方决定语义），
// `RemoveDir` 删**目录子树**（无 checksum，逐卷删除）。
type DeleteFileInput struct {
	// Owner 是操作主体（空 → anonymous，本方法内归一）。
	Owner string
	// RemotePath 是用户可见相对路径（原始值；本方法内校验，非法 → 400）。
	RemotePath string
	// ExpectedChecksum 是客户端声明的 SHA-256（**必填**且必须匹配，不符即拒绝删除）。
	ExpectedChecksum string
	// ExplicitVol 是 `?volume=` 显式卷（空 = 按 owner 卷视图定位）。
	ExplicitVol string
}

// DeleteFileResult 是单文件删除的领域结果。
type DeleteFileResult struct {
	// RemotePath 是**校验后**的用户可见路径。
	RemotePath string
	// Message 是面向客户端的成功文案（由域侧给出，避免调用方各自拼文案）。
	Message string
}

// DeleteFile 删除单个文件（含 checksum 门禁、文件级互斥、跨卷定位、配额/卷池/台账释放）。
//
// 执行顺序（与既有处理器逐字一致，勿重排——顺序本身是不变量）：
//  1. 路径校验（空 / pathguard / UserRel）→ 400
//  2. checksum 头缺失 → 400（**触盘前**拒绝）
//  3. 文件级互斥（与上传/move/restore/分块 complete 共用同 rel 锁）→ 409
//  4. 跨卷定位 home 卷 → 404（显式卷未命中 / 默认卷被 ACL 排除，fail-closed）
//  5. 开 fd → Stat → checksum 校验（不符 400，**保留文件**）→ 关闭 → 删除
//  6. 释放配额占用 + 卷容量池 + checksum 台账 + 计量 + 审计
//
// 失败一律以 *HTTPError 表达（状态码与文案即对外契约）。
func (s *Service) DeleteFile(ctx context.Context, input DeleteFileInput) (DeleteFileResult, error) {
	logger := s.rt.logger()

	filename := input.RemotePath
	if filename == "" {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgEmptyFilename}
	}
	// owner 归一后再解析租户：与 rename / mkdir 同一键（空 owner → anonymous）。
	owner := normalizeOwner(input.Owner)
	remotePath, vErr := pathguard.ValidateFilePath(filename)
	if vErr != nil {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidFilename}
	}
	tnt0 := s.rt.tenantOf(owner)
	if tnt0 == nil {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidFilename}
	}
	rel, relOK := tnt0.UserRel(remotePath)
	if !relOK {
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidFilename}
	}

	expectedChecksum := input.ExpectedChecksum
	if expectedChecksum == "" {
		logger.WarnContext(ctx, "X-File-Checksum 为空", "file_name", remotePath)
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgMissingChecksum}
	}

	// 跨卷定位（任务 5）：文件可能因换卷落在非默认卷。带显式 ?volume= 只在指定卷定位
	// （不在视图/卷上无此文件 → 404，fail-closed）。全视图未命中仅当默认卷对 owner 授权才
	// 回落默认租户（由 Open 产出 404/500，与单卷既有错误语义一致）；默认卷被 ACL 排除时不得
	// 回落——否则 owner 可经默认租户 Open 删除默认卷自身路径的遗留文件（ACL bypass，AD-6）。
	explicitVol := input.ExplicitVol

	// 文件级互斥（T6c move 锁架构延伸）：与单次上传 / 跨卷 move / 版本 restore / 分块 complete
	// 共用同 rel 锁。无锁时 delete 可在 move「复制成功 → 删源」窗口内先删源（move 侧虽有
	// IsNotExist 兜底，但语义依赖时序）；持锁后并发 move 直接 409，窗口闭合。
	release, locked := s.rt.fileLocks().Acquire(owner, rel)
	if !locked {
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultDenied, "文件正在移动/上传中")
		return DeleteFileResult{}, &HTTPError{Status: http.StatusConflict, Message: "文件正在移动/上传中，请稍后重试"}
	}
	defer release()

	loc, found := s.locateForRead(owner, rel, explicitVol)
	var homeVol string
	var root *storage.Root
	if found && loc.Tenant != nil {
		homeVol = loc.VolumeName
		root = loc.Tenant.Root()
	} else {
		if explicitVol != "" || !s.defaultVolumeAllows(owner) {
			return DeleteFileResult{}, &HTTPError{Status: http.StatusNotFound, Message: "文件不存在"}
		}
		tnt := s.rt.tenantOf(owner)
		if tnt == nil || tnt.Root() == nil {
			return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
		}
		root = tnt.Root()
	}

	// 基于 fd 操作缩小 TOCTOU 窗口：先打开文件，再基于 fd 执行 Stat 和 checksum 校验
	file, err := root.Open(rel)
	if err != nil {
		if os.IsNotExist(err) {
			s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "文件不存在")
			return DeleteFileResult{}, &HTTPError{Status: http.StatusNotFound, Message: "文件不存在"}
		}
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "打开文件失败")
		logger.ErrorContext(ctx, "打开文件失败", "file_name", remotePath, "error", err.Error())
		return DeleteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: "打开文件失败"}
	}

	// 基于 fd 的 Stat
	info, err := file.Stat()
	if err != nil {
		file.Close()
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "stat 失败")
		logger.ErrorContext(ctx, "stat 文件失败", "file_name", remotePath, "error", err.Error())
		return DeleteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: "stat 失败"}
	}
	_ = info

	// 基于 fd 的 checksum 校验
	cs, err := checksumReader(file)
	_, _ = file.Seek(0, io.SeekStart)
	if err != nil {
		file.Close()
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "计算 checksum 失败")
		logger.ErrorContext(ctx, "计算文件 checksum 失败", "file_name", remotePath, "error", err.Error())
		return DeleteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: "文件校验失败"}
	}
	if cs != expectedChecksum {
		file.Close()
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultDenied, "checksum 不匹配")
		logger.WarnContext(ctx, "文件校验失败", "file_name", remotePath)
		return DeleteFileResult{}, &HTTPError{Status: http.StatusBadRequest, Message: "文件校验失败"}
	}

	// 关闭后再删除
	file.Close()
	if err := root.Remove(rel); err != nil {
		// 审查 M-4：Detail 不含 err.Error()（os.Remove 错误含绝对路径，暴露服务端
		// 文件系统布局）；错误详情记业务日志，审计行用固定文案。
		logger.ErrorContext(ctx, "删除文件失败", "file_name", remotePath, "error", err.Error())
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "删除文件失败")
		return DeleteFileResult{}, &HTTPError{Status: http.StatusInternalServerError, Message: "删除文件失败"}
	}
	// P4 配额对账：删除即释放已确认占用（按删除前 stat 的文件大小）；按文件实际 rel
	// 解析子 Scope（与写入同一键，父链聚合释放到子目录/user/租户各层）。
	if scope := s.rt.quotaScope(owner, rel); scope != nil {
		scope.ReleaseUsage(info.Size())
	}
	// 卷容量池双 Release（AD-7）：写入经双账本预留/提交，删除须释放文件所在卷池，否则卷池
	// Usage 虚高（路由/换卷误判），依赖 reconcile 才自愈。homeVol 空（无卷语义旧装配）跳过。
	// 用 ReleaseCommitted 原子扣减（PR-C 终审 Minor：替代「读 Usage 两次 + Adjust」非原子序列）。
	if homeVol != "" && s.rt.volSet() != nil {
		if pool := s.rt.volSet().Pool(homeVol); pool != nil {
			pool.ReleaseCommitted(info.Size())
		}
	}
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.Delete(rel)
	}
	if s.rt.metricsRecorder() != nil {
		s.rt.metricsRecorder().RecordDelete()
	}
	s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultSuccess, "")
	logger.InfoContext(ctx, "文件已删除", "file_name", remotePath)
	return DeleteFileResult{RemotePath: remotePath, Message: fmt.Sprintf("文件删除成功: %s", remotePath)}, nil
}
