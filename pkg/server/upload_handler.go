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
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
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
// checksum 写入 per-tenant store，key = 租户根内相对路径 rel（无 owner 前缀）。
func (h *Handlers) setUploadResponseHeaders(w http.ResponseWriter, r *http.Request, root *storage.Root, remotePath, rel, serverChecksum string, logger *slog.Logger) {
	w.Header().Set(headerFileChecksum, serverChecksum)
	if cs := h.checksumStoreFor(ownerFromRequest(r)); cs != nil {
		cs.Set(rel, serverChecksum)
	} else {
		logger.WarnContext(r.Context(), "per-tenant checksum store 不可用，跳过记录", "file_name", remotePath)
	}

	// 处理文件修改时间
	if mtimeStr := r.Header.Get(headerFileMTime); mtimeStr != "" {
		mtimeInt, err := strconv.ParseInt(mtimeStr, 10, 64)
		if err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := root.Chtimes(rel, modTime, modTime); err != nil {
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
	remotePath, rel, ok := h.resolveFilePath(w, r, remotePathStr)
	if !ok {
		return
	}
	logger.DebugContext(r.Context(), "上传路径", "remote_path", remotePath, "header", r.Header.Get("X-File-Path"), "multipart", handler.Filename)

	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	root := tnt.Root()

	if err := root.MkdirAll(filepath.Dir(rel), 0755); err != nil {
		logger.ErrorContext(r.Context(), "创建目录失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	// 并发上传防护：防止同一文件被多个上传请求同时写入导致 OOM。
	// key 带租户前缀（server 级共享 map，防跨租户同 rel 碰撞）。
	upKey := tnt.ID + "\x00" + rel
	if _, loaded := h.uploadingFiles.LoadOrStore(upKey, "upload"); loaded {
		logger.WarnContext(r.Context(), "文件正在上传中，拒绝并发上传", "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在上传中"}, http.StatusConflict)
		return
	}
	defer h.uploadingFiles.Delete(upKey)

	// 重复检测与版本管理
	if h.handleDuplicateFile(w, r, root, rel, expectedChecksum, remotePath) {
		return
	}

	// 配额预留（P4）：覆盖写场景先统计旧文件大小 prev（Adjust 差分用），
	// 随后 TryReserve(handler.Size) 预留新文件空间；写入成功且校验通过后
	// Commit(实际写入字节数) / Adjust(prev, next)；写入/校验失败 Release()。
	// 用户上传文件落 user 桶，配额按 user 桶子 Scope 归集（父链聚合到租户 Scope 与 globalPool）。
	scope := h.quotaBucketFor(ownerFromRequest(r), "user")
	prev := int64(0)
	var res *quota.Reservation
	if scope != nil {
		if stat, statErr := root.Stat(rel); statErr == nil {
			prev = stat.Size()
		}
		rr, reserveErr := scope.TryReserve(handler.Size)
		if reserveErr != nil {
			logger.WarnContext(r.Context(), "存储配额不足，拒绝上传", "file_name", remotePath, "owner", ownerFromRequest(r))
			sendJSONResponse(w, UploadResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
			return
		}
		res = rr
	}

	// 原子写入 + 流式哈希
	serverChecksum, written, err := writeFileAtomicallyRoot(r.Context(), root, rel, file)
	if err != nil {
		if res != nil {
			res.Release()
		}
		logger.ErrorContext(r.Context(), "保存文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
		return
	}

	if serverChecksum != expectedChecksum {
		// 清理已写入的校验失败文件，忽略错误（临时文件由 writeFileAtomicallyRoot 清理）
		_ = root.Remove(rel)
		if res != nil {
			res.Release()
		}
		logger.WarnContext(r.Context(), "文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	// 配额对账：覆盖写 Adjust(prev, next)（旧文件已占用 prev，差分后收敛到新大小，
	// 不经过 reserved）；新文件 Commit(written)。
	if res != nil {
		if prev > 0 {
			scope.Adjust(prev, written)
			res.Release()
		} else {
			res.Commit(written)
		}
	}

	h.setUploadResponseHeaders(w, r, root, remotePath, rel, serverChecksum, logger)

	sendJSONResponse(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件上传成功, size: %d", handler.Size),
		Checksum: serverChecksum,
	}, http.StatusOK)
	if h.metrics != nil {
		h.metrics.RecordUpload(handler.Size)
	}
}

// writeFileAtomically 将 src 原子写入 dstPath（绝对路径），同时计算 SHA-256 哈希。
// 先写到唯一临时文件，再 os.Rename，防止部分写入与并发冲突。
// 保留供 chunked_upload 使用（任务 12 迁移到 Tenant chunk 桶后移除）；upload 链路用 writeFileAtomicallyRoot。
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

// writeFileAtomicallyRoot 将 src 原子写入租户根内 rel 路径，同时计算 SHA-256 哈希。
// 在目标同目录创建唯一临时文件（root.OpenFile O_EXCL），写入完成后 root.Rename
// 原子替换，防止部分写入与并发冲突。全程 root 相对，不派生绝对路径（防符号链接逃逸）。
func writeFileAtomicallyRoot(ctx context.Context, root *storage.Root, rel string, src io.Reader) (checksum string, written int64, err error) {
	dir := filepath.Dir(rel)
	base := filepath.Base(rel)
	tmpRel := filepath.Join(dir, base+".tmp."+fmt.Sprintf("%d", time.Now().UnixNano()))
	tmpFile, err := root.OpenFile(tmpRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件失败: %w", err)
	}
	defer func() { _ = root.Remove(tmpRel) }()

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
	if err := atomicRenameRoot(root, tmpRel, rel); err != nil {
		return checksum, written, fmt.Errorf("重命名临时文件失败: %w", err)
	}
	return checksum, written, nil
}

// atomicRenameRoot 在 storage.Root 内原子重命名 srcRel → dstRel。
// 与 atomicRename 对齐：快速路径直接 Rename，失败（Windows 并发场景）先删除目标
// 再重命名，并使用短退避重试以应对 Windows 句柄释放延迟。
func atomicRenameRoot(root *storage.Root, srcRel, dstRel string) error {
	// 快速路径：直接重命名
	if err := root.Rename(srcRel, dstRel); err == nil {
		return nil
	}
	// 慢速路径：删除目标文件，然后重命名临时文件
	// 使用短退避重试，解决 Windows 上并发 Rename 导致的"Access is denied"
	const maxAttempts = 5
	const baseDelay = 2 * time.Millisecond
	for i := range maxAttempts {
		_ = root.Remove(dstRel)
		if err := root.Rename(srcRel, dstRel); err == nil {
			return nil
		} else if i == maxAttempts-1 {
			return fmt.Errorf("重命名失败（已达最大重试次数 %d）: %w", maxAttempts, err)
		}
		time.Sleep(baseDelay << i)
	}
	return nil
}

// resolveFilePath 校验 filename 并映射到请求者租户的 user 桶相对路径。
// 返回已验证的协议路径（remotePath）与租户根内相对路径（rel，如 user/dir/f.txt）。
// 校验失败时返回 false。
func (h *Handlers) resolveFilePath(w http.ResponseWriter, r *http.Request, filename string) (remotePath, rel string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return "", "", false
	}
	// 写入侧守卫（审查 #4 收敛）：用户显式上传/重命名不得落到服务端内部目录
	// （.__cloud__ 等，它们只可经白名单 kind 或 sync 任务可达）；ValidateFilePath
	// 全局不再拒绝 .__ 首段（避免破坏 sync push 的本地根 .__ 前缀文件）。
	if isInternalDirPathPrefix(remotePath) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能访问服务端内部目录（.__ 前缀为服务端保留）"}, http.StatusBadRequest)
		return "", "", false
	}
	tnt := h.tenantOf(r)
	if tnt == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	rel, ok = tnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, rel, true
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
// 返回 true 表示已处理（调用方应 return）。root 为请求者租户根，rel 为租户根内相对路径。
func (h *Handlers) handleDuplicateFile(w http.ResponseWriter, r *http.Request, root *storage.Root, rel, expectedChecksum, remotePath string) bool {
	ctx := r.Context()
	stat, statErr := root.Stat(rel)
	if statErr != nil {
		return false // 文件不存在，继续正常上传
	}
	if verifyFileWithChecksumRoot(root, rel, expectedChecksum) {
		// 幂等上传：文件已存在且 checksum 匹配，直接返回成功（不保存版本）
		w.Header().Set(headerFileChecksum, expectedChecksum)
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
		return true
	}
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.Enabled {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本
		h.saveVersionBeforeOverwrite(r, remotePath)
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
	if serverCS, csErr := FileChecksumRoot(root, rel); csErr == nil {
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
