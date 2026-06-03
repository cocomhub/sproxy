// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

type UploadResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
}

// ChunkedInitResponse 分块上传初始化响应。
type ChunkedInitResponse struct {
	Success   bool   `json:"success"`
	UploadID  string `json:"upload_id,omitempty"`
	ChunkSize int64  `json:"chunk_size,omitempty"`
	Message   string `json:"message,omitempty"`
}

// ChunkStatusResponse 分块上传状态查询响应。
type ChunkStatusResponse struct {
	Success       bool   `json:"success"`
	Finished      bool   `json:"finished,omitempty"`  // 文件已完整上传（无需再传）
	UploadID      string `json:"upload_id,omitempty"` // omitempty 以便 finished 时返回空
	ReceivedCount int    `json:"received_count,omitempty"`
	TotalChunks   int    `json:"total_chunks,omitempty"`
	MissingChunks []int  `json:"missing_chunks,omitempty"`
	Completed     bool   `json:"completed,omitempty"`
	FileChecksum  string `json:"file_checksum,omitempty"`
	Filename      string `json:"filename,omitempty"`
	Message       string `json:"message,omitempty"`
}

// ChunkUploadResponse 单块上传响应。
type ChunkUploadResponse struct {
	Success     bool   `json:"success"`
	ChunkIndex  int    `json:"chunk_index"`
	ShouldRetry bool   `json:"should_retry,omitempty"`
	Message     string `json:"message,omitempty"`
}

// ChunkCompleteResponse 分块上传合并完成响应。
type ChunkCompleteResponse struct {
	Success      bool   `json:"success"`
	Filename     string `json:"filename,omitempty"`
	FileChecksum string `json:"file_checksum,omitempty"`
	Message      string `json:"message,omitempty"`
}

func sendJSONResponse(w http.ResponseWriter, response any, statusCode int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		slog.Default().Warn("Encode JSON response failed", "error", err)
	}
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

// BatchRenameOp 单条重命名操作
type BatchRenameOp struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Checksum string `json:"checksum"`
}
