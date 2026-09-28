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
	"github.com/cocomhub/sproxy/pkg/storage"
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
// 入参 file 为 *os.File（非加密卷）或解密流（加密卷，见 DownloadChunk）。
func (s *Service) seekAndReadFile(file io.ReadSeeker, offset, length int64) (data []byte, checksum string, err error) {
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		s.rt.logger().Error("文件 seek 失败", "error", err)
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

// readFromStream 从已 Discard 到目标偏移的解密流读取 length 字节并计算 SHA-256。
// 加密卷分块下载专用（解密流无任意 Seek）。
func (s *Service) readFromStream(r io.Reader, length int64) (data []byte, checksum string, err error) {
	data = make([]byte, length)
	n, rerr := io.ReadFull(r, data)
	if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
		return nil, "", fmt.Errorf("读取分块数据失败: %w", rerr)
	}
	data = data[:n]
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
	dp, err := s.rt.resolveDownloadPath(r)
	if err != nil {
		s.writeDownloadPathError(w, err)
		return
	}

	// 解析 offset 和 length
	offset, length, ok := parseChunkRange(r, s.rt.chunkSize())
	if !ok {
		s.sendJSON(w, UploadResponse{Success: false, Message: "无效的 offset 或 length"}, http.StatusBadRequest)
		return
	}

	// 所有下载 kind 均经租户根打开（root 相对，防符号链接逃逸）。
	root := dp.Tenant.Root()
	encrypted := root.IsEncrypted()
	file, fcloser, ok := s.openChunkSource(w, root, dp, encrypted, offset)
	if !ok {
		return
	}
	defer func() {
		if fcloser != nil {
			_ = fcloser.Close()
		}
	}()

	length, fileSize, ok := s.chunkReadParams(w, root, dp, offset, length)
	if !ok {
		return
	}

	data, serverChecksum, ok := s.readChunkData(w, dp, file, encrypted, offset, length)
	if !ok {
		return
	}

	s.writeChunkSuccess(w, r, dp, offset, length, fileSize, serverChecksum, data)
}

// openChunkSource 按是否加密卷打开可随机读的数据源并错误回包。
// 加密卷走 OpenDecrypted 解密流（丢弃 offset 字节 + Seek 重开），普通卷直接 Open。
// 失败时已写好 JSON 响应，返回 ok=false。
func (s *Service) openChunkSource(w http.ResponseWriter, root *storage.Root, dp DownloadPath, encrypted bool, offset int64) (io.ReadSeeker, io.Closer, bool) {
	if encrypted {
		return s.openEncryptedChunkSource(w, root, dp, offset)
	}
	return s.openPlainChunkSource(w, root, dp)
}

// openEncryptedChunkSource 打开加密卷解密流并丢弃 offset 字节（解密流无任意 Seek）。
func (s *Service) openEncryptedChunkSource(w http.ResponseWriter, root *storage.Root, dp DownloadPath, offset int64) (io.ReadSeeker, io.Closer, bool) {
	rc, oerr := root.OpenDecrypted(dp.Rel)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			s.sendJSON(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			s.sendJSON(w, UploadResponse{Success: false, Message: errMsgAccessFile}, http.StatusInternalServerError)
		}
		return nil, nil, false
	}
	// 解密流无任意 Seek：Discard offset 字节（O(offset)）。
	if _, derr := io.CopyN(io.Discard, rc, offset); derr != nil {
		_ = rc.Close()
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgAccessFile}, http.StatusInternalServerError)
		return nil, nil, false
	}
	// encReadCloser 实现 Seek（仅 0 重开），满足 io.ReadSeeker 接口。
	rs, ok := rc.(io.ReadSeeker)
	if !ok {
		_ = rc.Close()
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgAccessFile}, http.StatusInternalServerError)
		return nil, nil, false
	}
	return rs, rc, true
}

