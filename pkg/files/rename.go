// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// rename.go 是文件服务**写面**的重命名族（`POST /rename` 与 `POST /api/batch/rename`
// 及其私有辅助、批量 DTO），自 `pkg/server/rename_handler.go` 整族迁入（接收者由
// *Handlers 改为 *Service，方法体只换接收者与限定名）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// errMsgSrcChecksumFailed / errMsgCreateParentDirFailed 是重命名族的失败响应文案（随本族
// 自 pkg/server/errors.go 迁入；两个常量在 pkg/server 均已无其他使用者）。
const (
	errMsgSrcChecksumFailed     = "源文件 SHA-256 校验失败"
	errMsgCreateParentDirFailed = "目标路径父目录创建失败"
)

// BatchRenameRequest 批量重命名请求体。
type BatchRenameRequest struct {
	Operations []BatchRenameOp `json:"operations"`
}

// BatchRenameOp 单条重命名操作。
type BatchRenameOp struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Checksum string `json:"checksum"`
}

// parseRenameParams 从请求中提取重命名参数：from、to 和 X-File-Checksum。
func parseRenameParams(r *http.Request) (from, to, checksum string, err error) {
	from = r.URL.Query().Get("from")
	to = r.URL.Query().Get("to")
	if from == "" || to == "" {
		return "", "", "", fmt.Errorf("from 和 to 都不能为空")
	}
	from, err = pathguard.ValidateFilePath(from)
	if err != nil {
		return "", "", "", fmt.Errorf("无效的源路径")
	}
	to, err = pathguard.ValidateFilePath(to)
	if err != nil {
		return "", "", "", fmt.Errorf("无效的目标路径")
	}
	// 写入侧守卫已收敛：UserRel 保证 user/ 桶内（首段 .__/__ 内部前缀时拒绝），
	// 无需再单独校验内部目录守卫。
	checksum = r.Header.Get(headerFileChecksum)
	return from, to, checksum, nil
}

