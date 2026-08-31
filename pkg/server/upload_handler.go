// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
)

// hashPool 复用 SHA-256 hash 对象，减少每次上传的分配。
var hashPool = sync.Pool{
	New: func() any { return sha256.New() },
}

// parseUploadMultipart 解析上传请求的 multipart 表单，返回文件、文件信息、期望的 checksum 和错误。
func (h *Handlers) parseUploadMultipart(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (file multipart.File, handler *multipart.FileHeader, expectedChecksum string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, size.UploadBodyLimit)
	if err := r.ParseMultipartForm(size.MultipartBufSize); err != nil {
		logger.WarnContext(r.Context(), "解析 multipart 失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体过大或解析失败"}, http.StatusRequestEntityTooLarge)
		return nil, nil, "", false
	}
	// I-3：multipart 解析不读到 EOF，读完全部 body 触发 bodyValidator 哈希校验。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return nil, nil, "", false
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		logger.ErrorContext(r.Context(), "读取文件失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return nil, nil, "", false
	}

	expectedChecksum = r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		file.Close()
		logger.WarnContext(r.Context(), "缺少 X-File-Checksum 请求头")
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
		mtimeInt, err := strconv.ParseInt(mtimeStr, 10, 64)
		if err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := os.Chtimes(filePath, modTime, modTime); err != nil {
				logger.WarnContext(r.Context(), "设置文件时间戳失败", "file_name", remotePath, "error", err)
			}
		}
	}
}

func (h *Handlers) upload(w http.ResponseWriter, r *http.Request) {
	logger := h.logger

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
	logger.DebugContext(r.Context(), "上传路径", "remote_path", remotePath, "header", r.Header.Get("X-File-Path"), "multipart", handler.Filename)

	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		logger.ErrorContext(r.Context(), "创建目录失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	// 并发上传防护：防止同一文件被多个上传请求同时写入导致 OOM
	if _, loaded := h.uploadingFiles.LoadOrStore(remotePath, "upload"); loaded {
		logger.WarnContext(r.Context(), "文件正在上传中，拒绝并发上传", "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在上传中"}, http.StatusConflict)
		return
	}
	defer h.uploadingFiles.Delete(remotePath)

	// 重复检测与版本管理
	if h.handleDuplicateFile(w, r.Context(), filePath, expectedChecksum, remotePath) {
		return
	}

	// 原子写入 + 流式哈希
	serverChecksum, _, err := writeFileAtomically(r.Context(), filePath, file)
	if err != nil {
		logger.ErrorContext(r.Context(), "保存文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
		return
	}

	if serverChecksum != expectedChecksum {
		// 清理已写入的校验失败文件，忽略错误（临时文件由 writeFileAtomically 清理）
		_ = os.Remove(filePath)
		logger.WarnContext(r.Context(), "文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "file_name", remotePath)
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
func writeFileAtomically(ctx context.Context, dstPath string, src io.Reader) (checksum string, written int64, err error) {
	tmpFile, err := os.CreateTemp(filepath.Dir(dstPath), filepath.Base(dstPath)+".tmp.*")
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	h, ok := hashPool.Get().(hash.Hash)
	if !ok {
		return "", 0, fmt.Errorf("hashPool 返回非 hash.Hash 类型")
	}
	hash := h
	hash.Reset()
	defer hashPool.Put(hash)
	mw := io.MultiWriter(tmpFile, hash)
	written, err = copyWithContext(mw, src, ctx)
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
		sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return "", "", false
	}
	fullPath = h.safePath(remotePath)
	if fullPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, fullPath, true
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
// 返回 true 表示已处理（调用方应 return）。
func (h *Handlers) handleDuplicateFile(w http.ResponseWriter, ctx context.Context, filePath, expectedChecksum, remotePath string) bool {
	stat, statErr := os.Stat(filePath)
	if statErr != nil {
		return false // 文件不存在，继续正常上传
	}
	if verifyFileWithChecksum(filePath, expectedChecksum) {
		// 幂等上传：文件已存在且 checksum 匹配，直接返回成功（不保存版本）
		w.Header().Set(headerFileChecksum, expectedChecksum)
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
		return true
	}
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.Enabled {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本
		h.saveVersionBeforeOverwrite(remotePath)
		// 审查 I-3：覆盖动作记审计（含旧版本已保存的信息）。
		h.RecordAudit(ctx, AuditEvent{
			Action: "overwrite", ObjectType: "file", Object: remotePath,
			Result: AuditResultSuccess, Detail: "覆盖现有文件（版本已保存）",
		})
		return false // 继续执行写入流程，用新内容覆盖现有文件
	}
	// checksum 不匹配：冲突，需保留现有文件
	h.logger.WarnContext(ctx, "文件已存在，但校验失败", "file_name", remotePath)
	// 审查 I-3：versioning 关闭时同名覆盖是静默数据丢失，记审计（当前走冲突拒绝
	// 分支——保留现有文件，不覆盖；此处为 denied 留痕）。
	h.RecordAudit(ctx, AuditEvent{
		Action: "overwrite", ObjectType: "file", Object: remotePath,
		Result: AuditResultDenied, Detail: "文件已存在且 checksum 不匹配（versioning 关闭，拒绝覆盖）",
	})
	// 附带服务端文件的实际 checksum，方便客户端决策
	if serverCS, csErr := FileChecksum(filePath); csErr == nil {
		sendJSONResponse(w, UploadResponse{
			Success:  false,
			Message:  "文件已存在，但校验失败",
			Checksum: serverCS,
		}, http.StatusConflict)
	} else {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件已存在，但校验失败"}, http.StatusConflict)
	}
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
		} else if i == maxAttempts-1 {
			return fmt.Errorf("重命名失败（已达最大重试次数 %d）: %w", maxAttempts, err)
		}
		time.Sleep(baseDelay << i)
	}
	return nil
}

// copyWithContext 是 context-aware 的 io.Copy，每次 Read/Write 前检查 ctx.Done()。
func copyWithContext(w io.Writer, r io.Reader, ctx context.Context) (int64, error) {
	var total int64
	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, err := r.Read(buf)
		if n > 0 {
			nn, werr := w.Write(buf[:n])
			total += int64(nn)
			if werr != nil {
				return total, werr
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
