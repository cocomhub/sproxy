// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
)

// resolveAndValidateFile 校验文件名并返回安全的远程路径和完整路径。
// 校验失败时返回 ("", "", false)。
func (h *Handlers) resolveAndValidateFile(filename string) (remotePath, fullPath string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		return "", "", false
	}
	fullPath = h.safePath(remotePath)
	if fullPath == "" {
		return "", "", false
	}
	return remotePath, fullPath, true
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	logger := h.logger

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgEmptyFilename}, http.StatusBadRequest)
		return
	}
	remotePath, filePath, ok := h.resolveAndValidateFile(filename)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	expectedChecksum := r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		logger.Warn("X-File-Checksum 为空", "file_name", remotePath)
		return
	}

	// 基于 fd 操作缩小 TOCTOU 窗口：先打开文件，再基于 fd 执行 Stat 和 checksum 校验
	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
			return
		}
		h.logger.Error("打开文件失败", "file_name", remotePath, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "打开文件失败"}, http.StatusInternalServerError)
		return
	}

	// 基于 fd 的 Stat
	info, err := file.Stat()
	if err != nil {
		file.Close()
		h.logger.Error("stat 文件失败", "file_name", remotePath, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "stat 失败"}, http.StatusInternalServerError)
		return
	}
	_ = info

	// 基于 fd 的 checksum 校验
	cs, err := Checksum(file)
	_, _ = file.Seek(0, io.SeekStart)
	if err != nil {
		file.Close()
		h.logger.Error("计算文件 checksum 失败", "file_name", remotePath, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件校验失败"}, http.StatusInternalServerError)
		return
	}
	if cs != expectedChecksum {
		file.Close()
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件校验失败"}, http.StatusBadRequest)
		logger.Warn("文件校验失败", "file_name", remotePath)
		return
	}

	// 关闭后再删除
	file.Close()
	if err := os.Remove(filePath); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除文件失败"}, http.StatusInternalServerError)
		return
	}
	h.checksumStore.Delete(remotePath)
	if h.metrics != nil {
		h.metrics.RecordDelete()
	}
	logger.Info("文件已删除", "file_name", remotePath)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件删除成功: %s", remotePath)}, http.StatusOK)
}

// processBatchDeleteItem 处理单条文件删除操作。
func (h *Handlers) processBatchDeleteItem(f BatchDeleteFile, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: f.Filename}
	remotePath, filePath, ok := h.resolveAndValidateFile(f.Filename)
	if !ok {
		result.Message = "无效的文件路径"
		return result
	}
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		result.Success = true
		result.Message = "文件不存在（幂等删除）"
		logger.Warn("批量删除：文件不存在（幂等删除）", "file_name", remotePath)
		return result
	}
	if f.Checksum == "" {
		result.Message = "缺少 checksum"
		return result
	}
	// 校验 checksum，不匹配时拒绝删除
	if !verifyFileWithChecksum(filePath, f.Checksum) {
		result.Message = "文件校验失败"
		logger.Warn("批量删除时 checksum 不匹配", "file_name", remotePath)
		return result
	}
	if err := os.Remove(filePath); err != nil {
		result.Message = "删除失败"
	} else {
		h.checksumStore.Delete(remotePath)
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
	if len(req.Files) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := h.logger.With("batch", "delete")
	results := make([]BatchOperationResult, 0, len(req.Files))
	for _, f := range req.Files {
		results = append(results, h.processBatchDeleteItem(f, logger))
	}
	sendJSONResponse(w, BatchResponse{Results: results}, http.StatusOK)
}
