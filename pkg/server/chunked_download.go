// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/cocomhub/sproxy/internal/size"
)

// parseChunkRange 从查询参数中解析 offset 和 length。
// 返回解析后的 offset、length 和是否解析成功的标志。
func parseChunkRange(r *http.Request, cfg *Config) (offset, length int64, ok bool) {
	offset = int64(0)
	length = cfg.ChunkSize
	if length <= 0 {
		length = size.DefaultChunkSize
	}

	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		parsed, err := strconv.ParseInt(offsetStr, 10, 64)
		if err != nil || parsed < 0 {
			return 0, 0, false
		}
		offset = parsed
	}
	if lengthStr := r.URL.Query().Get("length"); lengthStr != "" {
		parsed, err := strconv.ParseInt(lengthStr, 10, 64)
		if err != nil || parsed <= 0 {
			return 0, 0, false
		}
		length = min(parsed, size.MaxChunkHashBuf)
	}
	return offset, length, true
}

// seekAndReadFile 打开文件、seek 到指定偏移、读取指定长度的数据。
// 返回数据内容和其 SHA-256 checksum。
func (h *Handlers) seekAndReadFile(filePath string, offset, length int64) (data []byte, checksum string, err error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()

	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		h.logger.Error("文件 seek 失败", "error", err)
		return nil, "", err
	}

	// 读入缓冲区并计算 hash
	data = make([]byte, length)
	if _, err := io.ReadFull(file, data); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("读取分块数据失败: %w", err)
	}

	chunkHash := sha256.Sum256(data)
	return data, hex.EncodeToString(chunkHash[:]), nil
}

// setChunkResponseHeaders 设置分块下载的响应头。
func setChunkResponseHeaders(w http.ResponseWriter, filename string, offset, length, fileSize int64) {
	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, fileSize))
	w.Header().Set("Content-Disposition", formatContentDisposition(filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
}

// downloadChunk 下载文件的指定分块。
//
// 参数：
//   - filename: 文件名（普通下载经 ValidateFilePath 校验；kind=cloud_archive 时为归档名）
//   - kind: 可选（cloud_archive 走归档目录拼接）
//   - offset: 起始偏移量（默认 0）
//   - length: 分块长度（默认 4 MiB）
//
// 响应头：
//   - Content-Range: bytes offset-(offset+length-1)/fileSize
//   - X-Chunk-Checksum: 本块的 SHA-256
//   - X-File-Checksum: 完整文件的 SHA-256（若 ChecksumStore 有记录）
func (h *Handlers) downloadChunk(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	filename, filePath, err := h.resolveDownloadPath(r)
	if err != nil {
		writeDownloadPathError(w, err)
		return
	}

	// 解析 offset 和 length
	offset, length, ok := parseChunkRange(r, cfg)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的 offset 或 length"}, http.StatusBadRequest)
		return
	}

	stat, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "访问文件失败"}, http.StatusInternalServerError)
		}
		return
	}

	fileSize := stat.Size()
	if offset >= fileSize {
		if fileSize == 0 && offset == 0 {
			// 空文件：返回 200 和 0 字节
			setChunkResponseHeaders(w, filename, 0, 0, 0)
			w.WriteHeader(http.StatusOK)
			return
		}
		sendJSONResponse(w, UploadResponse{Success: false, Message: "offset 超出文件大小"}, http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// 截断 length 使其不超过文件剩余长度和保护上限
	if offset+length > fileSize {
		length = fileSize - offset
	}
	if length > size.MaxChunkHashBuf {
		length = size.MaxChunkHashBuf
	}

	// 读取文件数据（含 seek 和重试回退）
	data, serverChecksum, err := h.seekAndReadFile(filePath, offset, length)
	if err != nil {
		h.logger.Error(errMsgOpenFileFailed, "error", err, "file_name", filename)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileReadFailed}, http.StatusInternalServerError)
		return
	}

	// 设置响应头
	setChunkResponseHeaders(w, filename, offset, length, fileSize)

	// 如果 ChecksumStore 有记录，返回完整文件 checksum（owner 作用域 key）
	if cs, ok := h.checksumStore.Get(h.checksumKeyFor(r, filename)); ok {
		w.Header().Set(headerFileChecksum, cs)
	}

	// 写入响应
	w.Header().Set("X-Chunk-Checksum", serverChecksum)
	w.WriteHeader(http.StatusOK)
	n, writeErr := w.Write(data)
	if writeErr != nil {
		h.logger.Warn("写入分块响应失败", "error", writeErr)
	}
	if writeErr == nil && h.metrics != nil {
		h.metrics.RecordDownload(int64(n))
	}
}