// resolveRenamePaths 计算 from 和 to 在指定 owner 租户 user 桶下的相对路径。
// from 与 to 必须落在同一租户内（UserRel 保证 user/ 桶内）。返回租户与两条 rel。
func (s *Service) resolveRenamePaths(owner, from, to string) (fromRel, toRel string, tnt *storage.Tenant, ok bool) {
	tnt = s.deps.TenantFor(owner)
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

// renameOpCtx 是 executeRename 的参数集合，用于减少函数参数数量（go:S107）。
type renameOpCtx struct {
	s                *Service
	w                http.ResponseWriter
	ctx              context.Context
	owner            string
	root             *storage.Root
	fromRel          string
	toRel            string
	from             string
	to               string
	expectedChecksum string
	logger           *slog.Logger
}

// executeRename 校验 checksum、执行 Rename、更新 checksumStore。
// 返回 nil 表示成功；返回 error 表示失败（已在内部发送响应）。
func executeRename(ctx renameOpCtx) error {
	ctx.logger.InfoContext(ctx.ctx, "开始重命名", "from", ctx.fromRel, "to", ctx.toRel)
	if _, err := ctx.root.Stat(ctx.fromRel); os.IsNotExist(err) {
		ctx.s.deps.RecordFileAudit(ctx.ctx, "rename", ctx.from, auditResultError, "源文件不存在")
		ctx.s.sendJSON(ctx.w, UploadResponse{Success: false, Message: "源文件不存在"}, http.StatusNotFound)
		return err
	}
	// TODO: 此处存在 TOCTOU 竞态窗口（Stat 与 Rename 之间），后续优化为原子操作
	if _, err := ctx.root.Stat(ctx.toRel); err == nil {
		ctx.s.deps.RecordFileAudit(ctx.ctx, "rename", ctx.from, auditResultDenied, "目标路径已存在: "+ctx.to)
		ctx.s.sendJSON(ctx.w, UploadResponse{Success: false, Message: "目标路径已存在"}, http.StatusConflict)
		// 审查 I-1：必须返回非 nil 错误——原 `return err`（err 恰为 nil）让调用方误判
		// 成功并追加一条假的 success 审计行（被拒绝的 rename 记为成功，破坏审计可信度）。
		return errors.New("rename: 目标路径已存在")
	}
	if !verifyFileWithChecksumRoot(ctx.root, ctx.fromRel, ctx.expectedChecksum) {
		ctx.s.deps.RecordFileAudit(ctx.ctx, "rename", ctx.from, auditResultDenied, "checksum 不匹配")
		ctx.logger.WarnContext(ctx.ctx, "rename checksum 校验失败", "from", ctx.from)
		ctx.s.sendJSON(ctx.w, UploadResponse{Success: false, Message: errMsgSrcChecksumFailed}, http.StatusBadRequest)
		return fmt.Errorf("checksum mismatch")
	}
	if err := ctx.root.MkdirAll(filepath.Dir(ctx.toRel), 0755); err != nil {
		ctx.s.deps.RecordFileAudit(ctx.ctx, "rename", ctx.from, auditResultError, "创建父目录失败: "+ctx.to)
		ctx.logger.ErrorContext(ctx.ctx, errMsgCreateParentDirFailed, "to", ctx.to, "error", err.Error())
		ctx.s.sendJSON(ctx.w, UploadResponse{Success: false, Message: errMsgCreateParentDirFailed}, http.StatusInternalServerError)
		return err
	}
	// 配额：rename 在 user 桶内移动字节（总量不变，桶/租户级天然正确）。但跨 bucket_limits
	// 子目录时 committed 归属需对称转移——源目录键释放、目标目录键入账（子目录配额对 rename
	// 同样封顶，防止"先传受限目录外再 rename 进来"绕过）。非跨键（同目录/同键）零操作。
	// 两键相同时无需记账（释放+入账互相抵消）；装配层配额未装配（QuotaScopeFor 返回 nil）
	// 时退化为无配额记账（旧行为，仅总量正确）。
	if fromScope := ctx.s.deps.QuotaScopeFor(ctx.owner, ctx.fromRel); fromScope != nil {
		if toScope := ctx.s.deps.QuotaScopeFor(ctx.owner, ctx.toRel); toScope != nil && fromScope != toScope {
			if srcInfo, statErr := ctx.root.Stat(ctx.fromRel); statErr == nil {
				size := srcInfo.Size()
				// 目标键先 TryReserve（子目录/租户/全局逐级检查，配额不足拒绝移动避免超限），
				// 成功后再原子 Rename，最后源键 ReleaseUsage。若 Rename 失败则 Release 归还目标预留。
				toRes, err := toScope.TryReserve(size)
				if err != nil {
					ctx.s.deps.RecordFileAudit(ctx.ctx, "rename", ctx.from, auditResultDenied, "目标目录配额不足: to="+ctx.to)
					ctx.logger.WarnContext(ctx.ctx, "rename 目标目录配额不足", "from", ctx.fromRel, "to", ctx.toRel, "size", size)
					ctx.s.sendJSON(ctx.w, UploadResponse{Success: false, Message: "目标目录配额不足"}, http.StatusInsufficientStorage)
					return errors.New("rename: 目标目录配额不足")
				}
				if err := atomicRenameRoot(ctx.root, ctx.fromRel, ctx.toRel); err != nil {
					toRes.Release() // Rename 失败归还目标预留（源键未动）。
					ctx.s.deps.RecordFileAudit(ctx.ctx, "rename", ctx.from, auditResultError, "重命名失败: "+ctx.to)
					ctx.logger.ErrorContext(ctx.ctx, "重命名失败", "from", ctx.from, "to", ctx.to, "error", err.Error())
					ctx.s.sendJSON(ctx.w, UploadResponse{Success: false, Message: "重命名失败"}, http.StatusInternalServerError)
					return err
				}
				toRes.Commit(size) // 目标键入账。
				fromScope.ReleaseUsage(size)
				if cs := ctx.s.deps.ChecksumStoreFor(ctx.owner); cs != nil {
					cs.Rename(ctx.fromRel, ctx.toRel)
				}
				return nil
			}
			// 源文件 stat 失败（理论不可达：上方已 Stat 校验存在）：不记账，继续原链路。
		}
	}
	if err := atomicRenameRoot(ctx.root, ctx.fromRel, ctx.toRel); err != nil {
		ctx.s.deps.RecordFileAudit(ctx.ctx, "rename", ctx.from, auditResultError, "重命名失败: "+ctx.to)
		ctx.logger.ErrorContext(ctx.ctx, "重命名失败", "from", ctx.from, "to", ctx.to, "error", err.Error())
		ctx.s.sendJSON(ctx.w, UploadResponse{Success: false, Message: "重命名失败"}, http.StatusInternalServerError)
		return err
	}
	if cs := ctx.s.deps.ChecksumStoreFor(ctx.owner); cs != nil {
		cs.Rename(ctx.fromRel, ctx.toRel)
	}
	return nil
}

// processBatchRenameItem 处理单条批量重命名操作。
//
// 多卷（T6b）：源文件按 owner 卷视图跨卷定位（LocateOwnerFile）——非默认卷文件可批量改名
// （同卷内移动），不再恒默认卷 404。默认卷被 ACL 排除时源在默认卷的遗留按不可见处理
// （源文件不存在，fail-closed 不泄存在性）。目标跨卷已存在（AD-4）→ 409 语义（目标路径已存在）。
func (s *Service) processBatchRenameItem(ctx context.Context, owner string, op BatchRenameOp, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: op.From + " -> " + op.To}
	from, err := pathguard.ValidateFilePath(op.From)
	if err != nil {
		result.Message = "无效的源路径"
		return result
	}
	to, err := pathguard.ValidateFilePath(op.To)
	if err != nil {
		result.Message = "无效的目标路径"
		return result
	}
	// 写入侧守卫已收敛：UserRel 保证 user/ 桶内（首段 .__/__ 内部前缀时拒绝）。
	if from == to {
		result.Success = true
		result.Message = "源与目标相同，无需移动"
		return result
	}
	owner = normalizeOwner(owner)
	fromRel, toRel, tnt, ok := s.resolveRenamePaths(owner, from, to)
	if !ok {
		result.Message = "无效的文件路径"
		return result
	}

	// 跨卷定位源（home 卷内完成 rename；默认卷被 ACL 排除时不得回落默认租户）。
	loc, found := s.deps.LocateOwnerFile(owner, fromRel)
	var root *storage.Root
	var homeVol string
	switch {
	case found && loc.Tenant != nil && loc.Tenant.Root() != nil:
		homeVol = loc.VolumeName
		root = loc.Tenant.Root()
	case s.deps.VolSet != nil && !s.defaultVolumeAllows(owner):
		result.Message = "源文件不存在"
		return result
	default:
		// 旧装配 / 单卷默认开放：回落默认租户由 Stat 产出「源文件不存在」（与单卷一致）。
		if tnt == nil || tnt.Root() == nil {
			result.Message = "无效的文件路径"
			return result
		}
		root = tnt.Root()
	}

	if _, err := root.Stat(fromRel); os.IsNotExist(err) {
		result.Message = "源文件不存在"
		return result
	}
	if _, err := root.Stat(toRel); err == nil {
		result.Message = "目标路径已存在"
		return result
	}
	// AD-4 唯一性：目标 rel 不得已在 owner 视图其它卷存在（同 rel 跨卷双份）。单卷 homeVol 空跳过。
	if homeVol != "" && s.deps.VolSet != nil {
		if dstLoc, dstFound := s.deps.LocateOwnerFile(owner, toRel); dstFound && dstLoc.VolumeName != homeVol {
			result.Message = "目标路径已存在"
			return result
		}
	}
	if op.Checksum == "" {
		result.Message = "缺少 checksum"
		return result
	}
	if !verifyFileWithChecksumRoot(root, fromRel, op.Checksum) {
		logger.WarnContext(ctx, "batch rename checksum 不匹配", "from", op.From)
		// 审查 I-4：batch rename 失败路径与单条 rename 对齐，补审计（checksum 拒绝 → denied）。
		s.deps.RecordFileAudit(ctx, "rename", op.From, auditResultDenied, "checksum 不匹配（batch）: to="+op.To)
		result.Message = errMsgSrcChecksumFailed
		return result
	}
	if err := root.MkdirAll(filepath.Dir(toRel), 0755); err != nil {
		logger.ErrorContext(ctx, errMsgCreateParentDirFailed, "to", to, "error", err.Error())
		s.deps.RecordFileAudit(ctx, "rename", op.From, auditResultError, "创建父目录失败: to="+op.To)
		result.Message = "创建父目录失败"
		return result
	}
	// 配额：跨 bucket_limits 子目录移动时对称转移（与 executeRename 同语义）。目标键配额
	// 不足 → 单条拒绝（批量继续处理其余）。装配层配额未装配（scope nil）时退化为无记账。
	if fromScope := s.deps.QuotaScopeFor(owner, fromRel); fromScope != nil {
		if toScope := s.deps.QuotaScopeFor(owner, toRel); toScope != nil && fromScope != toScope {
			srcInfo, statErr := root.Stat(fromRel)
			if statErr == nil {
				size := srcInfo.Size()
				toRes, err := toScope.TryReserve(size)
				if err != nil {
					s.deps.RecordFileAudit(ctx, "rename", op.From, auditResultDenied, "目标目录配额不足（batch）: to="+op.To)
					logger.WarnContext(ctx, "batch rename 目标目录配额不足", "from", fromRel, "to", toRel, "size", size)
					result.Message = "目标目录配额不足"
					return result
				}
				if err := atomicRenameRoot(root, fromRel, toRel); err != nil {
					toRes.Release()
					logger.ErrorContext(ctx, "batch rename 失败", "from", op.From, "to", op.To, "error", err.Error())
					s.deps.RecordFileAudit(ctx, "rename", op.From, auditResultError, "重命名失败: to="+op.To)
					result.Message = "重命名失败"
					return result
				}
				toRes.Commit(size)
				fromScope.ReleaseUsage(size)
				if cs := s.deps.ChecksumStoreFor(owner); cs != nil {
					cs.Rename(fromRel, toRel)
				}
				s.deps.RecordFileAudit(ctx, "rename", from, auditResultSuccess, "to="+to)
				logger.InfoContext(ctx, "文件已重命名", "from", op.From, "to", op.To)
				return BatchOperationResult{
					Filename: op.From + " -> " + op.To,
					Success:  true,
					Message:  "重命名成功",
				}
			}
			// stat 失败（理论不可达）：按无配额路径继续，不误报。
		}
	}
	if err := atomicRenameRoot(root, fromRel, toRel); err != nil {
		logger.ErrorContext(ctx, "batch rename 失败", "from", op.From, "to", op.To, "error", err.Error())
		s.deps.RecordFileAudit(ctx, "rename", op.From, auditResultError, "重命名失败: to="+op.To)
		result.Message = "重命名失败"
		return result
	}
	if cs := s.deps.ChecksumStoreFor(owner); cs != nil {
		cs.Rename(fromRel, toRel)
	}
	s.deps.RecordFileAudit(ctx, "rename", from, auditResultSuccess, "to="+to)
	logger.InfoContext(ctx, "文件已重命名", "from", op.From, "to", op.To)
	return BatchOperationResult{
		Filename: op.From + " -> " + op.To,
		Success:  true,
		Message:  "重命名成功",
	}
}

