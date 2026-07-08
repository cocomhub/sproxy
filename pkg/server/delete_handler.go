// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(headerRequestID)
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgEmptyFilename}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}
	filePath := h.safePath(remotePath)
	if filePath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}

	expectedChecksum := r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		logger.Warn("X-File-Checksum 为空", "file_name", remotePath)
		return
	}

	if !verifyFileWithChecksum(filePath, expectedChecksum) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件校验失败"}, http.StatusBadRequest)
		logger.Warn("文件校验失败", "file_name", remotePath)
		return
	}

	if err := os.Remove(filePath); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除文件失败"}, http.StatusInternalServerError)
		return
	}
	h.checksumStore.Delete(remotePath)
	if h.metrics != nil {
		h.metrics.RecordDelete()
	}
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件删除成功: %s", remotePath)}, http.StatusOK)
}

// processBatchDeleteItem 处理单条文件删除操作。
func (h *Handlers) processBatchDeleteItem(f BatchDeleteFile, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: f.Filename}
	remotePath, err := ValidateFilePath(f.Filename)
	if err != nil {
		result.Message = errMsgInvalidFilename
		return result
	}
	filePath := h.safePath(remotePath)
	if filePath == "" {
		result.Message = "无效的文件路径"
		return result
	}
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		result.Success = true
		result.Message = "文件不存在（幂等删除）"
		return result
	}
	if f.Checksum == "" {
		result.Message = "缺少 checksum"
		return result
	}
	// 校验 checksum
	valid := verifyFileWithChecksum(filePath, f.Checksum)
	// 仍然执行删除，但标记校验失败
	if err := os.Remove(filePath); err != nil {
		result.Message = "删除失败"
	} else {
		h.checksumStore.Delete(remotePath)
		result.Success = true
		result.Message = "删除成功"
		if !valid {
			result.Message = "删除成功（checksum 不匹配，文件内容可能已变更）"
			logger.Warn("删除时 checksum 不匹配", "file_name", remotePath)
		}
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
	if len(req.Files) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := h.logger.With("batch", "delete")
	results := make([]BatchOperationResult, 0, len(req.Files))
	for _, f := range req.Files {
		results = append(results, h.processBatchDeleteItem(f, logger))
	}
	sendJSONResponse(w, BatchDeleteResponse{Results: results}, http.StatusOK)
}
