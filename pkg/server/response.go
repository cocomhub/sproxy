// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sync/atomic"
)

// responseLogger 是 sendJSONResponse 使用的日志记录器，默认使用 slog.Default()。
// 可通过 SetResponseLogger 替换，用于测试或自定义日志输出。
var responseLogger atomic.Pointer[slog.Logger]

func init() {
	SetResponseLogger(slog.Default())
}

// SetResponseLogger 设置 sendJSONResponse 使用的日志记录器。
func SetResponseLogger(l *slog.Logger) {
	responseLogger.Store(l)
}

// UploadResponse 是通用响应结构。
type UploadResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
}

// 分块族的响应 DTO（ChunkedInitResponse / ChunkStatusResponse / UploadSessionInfo /
// ChunkSessionsResponse / ChunkUploadResponse / ChunkCompleteResponse）随分块处理器迁入
// pkg/files（DTO 定义在写出它的包里，见该包 service.go 的 HTTP 契约小节）。

func sendJSONResponse(w http.ResponseWriter, response any, statusCode int) {
	w.Header().Set(headerContentType, contentTypeJSON)
	buf, err := json.Marshal(response)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		responseLogger.Load().Warn("Encode JSON response failed", "error", err)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"})
		return
	}
	w.WriteHeader(statusCode)
	_, _ = w.Write(buf)
}

// formatContentDisposition 使用标准库安全地构造 Content-Disposition 头。
// 同时设置 filename（传统）和 filename*（RFC 5987）参数，以支持非 ASCII 文件名。
func formatContentDisposition(filename string) string {
	if filename == "" {
		return "attachment"
	}
	return mime.FormatMediaType("attachment", map[string]string{
		"filename":  filename,
		"filename*": "UTF-8''" + url.PathEscape(filename),
	})
}

// BatchOperationResult 批量操作单条结果
type BatchOperationResult struct {
	Filename string `json:"filename"`
	Success  bool   `json:"success"`
	Message  string `json:"message"`
}

// BatchOperationRequest 批量删除请求体
type BatchDeleteRequest struct {
	Files []BatchDeleteFile `json:"files"`
}

// BatchDeleteFile 批量删除中的单条文件
type BatchDeleteFile struct {
	Filename string `json:"filename"`
	Checksum string `json:"checksum"`
}

// BatchRenameRequest 批量重命名请求体
type BatchRenameRequest struct {
	Operations []BatchRenameOp `json:"operations"`
}

// BatchResponse is the JSON response for batch operations (delete, rename, etc.).
type BatchResponse struct {
	Results []BatchOperationResult `json:"results"`
}

// BatchRenameOp 单条重命名操作
type BatchRenameOp struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Checksum string `json:"checksum"`
}

// CloudBatchTaskResult 批量下载单个任务结果。
type CloudBatchTaskResult struct {
	ID       string `json:"id"`
	Owner    string `json:"owner,omitempty"` // 任务归属（创建者 AK / API key 名；空 = 全局兼容）
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}
