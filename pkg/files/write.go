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

// uploadingLockUpload 是 Deps.Uploading 锁池中「单次（非分块）上传」条目的值标记。
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

// setUploadResponseHeaders 设置上传成功后的响应头（checksum、mtime）。
// checksum 写入 per-tenant store，key = 租户根内相对路径 rel（无 owner 前缀）。
func (s *Service) setUploadResponseHeaders(w http.ResponseWriter, r *http.Request, root *storage.Root, remotePath, rel, serverChecksum string, logger *slog.Logger) {
	w.Header().Set(headerFileChecksum, serverChecksum)
	if cs := s.deps.ChecksumStoreFor(s.deps.ActorFromRequest(r)); cs != nil {
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

// Upload 处理 POST /upload。
// 多卷（T4/T5）：卷路由选目标卷（ACL/placement/容量），成功响应头 X-Volume 标识落盘卷；
// 覆盖写 stay-home 到 home 卷（见下方注释）。
func (s *Service) Upload(w http.ResponseWriter, r *http.Request) {
	logger := s.deps.Logger()

	file, handler, expectedChecksum, ok := s.parseUploadMultipart(w, r, logger)
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
	remotePath, rel, ok := s.resolveFilePath(w, r, remotePathStr)
	if !ok {
		return
	}
	logger.DebugContext(r.Context(), "上传路径", "remote_path", remotePath, "header", r.Header.Get("X-File-Path"), "multipart", handler.Filename)

	owner := s.deps.ActorFromRequest(r)

	// 并发上传防护：防止同一 owner 同 rel 被多个上传请求同时写入导致 OOM。
	// key = <owner>\x00<rel>（装配层共享 map，防跨租户同 rel 碰撞）。
	upKey := normalizeOwner(owner) + "\x00" + rel
	if _, loaded := s.deps.Uploading.LoadOrStore(upKey, uploadingLockUpload); loaded {
		logger.WarnContext(r.Context(), "文件正在上传中，拒绝并发上传", "file_name", remotePath)
		s.sendJSON(w, UploadResponse{Success: false, Message: "文件正在上传中"}, http.StatusConflict)
		return
	}
	defer s.deps.Uploading.Delete(upKey)

	explicitVol := r.FormValue("volume")

	// 重复检测与版本管理（自动路由）：先做「写前视图定位」确认 rel 的真实 home 卷（F1-A/B），
	// **只在 home 命中时**对其根做幂等/冲突/版本检查——幂等同 checksum 重传在容量/配额检查前
	// 直接 200（不因卷满误拒）；命中 → 覆盖写必须 stay-home 到 home 卷（容量路由只用于新文件，
	// 否则同 rel 跨卷双份 + owner 双计）。显式 volume 不做此检查：其语义是新文件定位，同名已
	// 存在（含同 checksum）一律由 routeUpload 唯一性 409。
	//
	// F-1（AD-6 写侧闭合）：home 以 owner 卷视图为界（LocateOwnerFile 只搜 AllowedVolumes）。
	// locate miss（rel 不在 owner 视图任何卷，含「默认卷被 ACL 排除但仍留 owner 遗留」）→
	// 该 rel 不属于 owner 逻辑树，**跳过 dup-check 视为新文件**交容量路由——不得对无权卷的
	// 遗留做版本化覆盖写（泄漏到无权卷 version/）或假 409/假幂等（误伤新写入）。
	forceHomeVol := ""
	if explicitVol == "" {
		// 装配层的 LocateOwnerFile 以值类型表示定位结果：found=false 即「未定位到任何卷」
		// （基线 *fileLocation 的 nil 与 false 同义，故此处不再重复判 nil）。
		loc, found := s.deps.LocateOwnerFile(owner, rel)
		if found && loc.Tenant != nil {
			forceHomeVol = loc.VolumeName
			if loc.VolumeName != "" {
				w.Header().Set(headerVolume, loc.VolumeName)
			}
			handled, existed := s.handleDuplicateFile(w, r, loc.Tenant, rel, expectedChecksum, remotePath)
			if handled {
				return // 幂等 200 / 冲突 409 已回包
			}
			// existed=true = home 卷命中且继续（版本化覆盖写）→ stay-home（forceHomeVol 保留，
			// 容量不足 RouteUpload 直接 507 不换卷）。existed=false 是 locate 命中与 dup-check
			// 间 TOCTOU 的防御（理论竞态）→ 交容量路由。
			if !existed {
				forceHomeVol = ""
			}
		}
		// locate miss → 新文件：交容量路由（无 dup-check；VolSet==nil 唯一根下 stat miss
		// 等价旧行为——handleDuplicateFile 在 miss 时本就返回 false）。
	}

	// 卷路由 + 双账本预留（T4/T5）：RouteUpload 按 ACL/placement 选目标卷，在 owner 全局
	// Scope + 卷容量池双 TryReserve；显式 volume= 时做 ACL 校验与唯一性查重（403/409）；
	// forceHomeVol 非空时强制该 home 卷单候选（容量不足 507 不换卷）。
	route, err := s.deps.RouteUpload(owner, rel, explicitVol, handler.Size, forceHomeVol)
	if err != nil {
		s.sendUploadRouteError(w, r, remotePath, err)
		return
	}
	if route.Tenant == nil || route.Tenant.Root() == nil {
		route.Release()
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	// 目标卷暴露给客户端（成功分支断言用；错误分支客户端忽略）。
	if route.VolumeName != "" {
		w.Header().Set(headerVolume, route.VolumeName)
	}
	root := route.Tenant.Root()

	if mkdirErr := root.MkdirAll(filepath.Dir(rel), 0755); mkdirErr != nil {
		route.Release()
		logger.ErrorContext(r.Context(), "创建目录失败", "error", mkdirErr.Error())
		s.sendJSON(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
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
		route.Release()
		logger.ErrorContext(r.Context(), "保存文件失败", "error", err.Error(), "file_name", remotePath)
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
		return
	}

	if serverChecksum != expectedChecksum {
		// 清理已写入的校验失败文件，忽略错误（临时文件由 writeFileAtomicallyRoot 清理）
		_ = root.Remove(rel)
		route.Release()
		logger.WarnContext(r.Context(), "文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "file_name", remotePath)
		s.sendJSON(w, UploadResponse{Success: false, Message: "文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	// 双账本结算：覆盖写 Adjust(prev, written) + Release（旧文件已占用 prev，差分收敛到
	// 新大小）；新文件 Commit(written)。owner 全局 Scope 与卷容量池同语义。
	route.Commit(prev, written)

	s.setUploadResponseHeaders(w, r, root, remotePath, rel, serverChecksum, logger)

	s.sendJSON(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件上传成功, size: %d", handler.Size),
		Checksum: serverChecksum,
	}, http.StatusOK)
	if s.deps.Metrics != nil {
		s.deps.Metrics.RecordUpload(handler.Size)
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

// resolveFilePath 校验 filename 并映射到请求者租户的 user 桶相对路径。
// 返回已验证的协议路径（remotePath）与租户根内相对路径（rel，如 user/dir/f.txt）。
// 校验失败时返回 false。
func (s *Service) resolveFilePath(w http.ResponseWriter, r *http.Request, filename string) (remotePath, rel string, ok bool) {
	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return "", "", false
	}
	tnt := s.deps.TenantFor(s.deps.ActorFromRequest(r))
	if tnt == nil {
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	// 服务端内部目录访问防护收敛到 UserRel（逐段 ValidSegmentName 拒绝 .__ 前缀）：
	// 用户显式上传/重命名不得落到服务端内部目录，user/ 桶内 .__ 前缀段已被
	// ValidSegmentName 拒绝，无需再单独内部目录守卫。
	rel, ok = tnt.UserRel(remotePath)
	if !ok {
		s.sendJSON(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, rel, true
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
// 返回 handled=true 表示已处理并回包（调用方应 return）；existed=true 表示 rel 在 homeTnt
// 上已存在且走「继续写入」（版本化覆盖写 proceed 分支）——调用方据此强制 route 回
// homeTnt 所在卷（覆盖写 stay-home 不变式，F1-A/F1-B）。homeTnt 为已定位的 home 卷租户
// （自动路由写前 LocateOwnerFile 定位；单卷 = 默认租户），rel 为租户根内相对路径。
func (s *Service) handleDuplicateFile(w http.ResponseWriter, r *http.Request, homeTnt *storage.Tenant, rel, expectedChecksum, remotePath string) (handled bool, existed bool) {
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
		s.sendJSON(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
		return true, true
	}
	// 与基线 `cfg := h.cfgPtr.Load(); if cfg.Versioning.Enabled` 逐字同源（接缝的
	// VersioningEnabled 即该形态，cfg 未装配时同样 panic——见 pkg/server/handlers.go 的
	// 接缝注释），故本处**无控制流残差**。
	if s.deps.VersioningEnabled() {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本（homeTnt 即旧文件所在卷）
		s.SaveVersionBeforeOverwrite(r, remotePath, homeTnt)
		// 审查 I-3：覆盖动作记审计（含旧版本已保存的信息）。
		s.deps.RecordFileAudit(ctx, "overwrite", remotePath, auditResultSuccess, "覆盖现有文件（版本已保存）")
		return false, true // 继续执行写入流程，用新内容覆盖现有文件（home=本卷）
	}
	// checksum 不匹配：冲突，需保留现有文件
	s.deps.Logger().WarnContext(ctx, "文件已存在，但校验失败", "file_name", remotePath)
	// 审查 I-3：versioning 关闭时同名覆盖是静默数据丢失，记审计（当前走冲突拒绝
	// 分支——保留现有文件，不覆盖；此处为 denied 留痕）。
	s.deps.RecordFileAudit(ctx, "overwrite", remotePath, auditResultDenied, "文件已存在且 checksum 不匹配（versioning 关闭，拒绝覆盖）")
	// 附带服务端文件的实际 checksum，方便客户端决策
	if serverCS, csErr := fileChecksumRoot(root, rel); csErr == nil {
		s.sendJSON(w, UploadResponse{
			Success:  false,
			Message:  "文件已存在，但校验失败",
			Checksum: serverCS,
		}, http.StatusConflict)
	} else {
		s.sendJSON(w, UploadResponse{Success: false, Message: "文件已存在，但校验失败"}, http.StatusConflict)
	}
	return true, true
}
