// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

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
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// hashPool 复用 SHA-256 hash 对象，减少每次上传的分配。
var hashPool = sync.Pool{
	New: func() any { return sha256.New() },
}

// headerVolume 是上传成功响应头：落盘目标卷名（多卷路由断言/客户端定位用；单卷向后兼容）。
const headerVolume = "X-Volume"

// sendUploadRouteError 把 routeUpload 的 *routeError 映射为 HTTP 响应（403/409/507/400）。
// 非 routeError 视为内部错误（500）。
func (h *Handlers) sendUploadRouteError(w http.ResponseWriter, r *http.Request, remotePath string, err error) {
	var re *routeError
	if errors.As(err, &re) {
		h.logger.WarnContext(r.Context(), "上传卷路由拒绝", "file_name", remotePath, "status", re.status, "reason", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: re.msg}, re.status)
		return
	}
	h.logger.ErrorContext(r.Context(), "上传卷路由失败", "file_name", remotePath, "error", err.Error())
	sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
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

	// 路径校验（支持子目录）。rel 与卷无关（user/<path> 相对各卷租户根），
	// 卷感知只决定文件落到哪一卷的租户根。
	remotePathStr := r.Header.Get("X-File-Path")
	if remotePathStr == "" {
		remotePathStr = handler.Filename
	}
	remotePath, rel, ok := h.resolveFilePath(w, r, remotePathStr)
	if !ok {
		return
	}
	logger.DebugContext(r.Context(), "上传路径", "remote_path", remotePath, "header", r.Header.Get("X-File-Path"), "multipart", handler.Filename)

	owner := ownerFromRequest(r)

	// 并发上传防护：防止同一 owner 同 rel 被多个上传请求同时写入导致 OOM。
	// key = <owner>\x00<rel>（server 级共享 map，防跨租户同 rel 碰撞）。
	upKey := normalizeOwner(owner) + "\x00" + rel
	if _, loaded := h.uploadingFiles.LoadOrStore(upKey, uploadingLockUpload); loaded {
		logger.WarnContext(r.Context(), "文件正在上传中，拒绝并发上传", "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在上传中"}, http.StatusConflict)
		return
	}
	defer h.uploadingFiles.Delete(upKey)

	explicitVol := r.FormValue("volume")

	// 重复检测与版本管理（自动路由）：先做「写前视图定位」确认 rel 的真实 home 卷（F1-A/B），
	// **只在 home 命中时**对其根做幂等/冲突/版本检查——幂等同 checksum 重传在容量/配额检查前
	// 直接 200（不因卷满误拒）；命中 → 覆盖写必须 stay-home 到 home 卷（容量路由只用于新文件，
	// 否则同 rel 跨卷双份 + owner 双计）。显式 volume 不做此检查：其语义是新文件定位，同名已
	// 存在（含同 checksum）一律由 routeUpload 唯一性 409。
	//
	// F-1（AD-6 写侧闭合）：home 以 owner 卷视图为界（locateOwnerFile 只搜 AllowedVolumes）。
	// locate miss（rel 不在 owner 视图任何卷，含「默认卷被 ACL 排除但仍留 owner 遗留」）→
	// 该 rel 不属于 owner 逻辑树，**跳过 dup-check 视为新文件**交容量路由——不得对无权卷的
	// 遗留做版本化覆盖写（泄漏到无权卷 version/）或假 409/假幂等（误伤新写入）。
	forceHomeVol := ""
	if explicitVol == "" {
		loc, found := h.locateOwnerFile(owner, rel)
		if found && loc != nil && loc.tenant != nil {
			forceHomeVol = loc.volumeName
			if loc.volumeName != "" {
				w.Header().Set(headerVolume, loc.volumeName)
			}
			handled, existed := h.handleDuplicateFile(w, r, loc.tenant, rel, expectedChecksum, remotePath)
			if handled {
				return // 幂等 200 / 冲突 409 已回包
			}
			// existed=true = home 卷命中且继续（版本化覆盖写）→ stay-home（forceHomeVol 保留，
			// 容量不足 routeUpload 直接 507 不换卷）。existed=false 是 locate 命中与 dup-check
			// 间 TOCTOU 的防御（理论竞态）→ 交容量路由。
			if !existed {
				forceHomeVol = ""
			}
		}
		// locate miss → 新文件：交容量路由（无 dup-check；volSet==nil 唯一根下 stat miss
		// 等价旧行为——handleDuplicateFile 在 miss 时本就返回 false）。
	}

	// 卷路由 + 双账本预留（T4/T5）：routeUpload 按 ACL/placement 选目标卷，在 owner 全局
	// Scope + 卷容量池双 TryReserve；显式 volume= 时做 ACL 校验与唯一性查重（403/409）；
	// forceHomeVol 非空时强制该 home 卷单候选（容量不足 507 不换卷）。
	route, err := h.routeUpload(owner, rel, explicitVol, handler.Size, forceHomeVol)
	if err != nil {
		h.sendUploadRouteError(w, r, remotePath, err)
		return
	}
	if route.tenant == nil || route.tenant.Root() == nil {
		route.release()
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	// 目标卷暴露给客户端（成功分支断言用；错误分支客户端忽略）。
	if route.volumeName != "" {
		w.Header().Set(headerVolume, route.volumeName)
	}
	root := route.tenant.Root()

	if mkdirErr := root.MkdirAll(filepath.Dir(rel), 0755); mkdirErr != nil {
		route.release()
		logger.ErrorContext(r.Context(), "创建目录失败", "error", mkdirErr.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	// 覆盖写场景先统计旧文件大小 prev（双 Adjust 差分用）。
	prev := int64(0)
	if stat, statErr := root.Stat(rel); statErr == nil {
		prev = stat.Size()
	}

	// 原子写入 + 流式哈希（目标卷 root）。
	serverChecksum, written, err := writeFileAtomicallyRoot(r.Context(), root, rel, file)
	if err != nil {
		route.release()
		logger.ErrorContext(r.Context(), "保存文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
		return
	}

	if serverChecksum != expectedChecksum {
		// 清理已写入的校验失败文件，忽略错误（临时文件由 writeFileAtomicallyRoot 清理）
		_ = root.Remove(rel)
		route.release()
		logger.WarnContext(r.Context(), "文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	// 双账本结算：覆盖写 Adjust(prev, written) + Release（旧文件已占用 prev，差分收敛到
	// 新大小）；新文件 Commit(written)。owner 全局 Scope 与卷容量池同语义。
	route.commit(prev, written)

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
	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return "", "", false
	}
	tnt := h.tenantOf(r)
	if tnt == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	// 服务端内部目录访问防护收敛到 UserRel（逐段 ValidSegmentName 拒绝 .__ 前缀）：
	// 用户显式上传/重命名不得落到服务端内部目录，user/ 桶内 .__ 前缀段已被
	// ValidSegmentName 拒绝，无需再单独内部目录守卫。
	rel, ok = tnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, rel, true
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
// 返回 handled=true 表示已处理并回包（调用方应 return）；existed=true 表示 rel 在 homeTnt
// 上已存在且走「继续写入」（版本化覆盖写 proceed 分支）——调用方据此强制 route 回
// homeTnt 所在卷（覆盖写 stay-home 不变式，F1-A/F1-B）。homeTnt 为已定位的 home 卷租户
// （自动路由写前 locateOwnerFile 定位；单卷 = 默认租户），rel 为租户根内相对路径。
func (h *Handlers) handleDuplicateFile(w http.ResponseWriter, r *http.Request, homeTnt *storage.Tenant, rel, expectedChecksum, remotePath string) (handled bool, existed bool) {
	ctx := r.Context()
	if homeTnt == nil || homeTnt.Root() == nil {
		return false, false
	}
	root := homeTnt.Root()
	stat, statErr := root.Stat(rel)
	if statErr != nil {
		return false, false // 文件不存在，继续正常上传（新文件或非本卷 home）
	}
	if verifyFileWithChecksumRoot(root, rel, expectedChecksum) {
		// 幂等上传：文件已存在且 checksum 匹配，直接返回成功（不保存版本）
		w.Header().Set(headerFileChecksum, expectedChecksum)
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
		return true, true
	}
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.Enabled {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本（homeTnt 即旧文件所在卷）
		h.fileService().SaveVersionBeforeOverwrite(r, remotePath, homeTnt)
		// 审查 I-3：覆盖动作记审计（含旧版本已保存的信息）。
		h.RecordAudit(ctx, AuditEvent{
			Action: "overwrite", ObjectType: "file", Object: remotePath,
			Result: AuditResultSuccess, Detail: "覆盖现有文件（版本已保存）",
		})
		return false, true // 继续执行写入流程，用新内容覆盖现有文件（home=本卷）
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
	return true, true
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
