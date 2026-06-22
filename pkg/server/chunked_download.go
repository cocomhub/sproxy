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
		if _, err := fmt.Sscanf(offsetStr, "%d", &offset); err != nil || offset < 0 {
			return 0, 0, false
		}
	}
	if lengthStr := r.URL.Query().Get("length"); lengthStr != "" {
		if _, err := fmt.Sscanf(lengthStr, "%d", &length); err != nil || length <= 0 {
			return 0, 0, false
		}
		if length > size.MaxChunkHashBuf {
			length = size.MaxChunkHashBuf
		}
	}
	return offset, length, true
}

// seekAndReadFile 打开文件、seek 到指定偏移、读取指定长度的数据。
// 如果第一次读取失败（io.ReadFull 返回非预期错误），尝试重新打开文件重试。
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
		// 文件可能被截断或读取到末尾，回退到缓冲区读取
		return h.seekAndReadFileWithRetry(filePath, offset, length)
	}

	chunkHash := sha256.Sum256(data)
	return data, hex.EncodeToString(chunkHash[:]), nil
}

// seekAndReadFileWithRetry 重试打开文件并读取，用于首次读取失败时的回退。
func (h *Handlers) seekAndReadFileWithRetry(filePath string, offset, length int64) ([]byte, string, error) {
	file2, openErr := os.Open(filePath)
	if openErr != nil {
		h.logger.Error("重新打开文件失败", "error", openErr)
		return nil, "", openErr
	}
	defer file2.Close()

	if _, seekErr := file2.Seek(offset, io.SeekStart); seekErr != nil {
		h.logger.Error("文件 seek 失败", "error", seekErr)
		return nil, "", seekErr
	}

	data := make([]byte, length)
	if _, readErr := io.ReadFull(file2, data); readErr != nil {
		h.logger.Error("读取文件失败", "error", readErr)
		return nil, "", readErr
	}

	chunkHash := sha256.Sum256(data)
	return data, hex.EncodeToString(chunkHash[:]), nil
}

// setChunkResponseHeaders 设置分块下载的响应头。
func setChunkResponseHeaders(w http.ResponseWriter, filename string, offset, length, fileSize int64) {
	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, fileSize))
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", length))
}

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
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgEmptyFilename}, http.StatusBadRequest)
		return
	}
	if _, err := ValidateFilePath(filename); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	// 解析 offset 和 length
	offset, length, ok := parseChunkRange(r, cfg)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的 offset 或 length"}, http.StatusBadRequest)
		return
	}

	filePath := h.safePath(filename)
	if filePath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
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

	// 如果 ChecksumStore 有记录，返回完整文件 checksum
	if cs, ok := h.checksumStore.Get(filename); ok {
		w.Header().Set(headerFileChecksum, cs)
	}

	// 写入响应
	w.Header().Set("X-Chunk-Checksum", serverChecksum)
	w.WriteHeader(http.StatusOK)
	_, writeErr := w.Write(data)
	if writeErr != nil {
		h.logger.Warn("写入分块响应失败", "error", writeErr)
	}
}
