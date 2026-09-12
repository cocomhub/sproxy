// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

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
	"github.com/cocomhub/sproxy/pkg/checksum"
)

// parseChunkRange 从查询参数中解析 offset 和 length。
// 返回解析后的 offset、length 和是否解析成功的标志。
func parseChunkRange(r *http.Request, cfgChunkSize int64) (offset, length int64, ok bool) {
	offset = int64(0)
	length = cfgChunkSize
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

// seekAndReadFile 从已打开的文件句柄 seek 到指定偏移、读取指定长度的数据。
// 返回数据内容和其 SHA-256 checksum。调用方负责打开与关闭文件。
// 普通下载经租户根打开后复用此函数（chunked_download 迁移到 Tenant API）。
func (s *Service) seekAndReadFile(file *os.File, offset, length int64) (data []byte, checksum string, err error) {
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		s.deps.Logger().Error("文件 seek 失败", "error", err)
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

// DownloadChunk 下载文件的指定分块。
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
func (s *Service) DownloadChunk(w http.ResponseWriter, r *http.Request) {
	dp, err := s.deps.ResolveDownloadPath(r)
	if err != nil {
		s.writeDownloadPathError(w, err)
		return
	}

	// 解析 offset 和 length
	offset, length, ok := parseChunkRange(r, s.deps.ChunkSize())
	if !ok {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的 offset 或 length"}, http.StatusBadRequest)
		return
	}

	// 所有下载 kind 均经租户根打开（root 相对，防符号链接逃逸）。
	file, err := dp.Tenant.Root().Open(dp.Rel)
	if err != nil {
		if os.IsNotExist(err) {
			s.sendJSON(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			s.sendJSON(w, UploadResponse{Success: false, Message: "访问文件失败"}, http.StatusInternalServerError)
		}
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		s.deps.Logger().Error("stat 文件失败", "error", err, "file_name", dp.Filename)
		s.sendJSON(w, UploadResponse{Success: false, Message: "访问文件失败"}, http.StatusInternalServerError)
		return
	}

	fileSize := stat.Size()
	if offset >= fileSize {
		if fileSize == 0 && offset == 0 {
			// 空文件：返回 200 和 0 字节
			setChunkResponseHeaders(w, dp.Filename, 0, 0, 0)
			w.WriteHeader(http.StatusOK)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: "offset 超出文件大小"}, http.StatusRequestedRangeNotSatisfiable)
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
	data, serverChecksum, err := s.seekAndReadFile(file, offset, length)
	if err != nil {
		s.deps.Logger().Error(errMsgOpenFileFailed, "error", err, "file_name", dp.Filename)
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgFileReadFailed}, http.StatusInternalServerError)
		return
	}

	// 设置响应头
	setChunkResponseHeaders(w, dp.Filename, offset, length, fileSize)

	// 如果 ChecksumStore 有记录，返回完整文件 checksum（per-tenant + 根内相对 key）
	if csStore, csKey := s.checksumStoreForRead(dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			w.Header().Set(headerFileChecksum, cs)
		}
	}

	// 写入响应
	w.Header().Set("X-Chunk-Checksum", serverChecksum)
	w.WriteHeader(http.StatusOK)
	n, writeErr := w.Write(data)
	if writeErr != nil {
		s.deps.Logger().Warn("写入分块响应失败", "error", writeErr)
	}
	if writeErr == nil && s.deps.Metrics != nil {
		s.deps.Metrics.RecordDownload(int64(n))
	}
}

// writeDownloadPathError 把 ResolveDownloadPath 返回的错误映射为统一 JSON 响应。
// 与 pkg/server.writeDownloadPathError 语义一致：带 HTTP 状态码的解析错误（装配层经
// HTTPError 传入，对应 pkg/server 的 *downloadPathError）按其状态码与文案回包；其余一律
// 400 + 无效文件名。
func (s *Service) writeDownloadPathError(w http.ResponseWriter, err error) {
	var he *HTTPError
	if errors.As(err, &he) {
		s.sendJSON(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
		return
	}
	s.sendJSON(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
}

// checksumStoreForRead 返回分块下载的 checksum 存储与 key。
// 所有下载 kind 均走 per-tenant store + 根内相对路径（无 owner 前缀；store 按 dp.Tenant.ID 取，
// 与写端 ChecksumStoreFor(owner) 一致）。per-tenant store 不可用时返回 nil（调用方跳过
// checksum 响应头）。
//
// 按接缝判据「可派生能力不新增字段」，本方法由 ChecksumStoreFor 派生（原
// pkg/server.checksumStoreForRead 仅做同一件事 + 取 dp.rel）。
func (s *Service) checksumStoreForRead(dp DownloadPath) (checksum.ChecksumStoreIface, string) {
	if dp.Tenant == nil {
		return nil, ""
	}
	cs := s.deps.ChecksumStoreFor(dp.Tenant.ID)
	if cs == nil {
		return nil, ""
	}
	return cs, dp.Rel
}
