// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
)

// parseUploadMultipart 解析上传请求的 multipart 表单，返回文件、文件信息、期望的 checksum 和错误。
func (h *Handlers) parseUploadMultipart(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (file multipart.File, handler *multipart.FileHeader, expectedChecksum string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, size.UploadBodyLimit)
	if err := r.ParseMultipartForm(size.MultipartBufSize); err != nil {
		logger.Warn("解析 multipart 失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体过大或解析失败"}, http.StatusRequestEntityTooLarge)
		return nil, nil, "", false
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		logger.Error("读取文件失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return nil, nil, "", false
	}

	expectedChecksum = r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		file.Close()
		logger.Warn("缺少 X-File-Checksum 请求头")
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		return nil, nil, "", false
	}
	return file, handler, expectedChecksum, true
}

// setUploadResponseHeaders 设置上传成功后的响应头（checksum、mtime）。
func (h *Handlers) setUploadResponseHeaders(w http.ResponseWriter, r *http.Request, remotePath, filePath, serverChecksum string, logger *slog.Logger) {
	w.Header().Set(headerFileChecksum, serverChecksum)
	h.checksumStore.Set(remotePath, serverChecksum)

	// 处理文件修改时间
	if mtimeStr := r.Header.Get(headerFileMTime); mtimeStr != "" {
		var mtimeInt int64
		if _, err := fmt.Sscanf(mtimeStr, "%d", &mtimeInt); err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := os.Chtimes(filePath, modTime, modTime); err != nil {
				logger.Warn("设置文件时间戳失败", "file_name", remotePath, "error", err)
			}
		}
	}
}

func (h *Handlers) upload(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(headerRequestID)
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	file, handler, expectedChecksum, ok := h.parseUploadMultipart(w, r, logger)
	if !ok {
		return
	}
	defer file.Close()

	// 路径校验（支持子目录）
	remotePathStr := r.Header.Get("X-File-Path")
	if remotePathStr == "" {
		remotePathStr = handler.Filename
	}
	remotePath, filePath, ok := h.resolveFilePath(w, remotePathStr)
	if !ok {
		return
	}
	logger.Debug("上传路径", "remote_path", remotePath, "header", r.Header.Get("X-File-Path"), "multipart", handler.Filename)

	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		logger.Error("创建目录失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	// 重复检测与版本管理
	if h.handleDuplicateFile(w, filePath, expectedChecksum, remotePath) {
		return
	}

	// 原子写入 + 流式哈希
	serverChecksum, _, err := writeFileAtomically(filePath, file)
	if err != nil {
		logger.Error("保存文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
		return
	}

	if serverChecksum != expectedChecksum {
		os.Remove(filePath)
		logger.Warn("文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	h.setUploadResponseHeaders(w, r, remotePath, filePath, serverChecksum, logger)

	sendJSONResponse(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件上传成功, size: %d", handler.Size),
		Checksum: serverChecksum,
	}, http.StatusOK)
	if h.metrics != nil {
		h.metrics.RecordUpload(handler.Size)
	}
}

// writeFileAtomically 将 src 原子写入 dstPath，同时计算 SHA-256 哈希。
// 先写到唯一临时文件，再 os.Rename，防止部分写入与并发冲突。
func writeFileAtomically(dstPath string, src io.Reader) (checksum string, written int64, err error) {
	tmpFile, err := os.CreateTemp(filepath.Dir(dstPath), filepath.Base(dstPath)+".tmp.*")
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	hash := sha256.New()
	mw := io.MultiWriter(tmpFile, hash)
	written, err = io.Copy(mw, src)
	if err != nil {
		tmpFile.Close()
		return "", written, fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", written, fmt.Errorf("关闭临时文件失败: %w", err)
	}
	checksum = hex.EncodeToString(hash.Sum(nil))
	if err := atomicRename(tmpPath, dstPath); err != nil {
		return checksum, written, fmt.Errorf("重命名临时文件失败: %w", err)
	}
	return checksum, written, nil
}

// resolveFilePath 校验 filename 并生成安全的 UploadsDir 下完整路径。
// 返回已验证的相对路径和绝对路径。校验失败时返回 false。
func (h *Handlers) resolveFilePath(w http.ResponseWriter, filename string) (remotePath, fullPath string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return "", "", false
	}
	fullPath = h.safePath(remotePath)
	if fullPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, fullPath, true
}

// resolveFilePathHTTP 供非 JSON handler 使用（如 stat 返回普通 http.Error）。
func (h *Handlers) resolveFilePathHTTP(w http.ResponseWriter, filename string) (remotePath, fullPath string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return "", "", false
	}
	fullPath = h.safePath(remotePath)
	if fullPath == "" {
		http.Error(w, "invalid file path", http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, fullPath, true
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
// 返回 true 表示已处理（调用方应 return）。
func (h *Handlers) handleDuplicateFile(w http.ResponseWriter, filePath, expectedChecksum, remotePath string) bool {
	stat, statErr := os.Stat(filePath)
	if statErr != nil {
		return false // 文件不存在，继续正常上传
	}
	if verifyFileWithChecksum(filePath, expectedChecksum) {
		// 幂等上传：文件已存在且 checksum 匹配，先保存版本后返回
		h.saveVersionBeforeOverwrite(remotePath)
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
		return true
	}
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.Enabled {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本
		h.saveVersionBeforeOverwrite(remotePath)
		return false // 继续执行写入流程，用新内容覆盖现有文件
	}
	// checksum 不匹配：冲突，需保留现有文件
	h.logger.Warn("文件已存在，但校验失败", "file_name", remotePath)
	sendJSONResponse(w, UploadResponse{Success: false, Message: "文件已存在，但校验失败"}, http.StatusConflict)
	return true
}

// atomicRename 尝试 os.Rename，如果失败（Windows 并发场景），
// 先删除目标再重命名，并使用短退避重试以应对 Windows 句柄释放延迟。
func atomicRename(src, dst string) error {
	// 快速路径：直接重命名
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 慢速路径：删除目标文件，然后重命名临时文件
	// 使用短退避重试，解决 Windows 上并发 Rename 导致的"Access is denied"
	const maxAttempts = 5
	const baseDelay = 2 * time.Millisecond
	for i := range maxAttempts {
		_ = os.Remove(dst)
		if err := os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(baseDelay << i)
	}
	return os.Rename(src, dst) // 最后一次尝试，返回最终错误
}
