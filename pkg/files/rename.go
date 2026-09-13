// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// rename.go 是重命名族的 **HTTP 面**：`POST /rename`（薄适配）与 `POST /api/batch/rename`
// 的 DTO 与逐条处理，自 `pkg/server/rename_handler.go` 整族迁入（接收者由 *Handlers 改为
// *Service）。
//
// 分工（D-2 第 4 片）：**领域逻辑在 write_ops.go 的 `RenameFile`**（跨卷定位、AD-4 唯一性、
// checksum 门禁、配额对称转移、审计），本文件只保留「解析请求 → 调域方法 → 写响应」与
// 批量族的逐条结果聚合。

import (
	"context"
	"encoding/json"
	"errors"
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
	loc, found := s.rt.locateOwnerFile(owner, fromRel)
	var root *storage.Root
	var homeVol string
	switch {
	case found && loc.Tenant != nil && loc.Tenant.Root() != nil:
		homeVol = loc.VolumeName
		root = loc.Tenant.Root()
	case s.rt.volSet() != nil && !s.defaultVolumeAllows(owner):
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
	if homeVol != "" && s.rt.volSet() != nil {
		if dstLoc, dstFound := s.rt.locateOwnerFile(owner, toRel); dstFound && dstLoc.VolumeName != homeVol {
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
		s.rt.recordFileAudit(ctx, "rename", op.From, auditResultDenied, "checksum 不匹配（batch）: to="+op.To)
		result.Message = errMsgSrcChecksumFailed
		return result
	}
	if err := root.MkdirAll(filepath.Dir(toRel), 0755); err != nil {
		logger.ErrorContext(ctx, errMsgCreateParentDirFailed, "to", to, "error", err.Error())
		s.rt.recordFileAudit(ctx, "rename", op.From, auditResultError, "创建父目录失败: to="+op.To)
		result.Message = "创建父目录失败"
		return result
	}
	// 配额：跨 bucket_limits 子目录移动时对称转移（与 executeRename 同语义）。目标键配额
	// 不足 → 单条拒绝（批量继续处理其余）。装配层配额未装配（scope nil）时退化为无记账。
	if fromScope := s.rt.quotaScope(owner, fromRel); fromScope != nil {
		if toScope := s.rt.quotaScope(owner, toRel); toScope != nil && fromScope != toScope {
			srcInfo, statErr := root.Stat(fromRel)
			if statErr == nil {
				size := srcInfo.Size()
				toRes, err := toScope.TryReserve(size)
				if err != nil {
					s.rt.recordFileAudit(ctx, "rename", op.From, auditResultDenied, "目标目录配额不足（batch）: to="+op.To)
					logger.WarnContext(ctx, "batch rename 目标目录配额不足", "from", fromRel, "to", toRel, "size", size)
					result.Message = "目标目录配额不足"
					return result
				}
				if err := atomicRenameRoot(root, fromRel, toRel); err != nil {
					toRes.Release()
					logger.ErrorContext(ctx, "batch rename 失败", "from", op.From, "to", op.To, "error", err.Error())
					s.rt.recordFileAudit(ctx, "rename", op.From, auditResultError, "重命名失败: to="+op.To)
					result.Message = "重命名失败"
					return result
				}
				toRes.Commit(size)
				fromScope.ReleaseUsage(size)
				if cs := s.rt.checksumStore(owner); cs != nil {
					cs.Rename(fromRel, toRel)
				}
				s.rt.recordFileAudit(ctx, "rename", from, auditResultSuccess, "to="+to)
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
		s.rt.recordFileAudit(ctx, "rename", op.From, auditResultError, "重命名失败: to="+op.To)
		result.Message = "重命名失败"
		return result
	}
	if cs := s.rt.checksumStore(owner); cs != nil {
		cs.Rename(fromRel, toRel)
	}
	s.rt.recordFileAudit(ctx, "rename", from, auditResultSuccess, "to="+to)
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
	logger := s.rt.logger().With("batch", "rename")
	owner := s.rt.actorOf(r)
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
//
// 本处理器只做「解析三个入参（from/to/checksum + 可选 volume）→ 调域方法 → 写响应」；
// 全部领域逻辑（跨卷定位、AD-4 唯一性、checksum 门禁、配额转移、审计）在 write_ops.go
// 的 RenameFile 内（远程写面复用同一份语义，见 write_ops.go 顶注）。
func (s *Service) Rename(w http.ResponseWriter, r *http.Request) {
	res, err := s.RenameFile(r.Context(), RenameFileInput{
		Owner:            s.rt.actorOf(r),
		From:             r.URL.Query().Get("from"),
		To:               r.URL.Query().Get("to"),
		ExpectedChecksum: r.Header.Get(headerFileChecksum),
		ExplicitVol:      r.URL.Query().Get("volume"),
	})
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: "重命名失败"}, http.StatusInternalServerError)
		return
	}
	s.sendJSON(w, UploadResponse{Success: true, Message: res.Message, Checksum: res.Checksum}, http.StatusOK)
}
