// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
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
	UploadID      string `json:"upload_id"`
	ReceivedCount int    `json:"received_count"`
	TotalChunks   int    `json:"total_chunks"`
	MissingChunks []int  `json:"missing_chunks"`
	Completed     bool   `json:"completed"`
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
	_ = json.NewEncoder(w).Encode(response)
}
