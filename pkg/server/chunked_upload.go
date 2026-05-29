// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// uploadInit 初始化一个分块上传会话。
func (h *Handlers) uploadInit(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	// 限制请求体大小
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB 足够
	var req struct {
		Filename     string `json:"filename"`
		TotalSize    int64  `json:"total_size"`
		ChunkSize    int64  `json:"chunk_size"`
		TotalChunks  int    `json:"total_chunks"`
		FileChecksum string `json:"file_checksum"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "请求体解析失败: " + err.Error()}, http.StatusBadRequest)
		return
	}

	// 校验字段
	if filepath.Base(req.Filename) != req.Filename {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}
	if req.TotalSize <= 0 {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "total_size 必须大于 0"}, http.StatusBadRequest)
		return
	}
	if req.ChunkSize <= 0 {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "chunk_size 必须大于 0"}, http.StatusBadRequest)
		return
	}
	if req.TotalChunks <= 0 {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "total_chunks 必须大于 0"}, http.StatusBadRequest)
		return
	}
	if req.ChunkSize*int64(req.TotalChunks) < req.TotalSize {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "chunk_size * total_chunks 应 >= total_size"}, http.StatusBadRequest)
		return
	}
	if len(req.FileChecksum) != 64 {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "file_checksum 必须是 64 位 hex"}, http.StatusBadRequest)
		return
	}
	if _, err := hex.DecodeString(req.FileChecksum); err != nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "file_checksum 不是有效的 hex"}, http.StatusBadRequest)
		return
	}

	// 确保上传目录存在
	if err := os.MkdirAll(cfg.UploadsDir, 0755); err != nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传目录失败"}, http.StatusInternalServerError)
		return
	}

	// 已存在同名文件的检查：如果文件已在，且 checksum 匹配，返回成功
	existingPath := filepath.Join(cfg.UploadsDir, req.Filename)
	if stat, err := os.Stat(existingPath); err == nil {
		if verifyFileWithChecksum(existingPath, req.FileChecksum) {
			sendJSONResponse(w, ChunkedInitResponse{
				Success:  true,
				UploadID: "already_exists",
				Message:  fmt.Sprintf("文件已存在, size: %d", stat.Size()),
			}, http.StatusOK)
			return
		}
		// 文件存在但 checksum 不匹配，不允许覆盖
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "同名文件已存在但 checksum 不匹配"}, http.StatusConflict)
		return
	}

	// 查找可续传的 session
	chunkSize := req.ChunkSize
	if chunkSize <= 0 {
		chunkSize = cfg.ChunkSize
	}
	if chunkSize <= 0 {
		chunkSize = 4 << 20 // 4 MiB 保底
	}

	session, reused, err := h.uploadStore.GetOrCreateSession(req.Filename, req.TotalSize, chunkSize, req.TotalChunks, req.FileChecksum)
	if err != nil {
		h.logger.Error("创建上传会话失败", "error", err)
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
		return
	}

	msg := "上传会话已创建"
	if reused {
		missing := MissingChunks(session)
		msg = fmt.Sprintf("续传会话已恢复，缺失 %d 个分块", len(missing))
	}

	sendJSONResponse(w, ChunkedInitResponse{
		Success:   true,
		UploadID:  session.UploadID,
		ChunkSize: session.ChunkSize,
		Message:   msg,
	}, http.StatusOK)
}

// uploadChunk 上传单个分块。
func (h *Handlers) uploadChunk(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	// 限制请求体大小
	if cfg.MaxChunkUploadBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, cfg.MaxChunkUploadBytes)
	}

	// 解析 multipart
	if err := r.ParseMultipartForm(1 << 20); err != nil { // 1MB 内存缓冲
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "解析 multipart 失败: " + err.Error()}, http.StatusRequestEntityTooLarge)
		return
	}

	uploadID := r.FormValue("upload_id")
	chunkIndexStr := r.FormValue("chunk_index")
	chunkChecksum := r.FormValue("chunk_checksum")

	if uploadID == "" || chunkIndexStr == "" {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "缺少 upload_id 或 chunk_index"}, http.StatusBadRequest)
		return
	}

	chunkIndex := 0
	if _, err := fmt.Sscanf(chunkIndexStr, "%d", &chunkIndex); err != nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "无效的 chunk_index"}, http.StatusBadRequest)
		return
	}

	// 获取 session
	session := h.uploadStore.GetSession(uploadID)
	if session == nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "upload_id 不存在或已过期"}, http.StatusNotFound)
		return
	}

	if session.Completed {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "上传已完成，不接受新分块"}, http.StatusGone)
		return
	}

	if chunkIndex < 0 || chunkIndex >= session.TotalChunks {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: fmt.Sprintf("chunk_index %d 超出范围 [0, %d)", chunkIndex, session.TotalChunks)}, http.StatusBadRequest)
		return
	}

	file, _, err := r.FormFile("chunk")
	if err != nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "读取分块文件失败: " + err.Error()}, http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 幂等：如果该块已接收且 checksum 匹配，直接返回成功
	if session.ReceivedChunks[chunkIndex] && session.ChunkChecksums[chunkIndex] == chunkChecksum {
		sendJSONResponse(w, ChunkUploadResponse{Success: true, ChunkIndex: chunkIndex, Message: "分块已存在，跳过"}, http.StatusOK)
		return
	}

	// 写入 chunk 文件并计算 SHA-256
	chunkPath := h.uploadStore.ChunkFilePath(uploadID, chunkIndex)
	// 确保 session 目录存在
	_ = os.MkdirAll(filepath.Dir(chunkPath), 0755)

	tmpPath := chunkPath + ".tmp"
	tempFile, err := os.Create(tmpPath)
	if err != nil {
		h.logger.Error("创建 chunk 临时文件失败", "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "创建临时文件失败"}, http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()
	defer os.Remove(tmpPath)

	// 边写边算 SHA-256
	sha256Hash := sha256.New()
	multiWriter := io.MultiWriter(tempFile, sha256Hash)
	if _, err := io.Copy(multiWriter, file); err != nil {
		h.logger.Error("写入 chunk 失败", "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "写入分块失败"}, http.StatusInternalServerError)
		return
	}

	if err := tempFile.Close(); err != nil {
		h.logger.Error("关闭 chunk 临时文件失败", "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "关闭临时文件失败"}, http.StatusInternalServerError)
		return
	}

	// 校验 SHA-256
	serverChecksum := hex.EncodeToString(sha256Hash.Sum(nil))
	if chunkChecksum != "" && serverChecksum != chunkChecksum {
		h.logger.Warn("chunk SHA-256 不匹配", "chunk_index", chunkIndex, "server", serverChecksum, "client", chunkChecksum)
		sendJSONResponse(w, ChunkUploadResponse{
			Success:     false,
			ChunkIndex:  chunkIndex,
			ShouldRetry: true,
			Message:     "SHA-256 校验不匹配",
		}, http.StatusOK)
		return
	}

	// 原子重命名
	if err := os.Rename(tmpPath, chunkPath); err != nil {
		h.logger.Error("重命名 chunk 文件失败", "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "保存分块失败"}, http.StatusInternalServerError)
		return
	}

	// 更新 session
	if err := h.uploadStore.MarkChunkReceived(uploadID, chunkIndex, serverChecksum); err != nil {
		h.logger.Error("标记分块已接收失败", "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "更新状态失败"}, http.StatusInternalServerError)
		return
	}

	sendJSONResponse(w, ChunkUploadResponse{
		Success:    true,
		ChunkIndex: chunkIndex,
		Message:    fmt.Sprintf("分块 %d 已接收并校验通过", chunkIndex),
	}, http.StatusOK)
}

// uploadStatus 查询上传会话状态。
func (h *Handlers) uploadStatus(w http.ResponseWriter, r *http.Request) {
	uploadID := r.URL.Query().Get("upload_id")
	if uploadID == "" {
		sendJSONResponse(w, ChunkStatusResponse{Success: false}, http.StatusBadRequest)
		return
	}

	session := h.uploadStore.GetSession(uploadID)
	if session == nil {
		sendJSONResponse(w, ChunkStatusResponse{Success: false}, http.StatusNotFound)
		return
	}

	missing := MissingChunks(session)
	sendJSONResponse(w, ChunkStatusResponse{
		Success:       true,
		UploadID:      session.UploadID,
		ReceivedCount: len(session.ReceivedChunks) - len(missing),
		TotalChunks:   session.TotalChunks,
		MissingChunks: missing,
		Completed:     session.Completed,
	}, http.StatusOK)
}

// uploadComplete 合并所有分块完成上传。
func (h *Handlers) uploadComplete(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1KB 足够
	var req struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "请求体解析失败: " + err.Error()}, http.StatusBadRequest)
		return
	}

	if req.UploadID == "" {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "缺少 upload_id"}, http.StatusBadRequest)
		return
	}

	session := h.uploadStore.GetSession(req.UploadID)
	if session == nil {
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "upload_id 不存在或已过期"}, http.StatusNotFound)
		return
	}

	if session.Completed {
		sendJSONResponse(w, ChunkCompleteResponse{
			Success:      true,
			Filename:     session.Filename,
			FileChecksum: session.FileChecksum,
			Message:      "上传已完成",
		}, http.StatusOK)
		return
	}

	// 检查所有分块是否已接收
	if !h.uploadStore.AllChunksReceived(req.UploadID) {
		// 刷新 session 获取最新状态
		session = h.uploadStore.GetSession(req.UploadID)
		missing := MissingChunks(session)
		sendJSONResponse(w, ChunkCompleteResponse{
			Success: false,
			Message: fmt.Sprintf("还有 %d 个分块未接收", len(missing)),
		}, http.StatusBadRequest)
		return
	}

	// 合并分块
	filePath := filepath.Join(cfg.UploadsDir, session.Filename)
	tmpPath := filePath + ".tmp"
	outFile, err := os.Create(tmpPath)
	if err != nil {
		h.logger.Error("创建合并文件失败", "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
		return
	}
	defer outFile.Close()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outFile, hasher)

	for i := 0; i < session.TotalChunks; i++ {
		chunkPath := h.uploadStore.ChunkFilePath(req.UploadID, i)
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			h.logger.Error("打开 chunk 文件失败", "chunk_index", i, "error", err)
			sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: fmt.Sprintf("读取分块 %d 失败", i)}, http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(multiWriter, chunkFile); err != nil {
			chunkFile.Close()
			h.logger.Error("合并 chunk 失败", "chunk_index", i, "error", err)
			sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: fmt.Sprintf("合并分块 %d 失败", i)}, http.StatusInternalServerError)
			return
		}
		chunkFile.Close()
	}

	if err := outFile.Close(); err != nil {
		h.logger.Error("关闭合并文件失败", "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "关闭目标文件失败"}, http.StatusInternalServerError)
		return
	}

	// 校验最终文件的 SHA-256
	finalChecksum := hex.EncodeToString(hasher.Sum(nil))
	if finalChecksum != session.FileChecksum {
		h.logger.Error("最终文件 SHA-256 校验失败", "server", finalChecksum, "client", session.FileChecksum)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "最终文件 SHA-256 校验失败，文件未保存"}, http.StatusBadRequest)
		return
	}

	// 原子重命名为最终文件名
	if err := os.Rename(tmpPath, filePath); err != nil {
		h.logger.Error("重命名最终文件失败", "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}

	// 记录 checksum
	h.checksumStore.Set(session.Filename, finalChecksum)

	// 标记完成（延迟清理 session 目录）
	if err := h.uploadStore.CompleteSession(req.UploadID); err != nil {
		h.logger.Warn("标记 session 完成失败", "error", err)
	}

	// 异步清理 session 目录
	go func() {
		time.Sleep(5 * time.Second)
		h.uploadStore.DeleteSession(req.UploadID)
	}()

	sendJSONResponse(w, ChunkCompleteResponse{
		Success:      true,
		Filename:     session.Filename,
		FileChecksum: finalChecksum,
		Message:      "文件合并并校验通过",
	}, http.StatusOK)
}