// openPlainChunkSource 打开普通（非加密）卷文件句柄。
func (s *Service) openPlainChunkSource(w http.ResponseWriter, root *storage.Root, dp DownloadPath) (io.ReadSeeker, io.Closer, bool) {
	f, oerr := root.Open(dp.Rel)
	if oerr != nil {
		if os.IsNotExist(oerr) {
			s.sendJSON(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			s.sendJSON(w, UploadResponse{Success: false, Message: errMsgAccessFile}, http.StatusInternalServerError)
		}
		return nil, nil, false
	}
	return f, f, true
}

// chunkReadParams 校验并截断分块边界：加密卷解密流无 Stat，取密文大小做越界判断；
// 空文件（size 0 且 offset 0）返回 200 + 0 字节；offset 越界回 416；length 截断到
// 文件剩余长度与保护上限。失败时已写好响应，返回 ok=false。
func (s *Service) chunkReadParams(w http.ResponseWriter, root *storage.Root, dp DownloadPath, offset, length int64) (int64, int64, bool) {
	var fileSize int64
	if st, serr := root.Stat(dp.Rel); serr == nil {
		fileSize = st.Size()
	}
	if offset >= fileSize {
		if fileSize == 0 && offset == 0 {
			// 空文件：返回 200 和 0 字节
			setChunkResponseHeaders(w, dp.Filename, 0, 0, 0)
			w.WriteHeader(http.StatusOK)
			return 0, 0, false
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: "offset 超出文件大小"}, http.StatusRequestedRangeNotSatisfiable)
		return 0, 0, false
	}
	// 截断 length 使其不超过文件剩余长度和保护上限
	if offset+length > fileSize {
		length = fileSize - offset
	}
	if length > size.MaxChunkHashBuf {
		length = size.MaxChunkHashBuf
	}
	return length, fileSize, true
}

// readChunkData 读取分块数据（加密卷走解密流、普通卷走 seek+读），失败回包 JSON。
func (s *Service) readChunkData(w http.ResponseWriter, dp DownloadPath, file io.ReadSeeker, encrypted bool, offset, length int64) ([]byte, string, bool) {
	var data []byte
	var serverChecksum string
	var err error
	if encrypted {
		data, serverChecksum, err = s.readFromStream(file, length)
	} else {
		data, serverChecksum, err = s.seekAndReadFile(file, offset, length)
	}
	if err != nil {
		s.rt.logger().Error(errMsgOpenFileFailed, "error", err, "file_name", dp.Filename)
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgFileReadFailed}, http.StatusInternalServerError)
		return nil, "", false
	}
	return data, serverChecksum, true
}

// writeChunkSuccess 写分块下载成功响应：响应头 + 完整文件 checksum + 数据 + 计量。
func (s *Service) writeChunkSuccess(w http.ResponseWriter, r *http.Request, dp DownloadPath, offset, length, fileSize int64, serverChecksum string, data []byte) {
	setChunkResponseHeaders(w, dp.Filename, offset, length, fileSize)
	// 如果 ChecksumStore 有记录，返回完整文件 checksum（per-tenant + 根内相对 key）
	if csStore, csKey := s.checksumStoreForRead(dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			w.Header().Set(headerFileChecksum, cs)
		}
	}
	w.Header().Set("X-Chunk-Checksum", serverChecksum)
	w.WriteHeader(http.StatusOK)
	n, writeErr := w.Write(data)
	if writeErr != nil {
		s.rt.logger().Warn("写入分块响应失败", "error", writeErr)
	}
	if writeErr == nil && s.rt.metricsRecorder() != nil {
		s.rt.metricsRecorder().RecordDownload(int64(n))
		// 计量报告归属 owner 维度（roadmap 11.10-⑩）：成功分块下载按请求主体记 per-owner 字节。
		if owner := s.rt.actorOf(r); owner != "" {
			s.rt.metricsRecorder().RecordDownloadForOwner(owner, int64(n))
		}
	}
}

// writeDownloadPathError 把 ResolveDownloadPath 返回的错误映射为统一 JSON 响应。
// 与 pkg/server.writeDownloadPathError 语义一致：带 HTTP 状态码的解析错误（装配层经
// HTTPError 传入，对应 pkg/server 的 *downloadPathError）按其状态码与文案回包；其余一律
// 400 + 无效文件名。
func (s *Service) writeDownloadPathError(w http.ResponseWriter, err error) {
	if he, ok := errors.AsType[*HTTPError](err); ok {
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
	cs := s.rt.checksumStore(dp.Tenant.ID)
	if cs == nil {
		return nil, ""
	}
	return cs, dp.Rel
}
