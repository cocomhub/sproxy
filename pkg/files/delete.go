// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// delete.go 是删除族的 **HTTP 面**：`POST /delete`（薄适配）与 `POST /api/batch/delete`
// 的 DTO 与逐条处理，自 `pkg/server/delete_handler.go` 整族迁入（接收者由 *Handlers 改为
// *Service）。
//
// 分工（D-2 第 4 片）：**领域逻辑在 write_ops.go 的 `DeleteFile`**（文件级互斥、跨卷定位、
// checksum 门禁、配额/卷池/台账释放、计量与审计），本文件只保留「解析请求 → 调域方法 →
// 写响应」与批量族的逐条结果聚合。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// errMsgEmptyFilename 是 filename 查询参数缺失时的响应文案（随本族自 pkg/server/errors.go
// 迁入）：与 pkg/server.errors.go 同值——该常量在装配层另有 2 个消费者（下载路径解析），
// 故两侧各留一份（等价副本，只影响文案字面量，与其余 errMsg* 副本同一约定）。
const errMsgEmptyFilename = "文件名不能为空"

// BatchDeleteRequest 批量删除请求体。
type BatchDeleteRequest struct {
	Files []BatchDeleteFile `json:"files"`
}

// BatchDeleteFile 批量删除中的单条文件。
type BatchDeleteFile struct {
	Filename string `json:"filename"`
	Checksum string `json:"checksum"`
}

// resolveAndValidateFileForOwner 校验文件名并返回指定 owner 租户 user 桶下的相对路径
// （如 user/dir/f.txt）。供批量操作（ctx 无 *http.Request）使用；校验失败返回 ("", "", false)。
func (s *Service) resolveAndValidateFileForOwner(owner, filename string) (remotePath, rel string, ok bool) {
	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		return "", "", false
	}
	tnt := s.rt.tenantOf(owner)
	if tnt == nil {
		return "", "", false
	}
	rel, ok = tnt.UserRel(remotePath)
	if !ok {
		return "", "", false
	}
	return remotePath, rel, true
}

// Delete 处理 POST /delete?filename=<name>[&volume=<v>]。
// 要求 X-File-Checksum 头与文件实际 checksum 匹配才删除（防误删）。
//
// 本处理器只做「解析入参 → 调域方法 → 写响应」；全部领域逻辑（文件级互斥、跨卷定位、
// checksum 门禁、配额/卷池/台账释放、审计）在 write_ops.go 的 DeleteFile 内。
func (s *Service) Delete(w http.ResponseWriter, r *http.Request) {
	res, err := s.DeleteFile(r.Context(), DeleteFileInput{
		Owner:            s.rt.actorOf(r),
		RemotePath:       r.URL.Query().Get("filename"),
		ExpectedChecksum: r.Header.Get(headerFileChecksum),
		ExplicitVol:      r.URL.Query().Get("volume"),
	})
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: "删除文件失败"}, http.StatusInternalServerError)
		return
	}
	s.sendJSON(w, UploadResponse{Success: true, Message: res.Message}, http.StatusOK)
}

// processBatchDeleteItem 处理单条文件删除操作。
//
// 多卷（T6b）：逐文件按 owner 卷视图跨卷定位（LocateOwnerFile）——非默认卷文件可批量删除，
// 不再恒默认卷 404/静默成功。语义：真实缺失（视图全无）才幂等成功提示；默认卷被 ACL 排除时
// 默认卷遗留不可见按缺失处理（fail-closed，不泄存在性，绝不经默认租户直删遗留）。
func (s *Service) processBatchDeleteItem(ctx context.Context, owner string, f BatchDeleteFile, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: f.Filename}
	remotePath, rel, ok := s.resolveAndValidateFileForOwner(owner, f.Filename)
	if !ok {
		result.Message = "无效的文件路径"
		return result
	}
	owner = normalizeOwner(owner)

	// 跨卷定位：定位 rel 实际所在卷（视图内）。homeVol 供删除后卷容量池 Release（与单删一致）。
	loc, found := s.rt.locateOwnerFile(owner, rel)
	var root *storage.Root
	var homeVol string
	switch {
	case found && loc.Tenant != nil && loc.Tenant.Root() != nil:
		homeVol = loc.VolumeName
		root = loc.Tenant.Root()
	case s.rt.volSet() != nil && !s.defaultVolumeAllows(owner):
		// 默认卷被 ACL 排除：视图外遗留不可见 → 幂等成功（不泄存在性，不直删默认卷）。
		result.Success = true
		result.Message = "文件不存在（幂等删除）"
		logger.WarnContext(ctx, "批量删除：文件不在视图（幂等删除）", "file_name", remotePath)
		return result
	default:
		// 旧装配 / 单卷默认开放：LocateOwnerFile 已覆盖默认卷，miss 即真实缺失（Stat 兜底幂等）。
		tnt := s.rt.tenantOf(owner)
		if tnt == nil || tnt.Root() == nil {
			result.Message = "无效的文件路径"
			return result
		}
		root = tnt.Root()
	}

	stat, statErr := root.Stat(rel)
	if os.IsNotExist(statErr) {
		result.Success = true
		result.Message = "文件不存在（幂等删除）"
		logger.WarnContext(ctx, "批量删除：文件不存在（幂等删除）", "file_name", remotePath)
		return result
	}
	if f.Checksum == "" {
		result.Message = "缺少 checksum"
		return result
	}
	// 校验 checksum，不匹配时拒绝删除
	if !verifyFileWithChecksumRoot(root, rel, f.Checksum) {
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultDenied, "checksum 不匹配")
		result.Message = "文件校验失败"
		logger.WarnContext(ctx, "批量删除时 checksum 不匹配", "file_name", remotePath)
		return result
	}
	if err := root.Remove(rel); err != nil {
		// 审查 M-4：Detail 不含 err.Error()（绝对路径暴露）。
		logger.ErrorContext(ctx, "批量删除文件失败", "file_name", remotePath, "error", err.Error())
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultError, "删除文件失败")
		result.Message = "删除失败"
	} else {
		// P4 配额对账：批量删除同样按删除前 stat 的文件大小释放占用（按 rel 解析子 Scope）。
		if scope := s.rt.quotaScope(owner, rel); scope != nil {
			scope.ReleaseUsage(stat.Size())
		}
		// 卷容量池双 Release（AD-7）：删除释放文件所在卷池，否则 Usage 虚高（与单删一致）。
		if homeVol != "" && s.rt.volSet() != nil {
			if pool := s.rt.volSet().Pool(homeVol); pool != nil {
				pool.ReleaseCommitted(stat.Size())
			}
		}
		if cs := s.rt.checksumStore(owner); cs != nil {
			cs.Delete(rel)
		}
		s.rt.recordFileAudit(ctx, "delete", remotePath, auditResultSuccess, "")
		result.Success = true
		result.Message = "删除成功"
	}
	return result
}

// BatchDelete 处理 POST /api/batch/delete。
// 请求体 JSON：{"files": [{"file_name": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余文件。
func (s *Service) BatchDelete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		s.sendJSON(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := s.rt.logger().With("batch", "delete")
	owner := s.rt.actorOf(r)
	results := make([]BatchOperationResult, 0, len(req.Files))
	for _, f := range req.Files {
		results = append(results, s.processBatchDeleteItem(r.Context(), owner, f, logger))
	}
	s.sendJSON(w, BatchResponse{Results: results}, http.StatusOK)
}
