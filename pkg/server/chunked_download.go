// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

const maxChunkHashBuf = 8 << 20 // 8 MiB 最大缓冲用于计算 chunk hash

// downloadChunk 下载文件的指定分块。
//
// 参数：
//   - filename: 文件名（path.Base 校验防穿越）
//   - offset: 起始偏移量（默认 0）
//   - length: 分块长度（默认 4 MiB）
//
// 响应头：
//   - Content-Range: bytes offset-(offset+length-1)/fileSize
//   - X-Chunk-Checksum: 本块的 SHA-256
//   - X-File-Checksum: 完整文件的 SHA-256（若 ChecksumStore 有记录）
func (h *Handlers) downloadChunk(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}
	if _, err := ValidateFilePath(filename); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}

	// 解析 offset 和 length
	offset := int64(0)
	length := cfg.ChunkSize
	if length <= 0 {
		length = 4 << 20 // 4 MiB 保底
	}

	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if _, err := fmt.Sscanf(offsetStr, "%d", &offset); err != nil || offset < 0 {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的 offset"}, http.StatusBadRequest)
			return
		}
	}
	if lengthStr := r.URL.Query().Get("length"); lengthStr != "" {
		if _, err := fmt.Sscanf(lengthStr, "%d", &length); err != nil || length <= 0 {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的 length"}, http.StatusBadRequest)
			return
		}
		// 保护：防止 length 过大导致 OOM
		if length > cfg.MaxChunkUploadBytes && cfg.MaxChunkUploadBytes > 0 {
			length = cfg.MaxChunkUploadBytes
		}
	}

	filePath := filepath.Join(cfg.UploadsDir, filename)
	stat, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "访问文件失败"}, http.StatusInternalServerError)
		}
		return
	}

	fileSize := stat.Size()
	if offset >= fileSize {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "offset 超出文件大小"}, http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// 截断 length 使其不超过文件剩余长度
	if offset+length > fileSize {
		length = fileSize - offset
	}

	// 防止 length 过大导致 OOM（限制到 maxChunkHashBuf = 8 MiB）
	if length > maxChunkHashBuf {
		length = maxChunkHashBuf
	}

	file, err := os.Open(filePath)
	if err != nil {
		h.logger.Error("打开文件失败", "error", err, "filename", filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "打开文件失败"}, http.StatusInternalServerError)
		return
	}
	defer file.Close()

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		h.logger.Error("文件 seek 失败", "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件读取失败"}, http.StatusInternalServerError)
		return
	}

	// 设置响应头（length 已截断完毕）
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, fileSize))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))

	// 如果 ChecksumStore 有记录，返回完整文件 checksum
	if cs, ok := h.checksumStore.Get(filename); ok {
		w.Header().Set("X-File-Checksum", cs)
	}

	// 计算本块 SHA-256：先读入缓冲区，计算 hash，再写入 ResponseWriter
	// 缓冲区最大 maxChunkHashBuf，避免 OOM
	if length > maxChunkHashBuf {
		length = maxChunkHashBuf
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(file, data); err != nil {
		// 文件可能被截断或读取到末尾
		// 回退到流式读取
		_ = file.Close()
		file2, openErr := os.Open(filePath)
		if openErr != nil {
			h.logger.Error("重新打开文件失败", "error", openErr)
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件读取失败"}, http.StatusInternalServerError)
			return
		}
		defer file2.Close()
		file2.Seek(offset, io.SeekStart)

		hasher := sha256.New()
		limitedReader := io.LimitReader(file2, length)
		multiWriter := io.MultiWriter(w, hasher)
		if _, copyErr := io.CopyN(multiWriter, limitedReader, length); copyErr != nil {
			h.logger.Error("流式读取文件失败", "error", copyErr)
			return
		}
		w.Header().Set("X-Chunk-Checksum", hex.EncodeToString(hasher.Sum(nil)))
		return
	}

	// 计算 hash
	chunkHash := sha256.Sum256(data)
	w.Header().Set("X-Chunk-Checksum", hex.EncodeToString(chunkHash[:]))

	// 写入响应
	w.WriteHeader(http.StatusOK)
	_, writeErr := w.Write(data)
	if writeErr != nil {
		h.logger.Warn("写入分块响应失败", "error", writeErr)
	}
}
