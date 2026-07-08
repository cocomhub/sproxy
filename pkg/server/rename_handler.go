// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// parseRenameParams 从请求中提取重命名参数：from、to 和 X-File-Checksum。
func parseRenameParams(r *http.Request) (from, to, checksum string, err error) {
	from = r.URL.Query().Get("from")
	to = r.URL.Query().Get("to")
	if from == "" || to == "" {
		return "", "", "", fmt.Errorf("from 和 to 都不能为空")
	}
	from, err = ValidateFilePath(from)
	if err != nil {
		return "", "", "", fmt.Errorf("无效的源路径")
	}
	to, err = ValidateFilePath(to)
	if err != nil {
		return "", "", "", fmt.Errorf("无效的目标路径")
	}
	checksum = r.Header.Get(headerFileChecksum)
	return from, to, checksum, nil
}

// resolveRenamePaths 计算 from 和 to 对应的安全绝对路径。
func resolveRenamePaths(h *Handlers, w http.ResponseWriter, from, to string) (fromPath, toPath string, ok bool) {
	fromPath = h.safePath(from)
	toPath = h.safePath(to)
	if fromPath == "" || toPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return fromPath, toPath, true
}

// renameOpCtx 是 executeRename 的参数集合，用于减少函数参数数量（go:S107）。
type renameOpCtx struct {
	h                *Handlers
	w                http.ResponseWriter
	fromPath         string
	toPath           string
	from             string
	to               string
	expectedChecksum string
	logger           *slog.Logger
}

// executeRename 校验 checksum、执行 Rename、更新 checksumStore。
// 返回 nil 表示成功；返回 error 表示失败（已在内部发送响应）。
func executeRename(ctx renameOpCtx) error {
	if _, err := os.Stat(ctx.fromPath); os.IsNotExist(err) {
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: "源文件不存在"}, http.StatusNotFound)
		return err
	}
	if _, err := os.Stat(ctx.toPath); err == nil {
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: "目标路径已存在"}, http.StatusConflict)
		return err
	}
	if !verifyFileWithChecksum(ctx.fromPath, ctx.expectedChecksum) {
		ctx.logger.Warn("rename checksum 校验失败", "from", ctx.from)
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: errMsgSrcChecksumFailed}, http.StatusBadRequest)
		return fmt.Errorf("checksum mismatch")
	}
	if err := os.MkdirAll(filepath.Dir(ctx.toPath), 0755); err != nil {
		ctx.logger.Error(errMsgCreateParentDirFailed, "to", ctx.to, "error", err.Error())
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: errMsgCreateParentDirFailed}, http.StatusInternalServerError)
		return err
	}
	if err := atomicRename(ctx.fromPath, ctx.toPath); err != nil {
		ctx.logger.Error("重命名失败", "from", ctx.from, "to", ctx.to, "error", err.Error())
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: "重命名失败"}, http.StatusInternalServerError)
		return err
	}
	ctx.h.checksumStore.Rename(ctx.from, ctx.to)
	return nil
}

// processBatchRenameItem 处理单条批量重命名操作。
func (h *Handlers) processBatchRenameItem(op BatchRenameOp, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: op.From + " -> " + op.To}
	from, err := ValidateFilePath(op.From)
	if err != nil {
		result.Message = "无效的源路径"
		return result
	}
	to, err := ValidateFilePath(op.To)
	if err != nil {
		result.Message = "无效的目标路径"
		return result
	}
	if from == to {
		result.Success = true
		result.Message = "源与目标相同，无需移动"
		return result
	}
	fromPath := h.safePath(from)
	toPath := h.safePath(to)
	if fromPath == "" || toPath == "" {
		result.Message = "无效的文件路径"
		return result
	}
	if _, err := os.Stat(fromPath); os.IsNotExist(err) {
		result.Message = "源文件不存在"
		return result
	}
	if _, err := os.Stat(toPath); err == nil {
		result.Message = "目标路径已存在"
		return result
	}
	if op.Checksum == "" {
		result.Message = "缺少 checksum"
		return result
	}
	if !verifyFileWithChecksum(fromPath, op.Checksum) {
		logger.Warn("batch rename checksum 不匹配", "from", op.From)
		result.Message = errMsgSrcChecksumFailed
		return result
	}
	if err := os.MkdirAll(filepath.Dir(toPath), 0755); err != nil {
		logger.Error(errMsgCreateParentDirFailed, "to", to, "error", err.Error())
		result.Message = "创建父目录失败"
		return result
	}
	if err := atomicRename(fromPath, toPath); err != nil {
		logger.Error("batch rename 失败", "from", op.From, "to", op.To, "error", err.Error())
		result.Message = "重命名失败"
		return result
	}
	h.checksumStore.Rename(from, to)
	return BatchOperationResult{
		Filename: op.From + " -> " + op.To,
		Success:  true,
		Message:  "重命名成功",
	}
}

// batchRename 处理 POST /api/batch/rename。
// 请求体 JSON：{"operations": [{"from": "...", "to": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余操作。
func (h *Handlers) batchRename(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	if len(req.Operations) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "operations 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := h.logger.With("batch", "rename")
	results := make([]BatchOperationResult, 0, len(req.Operations))
	for _, op := range req.Operations {
		result := h.processBatchRenameItem(op, logger)
		results = append(results, result)
	}
	sendJSONResponse(w, BatchRenameResponse{Results: results}, http.StatusOK)
}

// rename 处理 POST /rename?from=<old>&to=<new>。
// 与 delete 对称，要求 X-File-Checksum 头校验源文件，避免误覆盖。
// 目标路径已存在时返回 409；服务端会自动 mkdir -p 中间目录。
func (h *Handlers) rename(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(headerRequestID)
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	from, to, expectedChecksum, err := parseRenameParams(r)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return
	}

	if from == to {
		sendJSONResponse(w, UploadResponse{Success: true, Message: "源与目标相同，无需移动"}, http.StatusOK)
		return
	}

	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		return
	}

	fromPath, toPath, ok := resolveRenamePaths(h, w, from, to)
	if !ok {
		return
	}

	if err := executeRename(renameOpCtx{
		h:                h,
		w:                w,
		fromPath:         fromPath,
		toPath:           toPath,
		from:             from,
		to:               to,
		expectedChecksum: expectedChecksum,
		logger:           logger,
	}); err != nil {
		return
	}

	logger.Info("文件已重命名", "from", from, "to", to, "checksum", expectedChecksum)
	sendJSONResponse(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件已重命名: %s -> %s", from, to),
		Checksum: expectedChecksum,
	}, http.StatusOK)
}
