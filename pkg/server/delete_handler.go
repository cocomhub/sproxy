// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
)

// resolveAndValidateFile 校验文件名并返回请求者租户 user 桶下的相对路径（如 user/dir/f.txt）。
// 校验失败时返回 ("", "", false)。
func (h *Handlers) resolveAndValidateFile(r *http.Request, filename string) (remotePath, rel string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		return "", "", false
	}
	tnt := h.tenantOf(r)
	if tnt == nil {
		return "", "", false
	}
	rel, ok = tnt.UserRel(remotePath)
	if !ok {
		return "", "", false
	}
	return remotePath, rel, true
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	logger := h.logger

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgEmptyFilename}, http.StatusBadRequest)
		return
	}
	remotePath, rel, ok := h.resolveAndValidateFile(r, filename)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	expectedChecksum := r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		logger.WarnContext(r.Context(), "X-File-Checksum 为空", "file_name", remotePath)
		return
	}

	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	root := tnt.Root()

	// 基于 fd 操作缩小 TOCTOU 窗口：先打开文件，再基于 fd 执行 Stat 和 checksum 校验
	file, err := root.Open(rel)
	if err != nil {
		if os.IsNotExist(err) {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "delete", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "文件不存在",
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
			return
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "打开文件失败",
		})
		h.logger.ErrorContext(r.Context(), "打开文件失败", "file_name", remotePath, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "打开文件失败"}, http.StatusInternalServerError)
		return
	}

	// 基于 fd 的 Stat
	info, err := file.Stat()
	if err != nil {
		file.Close()
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "stat 失败",
		})
		h.logger.ErrorContext(r.Context(), "stat 文件失败", "file_name", remotePath, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "stat 失败"}, http.StatusInternalServerError)
		return
	}
	_ = info

	// 基于 fd 的 checksum 校验
	cs, err := Checksum(file)
	_, _ = file.Seek(0, io.SeekStart)
	if err != nil {
		file.Close()
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "计算 checksum 失败",
		})
		h.logger.ErrorContext(r.Context(), "计算文件 checksum 失败", "file_name", remotePath, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件校验失败"}, http.StatusInternalServerError)
		return
	}
	if cs != expectedChecksum {
		file.Close()
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "checksum 不匹配",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件校验失败"}, http.StatusBadRequest)
		logger.WarnContext(r.Context(), "文件校验失败", "file_name", remotePath)
		return
	}

	// 关闭后再删除
	file.Close()
	if err := root.Remove(rel); err != nil {
		// 审查 M-4：Detail 不含 err.Error()（os.Remove 错误含绝对路径，暴露服务端
		// 文件系统布局）；错误详情记业务日志，审计行用固定文案。
		h.logger.ErrorContext(r.Context(), "删除文件失败", "file_name", remotePath, "error", err.Error())
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "删除文件失败",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除文件失败"}, http.StatusInternalServerError)
		return
	}
	// P4 配额对账：删除即释放已确认占用（按删除前 stat 的文件大小）。
	if scope := h.quotaFor(ownerFromRequest(r)); scope != nil {
		scope.ReleaseUsage(info.Size())
	}
	if cs := h.checksumStoreFor(ownerFromRequest(r)); cs != nil {
		cs.Delete(rel)
	}
	if h.metrics != nil {
		h.metrics.RecordDelete()
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "delete", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess,
	})
	logger.InfoContext(r.Context(), "文件已删除", "file_name", remotePath)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件删除成功: %s", remotePath)}, http.StatusOK)
}

// processBatchDeleteItem 处理单条文件删除操作。
func (h *Handlers) processBatchDeleteItem(ctx context.Context, owner string, f BatchDeleteFile, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: f.Filename}
	remotePath, rel, ok := h.resolveAndValidateFileForOwner(owner, f.Filename)
	if !ok {
		result.Message = "无效的文件路径"
		return result
	}
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		result.Message = "无效的文件路径"
		return result
	}
	root := tnt.Root()
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
		h.RecordAudit(ctx, AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "checksum 不匹配",
		})
		result.Message = "文件校验失败"
		logger.WarnContext(ctx, "批量删除时 checksum 不匹配", "file_name", remotePath)
		return result
	}
	if err := root.Remove(rel); err != nil {
		// 审查 M-4：Detail 不含 err.Error()（绝对路径暴露）。
		logger.ErrorContext(ctx, "批量删除文件失败", "file_name", remotePath, "error", err.Error())
		h.RecordAudit(ctx, AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "删除文件失败",
		})
		result.Message = "删除失败"
	} else {
		// P4 配额对账：批量删除同样按删除前 stat 的文件大小释放占用。
		if scope := h.quotaFor(owner); scope != nil {
			scope.ReleaseUsage(stat.Size())
		}
		if cs := h.checksumStoreFor(owner); cs != nil {
			cs.Delete(rel)
		}
		h.RecordAudit(ctx, AuditEvent{
			Action: "delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultSuccess,
		})
		result.Success = true
		result.Message = "删除成功"
	}
	return result
}

// batchDelete 处理 POST /api/batch/delete。
// 请求体 JSON：{"files": [{"file_name": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余文件。
func (h *Handlers) batchDelete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := h.logger.With("batch", "delete")
	owner := ownerFromRequest(r)
	results := make([]BatchOperationResult, 0, len(req.Files))
	for _, f := range req.Files {
		results = append(results, h.processBatchDeleteItem(r.Context(), owner, f, logger))
	}
	sendJSONResponse(w, BatchResponse{Results: results}, http.StatusOK)
}
