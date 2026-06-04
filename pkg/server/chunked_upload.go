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

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/internal/size"
)

// uploadInit 初始化一个分块上传会话。
func (h *Handlers) uploadInit(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	// 限制请求体大小
	r.Body = http.MaxBytesReader(w, r.Body, size.MultipartBufSize) // 1MB 足够
	var req struct {
		UploadID     string `json:"upload_id"`
		Filename     string `json:"filename"`
		TotalSize    int64  `json:"total_size"`
		ChunkSize    int64  `json:"chunk_size"`
		TotalChunks  int    `json:"total_chunks"`
		FileChecksum string `json:"file_checksum"`
		FileModTime  int64  `json:"file_mod_time"` // UnixNano, 0 = unknown
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "请求体解析失败: " + err.Error()}, http.StatusBadRequest)
		return
	}

	h.logger.Debug("uploadInit 请求", "filename", req.Filename, "total_size", req.TotalSize,
		"chunk_size", req.ChunkSize, "total_chunks", req.TotalChunks,
		"file_checksum", shortid.ShortHash(req.FileChecksum), "upload_id", req.UploadID)

	// 校验字段
	if req.UploadID == "" {
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "缺少 upload_id"}, http.StatusBadRequest)
		return
	}
	if _, err := ValidateFilePath(req.Filename); err != nil {
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
			h.logger.Info("文件已存在，跳过上传", "filename", req.Filename, "size", stat.Size(), "checksum", shortid.ShortHash(req.FileChecksum))
			sendJSONResponse(w, ChunkedInitResponse{
				Success:  true,
				UploadID: "already_exists",
				Message:  fmt.Sprintf("文件已存在, size: %d", stat.Size()),
			}, http.StatusOK)
			return
		}
		// 文件存在但 checksum 不匹配，不允许覆盖
		h.logger.Warn("同名文件已存在但 checksum 不匹配", "filename", req.Filename)
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "同名文件已存在但 checksum 不匹配"}, http.StatusConflict)
		return
	}

	// 分块大小：使用客户端传入的 chunk_size，由客户端自适应计算
	chunkSize := req.ChunkSize
	if chunkSize <= 0 {
		chunkSize = cfg.ChunkSize
	}
	if chunkSize <= 0 {
		chunkSize = size.DefaultChunkSize // 4 MiB 保底
	}

	// 确保客户端 chunk 不超过服务端单块请求上限
	// 预留 1024 字节用于 multipart 边界与表单字段开销，避免 body 略超限制导致 413
	if chunkSize > size.DefaultChunkBodyLimit-1024 {
		h.logger.Info("chunk_size 超出服务端上限，自动裁剪",
			"client_chunk_size", chunkSize,
			"max_chunk_upload_bytes", size.DefaultChunkBodyLimit,
			"filename", req.Filename,
			"upload_id", shortid.ShortHash(req.UploadID))
		chunkSize = size.DefaultChunkBodyLimit - 1024
		req.TotalChunks = int((req.TotalSize + chunkSize - 1) / chunkSize)
	}

	session, reused, err := h.uploadStore.GetOrCreateSession(req.UploadID, req.Filename,
		req.TotalSize, chunkSize, req.TotalChunks, req.FileChecksum, req.FileModTime)
	if err != nil {
		h.logger.Error("创建/续传上传会话失败", "upload_id", req.UploadID, "error", err)
		sendJSONResponse(w, ChunkedInitResponse{Success: false, Message: "创建上传会话失败"}, http.StatusInternalServerError)
		return
	}

	msg := "上传会话已创建"
	if reused {
		missing := MissingChunks(session)
		msg = fmt.Sprintf("续传会话已恢复，缺失 %d 个分块", len(missing))
		h.logger.Info("续传会话", "upload_id", session.UploadID, "filename", req.Filename,
			"missing", len(missing), "total", session.TotalChunks)
	} else {
		h.logger.Info("新上传会话", "upload_id", session.UploadID, "filename", req.Filename,
			"total_size", req.TotalSize, "total_chunks", session.TotalChunks)
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
	// 限制请求体大小（含 multipart 开销）
	r.Body = http.MaxBytesReader(w, r.Body, size.DefaultChunkBodyLimit)

	// 解析 multipart
	if err := r.ParseMultipartForm(size.DefaultChunkBodyLimit); err != nil { // 超过 DefaultChunkBodyLimit 由 MaxBytesReader 拦截
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
	if chunkChecksum == "" {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "缺少 chunk_checksum"}, http.StatusBadRequest)
		return
	}
	if len(chunkChecksum) != 64 {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "chunk_checksum 必须是 64 位 hex"}, http.StatusBadRequest)
		return
	}
	if _, err := hex.DecodeString(chunkChecksum); err != nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "chunk_checksum 不是有效的 hex"}, http.StatusBadRequest)
		return
	}

	chunkIndex := 0
	if _, err := fmt.Sscanf(chunkIndexStr, "%d", &chunkIndex); err != nil {
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "无效的 chunk_index"}, http.StatusBadRequest)
		return
	}

	h.logger.Debug("uploadChunk 请求", "upload_id", uploadID, "chunk_index", chunkIndex)

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
		h.logger.Debug("chunk 已存在，跳过", "upload_id", uploadID, "chunk_index", chunkIndex, "checksum", shortid.ShortHash(chunkChecksum))
		sendJSONResponse(w, ChunkUploadResponse{Success: true, ChunkIndex: chunkIndex, Message: "分块已存在，跳过"}, http.StatusOK)
		return
	}

	// 写入 chunk 文件并计算 SHA-256
	chunkPath := h.uploadStore.ChunkFilePath(uploadID, chunkIndex)
	// 获取 chunk IO 读锁：允许多个 uploadChunk 并发写入不同 chunk，
	// 但阻塞 mergeOneChunk 的写锁，直到本 chunk 重命名完成。
	unlockIO := h.uploadStore.lockChunkIO(uploadID)
	defer unlockIO()
	// 确保 session 目录存在
	if err := os.MkdirAll(filepath.Dir(chunkPath), 0755); err != nil {
		h.logger.Warn("创建 session 目录失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
	}

	tmpPath := chunkPath + ".tmp"
	tempFile, err := os.Create(tmpPath)
	if err != nil {
		h.logger.Error("创建 chunk 临时文件失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, Message: "创建临时文件失败"}, http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()
	defer os.Remove(tmpPath)

	// 边写边算 SHA-256
	sha256Hash := sha256.New()
	multiWriter := io.MultiWriter(tempFile, sha256Hash)
	if _, err := io.Copy(multiWriter, file); err != nil {
		h.logger.Error("写入 chunk 失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "写入分块失败"}, http.StatusInternalServerError)
		return
	}

	if err := tempFile.Close(); err != nil {
		h.logger.Error("关闭 chunk 临时文件失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "关闭临时文件失败"}, http.StatusInternalServerError)
		return
	}

	// 校验 SHA-256
	serverChecksum := hex.EncodeToString(sha256Hash.Sum(nil))
	if serverChecksum != chunkChecksum {
		h.logger.Warn("chunk SHA-256 不匹配", "upload_id", uploadID, "chunk_index", chunkIndex,
			"server", shortid.ShortHash(serverChecksum), "client", shortid.ShortHash(chunkChecksum))
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
		h.logger.Error("重命名 chunk 文件失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "保存分块失败"}, http.StatusInternalServerError)
		return
	}

	// 更新 session
	if err := h.uploadStore.MarkChunkReceived(uploadID, chunkIndex, serverChecksum); err != nil {
		h.logger.Error("标记分块已接收失败", "upload_id", uploadID, "chunk_index", chunkIndex, "error", err)
		sendJSONResponse(w, ChunkUploadResponse{Success: false, ChunkIndex: chunkIndex, ShouldRetry: true, Message: "更新状态失败"}, http.StatusInternalServerError)
		return
	}

	h.logger.Debug("chunk 上传成功", "upload_id", uploadID, "chunk_index", chunkIndex, "checksum", shortid.ShortHash(serverChecksum))
	sendJSONResponse(w, ChunkUploadResponse{
		Success:    true,
		ChunkIndex: chunkIndex,
		Message:    fmt.Sprintf("分块 %d 已接收并校验通过", chunkIndex),
	}, http.StatusOK)
}

// uploadStatus 查询上传会话状态。
func (h *Handlers) uploadStatus(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	uploadID := params.Get("upload_id")
	filename := params.Get("filename")

	// 1. 按 upload_id 查 session
	if uploadID != "" {
		session := h.uploadStore.GetSession(uploadID)
		if session != nil {
			missing := MissingChunks(session)
			finished := session.Completed
			sendJSONResponse(w, ChunkStatusResponse{
				Success:       true,
				Finished:      finished,
				UploadID:      session.UploadID,
				ReceivedCount: len(session.ReceivedChunks) - len(missing),
				TotalChunks:   session.TotalChunks,
				MissingChunks: missing,
				Completed:     session.Completed,
				FileChecksum:  session.FileChecksum,
				Filename:      session.Filename,
				Message:       fmt.Sprintf("会话%d/%d分块已接收", len(session.ReceivedChunks)-len(missing), session.TotalChunks),
			}, http.StatusOK)
			return
		}
		// upload_id 存在但 session 不存在
		if filename == "" {
			sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: "upload_id 不存在或已过期"}, http.StatusNotFound)
			return
		}
	}

	// 2. 按 filename 查找未完成的 session
	if filename != "" {
		// 防御性校验：防止路径穿越
		if _, err := ValidateFilePath(filename); err != nil {
			sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
			return
		}
		session := h.uploadStore.GetSessionByFilename(filename)
		if session != nil {
			missing := MissingChunks(session)
			sendJSONResponse(w, ChunkStatusResponse{
				Success:       true,
				UploadID:      session.UploadID,
				ReceivedCount: len(session.ReceivedChunks) - len(missing),
				TotalChunks:   session.TotalChunks,
				MissingChunks: missing,
				Completed:     session.Completed,
				FileChecksum:  session.FileChecksum,
				Filename:      session.Filename,
			}, http.StatusOK)
			return
		}

		// 3. 检查磁盘上文件是否已存在且 checksum 匹配
		cfg := h.cfgPtr.Load()
		filePath := filepath.Join(cfg.UploadsDir, filename)
		if stat, err := os.Stat(filePath); err == nil {
			if checksum, ok := h.checksumStore.Get(filename); ok {
				sendJSONResponse(w, ChunkStatusResponse{
					Success:      true,
					Finished:     true,
					Completed:    true,
					FileChecksum: checksum,
					Filename:     filename,
					Message:      fmt.Sprintf("文件已存在, size: %d", stat.Size()),
				}, http.StatusOK)
				return
			}
			// 有文件但无 checksum 记录（意外情况），实时计算
			if cs, err := FileChecksum(filePath); err == nil {
				sendJSONResponse(w, ChunkStatusResponse{
					Success:      true,
					Finished:     true,
					Completed:    true,
					FileChecksum: cs,
					Filename:     filename,
					Message:      fmt.Sprintf("文件已存在, size: %d", stat.Size()),
				}, http.StatusOK)
				return
			}
		}
	}

	// 什么都没找到
	sendJSONResponse(w, ChunkStatusResponse{Success: false, Message: "未找到文件或上传会话"}, http.StatusNotFound)
}

// uploadComplete 合并所有分块完成上传。
func (h *Handlers) uploadComplete(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	r.Body = http.MaxBytesReader(w, r.Body, size.CompleteBodyLimit) // 1KB 足够
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

	h.logger.Info("uploadComplete 开始", "upload_id", req.UploadID, "filename", session.Filename,
		"received", countReceived(session.ReceivedChunks), "total", session.TotalChunks)

	if session.Completed {
		h.logger.Info("上传已完成（幂等）", "upload_id", req.UploadID, "filename", session.Filename)
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
		h.logger.Warn("合并请求时还有分块未接收", "upload_id", req.UploadID, "missing", len(missing))
		sendJSONResponse(w, ChunkCompleteResponse{
			Success: false,
			Message: fmt.Sprintf("还有 %d 个分块未接收", len(missing)),
		}, http.StatusBadRequest)
		return
	}

	// 合并分块
	filePath := filepath.Join(cfg.UploadsDir, session.Filename)

	// 确保目标文件的父目录存在（支持子目录路径）
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		h.logger.Error("创建目标父目录失败", "upload_id", req.UploadID, "filename", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "创建目标目录失败"}, http.StatusInternalServerError)
		return
	}

	tmpPath := filePath + ".tmp"
	outFile, err := os.Create(tmpPath)
	if err != nil {
		h.logger.Error("创建合并文件失败", "upload_id", req.UploadID, "filename", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
		return
	}
	defer outFile.Close()
	defer os.Remove(tmpPath)

	hasher := sha256.New()
	multiWriter := io.MultiWriter(outFile, hasher)

	for i := 0; i < session.TotalChunks; i++ {
		if err := h.mergeOneChunk(req.UploadID, i, multiWriter); err != nil {
			h.logger.Error("合并 chunk 失败", "upload_id", req.UploadID, "chunk_index", i, "error", err)
			sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: fmt.Sprintf("合并分块 %d 失败: %v", i, err)}, http.StatusInternalServerError)
			return
		}
	}

	if err := outFile.Close(); err != nil {
		h.logger.Error("关闭合并文件失败", "upload_id", req.UploadID, "error", err)
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
		h.logger.Error("重命名最终文件失败", "upload_id", req.UploadID, "filename", session.Filename, "error", err)
		sendJSONResponse(w, ChunkCompleteResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}

	// 保留文件原始修改时间
	if session.FileModTime > 0 {
		modTime := time.Unix(0, session.FileModTime)
		if err := os.Chtimes(filePath, modTime, modTime); err != nil {
			h.logger.Warn("设置文件时间戳失败", "filename", session.Filename, "error", err)
		}
	}

	// 记录 checksum
	h.checksumStore.Set(session.Filename, finalChecksum)

	// 标记完成（延迟清理 session 目录）
	if err := h.uploadStore.CompleteSession(req.UploadID); err != nil {
		h.logger.Warn("标记 session 完成失败", "upload_id", req.UploadID, "error", err)
	}

	// 异步清理 session 目录（由 wg 追踪，支持优雅停止）
	h.uploadStore.CleanupSessionAfter(req.UploadID, 5*time.Second)

	h.logger.Info("文件合并完成", "filename", session.Filename, "checksum", shortid.ShortHash(finalChecksum), "size", session.TotalSize)
	sendJSONResponse(w, ChunkCompleteResponse{
		Success:      true,
		Filename:     session.Filename,
		FileChecksum: finalChecksum,
		Message:      "文件合并并校验通过",
	}, http.StatusOK)
}

// mergeOneChunk 读取单个 chunk 文件并把内容拷贝到 dst。
// 获取 chunk 合并写锁：等待所有正在写入的 chunk 完成后才允许读取，
// 阻塞新的 chunk 写入，避免读到不完整的 chunk。
func (h *Handlers) mergeOneChunk(uploadID string, idx int, dst io.Writer) error {
	chunkPath := h.uploadStore.ChunkFilePath(uploadID, idx)
	unlockMerge := h.uploadStore.lockChunkMerge(uploadID)
	defer unlockMerge()
	chunkFile, err := os.Open(chunkPath)
	if err != nil {
		return fmt.Errorf("打开 chunk %d 失败: %w", idx, err)
	}
	defer chunkFile.Close()
	if _, err := io.Copy(dst, chunkFile); err != nil {
		return fmt.Errorf("拷贝 chunk %d 失败: %w", idx, err)
	}
	return nil
}
