// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// write.go 是文件服务**写面**的单次上传族（`POST /upload` 处理器及其私有辅助），自
// `pkg/server/upload_handler.go` 整族迁入（接收者由 *Handlers 改为 *Service，方法体只换
// 接收者与限定名）。批量的写面族见 rename.go / delete.go；目录族见 dirs.go。
//
// 留在装配层（pkg/server/upload_handler.go）的部分：`atomicRenameRoot` 与 `copyWithContext`
// ——它们在 pkg/server 侧另有消费者（跨卷 move：volumes_api.go），且本包已有语义等价的
// 本地实现（atomicRenameRoot 在 service.go、copyWithContext 在本文件），故按「跨族共享的
// 纯函数」处置：装配层保留一份，本包持等价实现。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	"github.com/cocomhub/sproxy/pkg/storage"
)

// hashPool 复用 SHA-256 hash 对象，减少每次上传的分配。
var hashPool = sync.Pool{
	New: func() any { return sha256.New() },
}

// headerVolume 是上传成功响应头：落盘目标卷名（多卷路由断言/客户端定位用；单卷向后兼容）。
const headerVolume = "X-Volume"

// uploadingLockUpload 是 FileLocks 锁池中「单次（非分块）上传」条目的值标记。
// 与 pkg/server.uploadingLockUpload 同值——该字面量是**跨层值契约**：装配层的
// isUploadingLockMarker 按它判定「条目是锁标记而非 upload_id」并在过期清理时跳过。值一旦
// 不被识别，长上传（>10 分钟）的锁条目会被当成过期会话删除，同 rel 并发上传重新放行。
// 两侧字面量相等由 `pkg/server/helper_impl_drift_test.go` 的源码级断言守卫。
const uploadingLockUpload = "upload"

// errMsgMissingChecksum 是缺少 X-File-Checksum 请求头时的响应文案（随写面自
// pkg/server/errors.go 迁入；该常量在 pkg/server 已无其他使用者）。
const errMsgMissingChecksum = "缺少 X-File-Checksum 请求头"

// parseUploadMultipart 解析上传请求的 multipart 表单，返回文件、文件信息、期望的 checksum 和错误。
func (s *Service) parseUploadMultipart(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (file multipart.File, handler *multipart.FileHeader, expectedChecksum string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, size.UploadBodyLimit)
	//nolint:gosec // G120 误报：请求体已由上一行 http.MaxBytesReader 限定为 size.UploadBodyLimit
	if err := r.ParseMultipartForm(size.MultipartBufSize); err != nil {
		logger.WarnContext(r.Context(), "解析 multipart 失败", "error", err.Error())
		s.sendJSON(w, UploadResponse{Success: false, Message: "请求体过大或解析失败"}, http.StatusRequestEntityTooLarge)
		return nil, nil, "", false
	}
	// I-3：multipart 解析不读到 EOF，读完全部 body 触发 bodyValidator 哈希校验。
	if err := drainAndVerifyBody(r); err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return nil, nil, "", false
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		logger.ErrorContext(r.Context(), "读取文件失败", "error", err.Error())
		s.sendJSON(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return nil, nil, "", false
	}

	expectedChecksum = r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		file.Close()
		logger.WarnContext(r.Context(), "缺少 X-File-Checksum 请求头")
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		return nil, nil, "", false
	}
	return file, handler, expectedChecksum, true
}

// Upload 处理 POST /upload。
// 多卷（T4/T5）：卷路由选目标卷（ACL/placement/容量），成功响应头 X-Volume 标识落盘卷；
// 覆盖写 stay-home 到 home 卷（见下方注释）。
func (s *Service) Upload(w http.ResponseWriter, r *http.Request) {
	logger := s.rt.logger()

	file, handler, expectedChecksum, ok := s.parseUploadMultipart(w, r, logger)
	if !ok {
		return
	}
	defer func() { _ = file.Close() }()

	// 路径来源：优先 X-File-Path 头（保留子目录），回退 multipart 文件名。
	remotePathStr := r.Header.Get("X-File-Path")
	if remotePathStr == "" {
		remotePathStr = handler.Filename
	}

	res, err := s.WriteFile(r.Context(), WriteFileInput{
		Owner:            s.rt.actorOf(r),
		RemotePath:       remotePathStr,
		ExplicitVol:      r.FormValue("volume"),
		ExpectedChecksum: expectedChecksum,
		ClientSize:       handler.Size,
		Mtime:            parseMTimeHeader(r),
	}, file)
	if err != nil {
		var he *HTTPError
		if errors.As(err, &he) {
			// 上传冲突会附带服务端实际 checksum（历史契约：方便客户端决策）。
			s.sendJSON(w, UploadResponse{Success: false, Message: he.Message, Checksum: he.Checksum}, he.Status)
			return
		}
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
		return
	}

	if res.VolumeName != "" {
		w.Header().Set(headerVolume, res.VolumeName)
	}
	w.Header().Set(headerFileChecksum, res.Checksum)
	s.sendJSON(w, UploadResponse{Success: true, Message: res.Message, Checksum: res.Checksum}, http.StatusOK)
	if s.rt.metricsRecorder() != nil {
		s.rt.metricsRecorder().RecordUpload(handler.Size)
	}
}

// parseMTimeHeader 解析 X-File-MTime（UnixNano）；缺失/非法/非正数返回 0（不设置）。
func parseMTimeHeader(r *http.Request) int64 {
	mtimeStr := r.Header.Get(headerFileMTime)
	if mtimeStr == "" {
		return 0
	}
	mtime, err := strconv.ParseInt(mtimeStr, 10, 64)
	if err != nil || mtime <= 0 {
		return 0
	}
	return mtime
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

// copyWithContext 是 context-aware 的 io.Copy，每次 Read/Write 前检查 ctx.Done()。
//
// 本函数自 pkg/server/upload_handler.go **原样下沉**（函数体逐字未改）：它不含 Handlers
// 私有状态，属纯计算（按接缝判据不进接缝）。pkg/server 侧的同名实现另有消费者
// （跨卷 move：volumes_api.go 与 share.go），故两侧各留一份，等价性由
// `pkg/server/helper_impl_drift_test.go` 的源码级断言守卫。
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