// BatchRename 处理 POST /api/batch/rename。
// 请求体 JSON：{"operations": [{"from": "...", "to": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余操作。
func (s *Service) BatchRename(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if len(req.Operations) == 0 {
		s.sendJSON(w, UploadResponse{Success: false, Message: "operations 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := s.deps.Logger().With("batch", "rename")
	owner := s.deps.ActorFromRequest(r)
	results := make([]BatchOperationResult, 0, len(req.Operations))
	for _, op := range req.Operations {
		result := s.processBatchRenameItem(r.Context(), owner, op, logger)
		results = append(results, result)
	}
	s.sendJSON(w, BatchResponse{Results: results}, http.StatusOK)
}

// Rename 处理 POST /rename?from=<old>&to=<new>。
// 与 delete 对称，要求 X-File-Checksum 头校验源文件，避免误覆盖。
// 目标路径已存在时返回 409；服务端会自动 mkdir -p 中间目录。
func (s *Service) Rename(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger()

	from, to, expectedChecksum, err := parseRenameParams(r)
	if err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return
	}

	if from == to {
		s.sendJSON(w, UploadResponse{Success: true, Message: "源与目标相同，无需移动"}, http.StatusOK)
		return
	}

	if expectedChecksum == "" {
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		return
	}

	owner := normalizeOwner(s.deps.ActorFromRequest(r))
	fromRel, toRel, tnt, ok := s.resolveRenamePaths(owner, from, to)
	if !ok || tnt == nil || tnt.Root() == nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// 跨卷定位源 home（任务 5）：文件可能因换卷落在非默认卷，rename 在 home 卷内完成
	// （同卷；跨卷移动走 T6 move API）。带显式 ?volume= 只在指定卷定位源（不在 → 404）。
	// 全视图未命中仅当默认卷对 owner 授权才回落默认租户（由 executeRename 的 Stat 产出 404，
	// 与单卷一致）；默认卷被 ACL 排除时不得回落——否则 owner 可 rename 默认卷自身路径的
	// 遗留文件（ACL bypass，AD-6）。
	explicitVol := r.URL.Query().Get("volume")
	loc, found := s.locateForRead(owner, fromRel, explicitVol)
	var homeVol string
	var root *storage.Root
	switch {
	case found && loc.Tenant != nil:
		homeVol = loc.VolumeName
		root = loc.Tenant.Root()
	case explicitVol != "":
		s.sendJSON(w, UploadResponse{Success: false, Message: "源文件不存在"}, http.StatusNotFound)
		return
	case !s.defaultVolumeAllows(owner):
		s.sendJSON(w, UploadResponse{Success: false, Message: "源文件不存在"}, http.StatusNotFound)
		return
	default:
		root = tnt.Root()
	}

	// AD-4 唯一性：目标 rel 不得已存在于 owner 视图其它卷（否则 rename 后同逻辑路径跨卷
	// 双份）。目标已在同一 home 卷由 executeRename 的 Stat 捕获（409）；目标在其它卷 →
	// 直接 409（跨卷移动非本任务语义）。单卷/无卷语义时 homeVol 空 → 跳过。
	if homeVol != "" && s.deps.VolSet != nil {
		if dstLoc, dstFound := s.deps.LocateOwnerFile(owner, toRel); dstFound && dstLoc.VolumeName != homeVol {
			s.sendJSON(w, UploadResponse{Success: false, Message: "目标路径已存在"}, http.StatusConflict)
			return
		}
	}

	if err := executeRename(renameOpCtx{
		s:                s,
		w:                w,
		ctx:              r.Context(),
		owner:            owner,
		root:             root,
		fromRel:          fromRel,
		toRel:            toRel,
		from:             from,
		to:               to,
		expectedChecksum: expectedChecksum,
		logger:           logger,
	}); err != nil {
		return
	}

	s.deps.RecordFileAudit(r.Context(), "rename", from, auditResultSuccess, "to="+to)
	logger.InfoContext(r.Context(), "文件已重命名", "from", from, "to", to, "checksum", expectedChecksum)
	s.sendJSON(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件已重命名: %s -> %s", from, to),
		Checksum: expectedChecksum,
	}, http.StatusOK)
}
