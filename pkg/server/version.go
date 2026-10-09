// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// version.go 是 `/api/versions` 的**附属 API 面**（list / restore / delete 三个处理器），
// 归装配层：核心文件操作 HTTP 面在 `pkg/files`，附属 API 面（versions / share / cloud /
// archive）留在装配层。
//
// 版本族的**存储侧**（版本 ID 生成、版本文件落盘/删除、跨卷版本目录定位与列举、覆盖写前
// 备份）在 `pkg/files/version_store.go`；本文件经 `h.fileService()` 消费其导出方法
// （CollectVersionEntries / FindVersionFile / SaveVersion / ReleaseVersionUsage /
// SaveVersionBeforeOverwrite / files.VersionIDTime），并保留本层特有的装配能力
// （文件级锁、审计、卷容量池、checksum 台账、跨卷复制）。
package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// filesMetaWriteAfterRestore 版本恢复落盘后重算配套 meta sidecar（C3 修复）：
// 复用 filesMetaPolicy.WriteMeta（与上传 writeFileSettle 同一入口——owner 配额
// 记账 + 覆盖写 Adjust 差分 + sidecar 落 meta 桶）。
func (h *Handlers) filesMetaWriteAfterRestore(dstRoot *storage.Root, owner, targetRel string) error {
	policy := filesMetaPolicy{h: h}
	if !policy.Enabled() {
		return nil
	}
	return policy.WriteMeta(context.Background(), owner, dstRoot, targetRel)
}

// VersionInfo 版本信息。
type VersionInfo struct {
	Filename  string `json:"filename"`
	VersionID int64  `json:"version_id"` // 毫秒时间戳×1000 + 随机后缀（见 newVersionID）
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum,omitempty"`
	CreatedAt string `json:"created_at"`
}

// listVersionsHandler 处理 GET /api/versions?filename=xxx。
func (h *Handlers) listVersionsHandler(w http.ResponseWriter, r *http.Request) {
	owner, remotePath, rel, verDir, ok := h.resolveVersionListTarget(w, r)
	if !ok {
		return
	}
	entries, ok := h.collectVersionEntriesForList(w, r, owner, remotePath)
	if !ok {
		return
	}
	versions, ok := h.buildVersionList(w, owner, remotePath, rel, verDir, entries)
	if !ok {
		return
	}
	sendJSONResponse(w, map[string]any{"versions": versions}, http.StatusOK)
}

// resolveVersionListTarget 解析并校验版本列表请求（filename / 路径 / 版本特性 / 租户视图 /
// 版本目录），任一失败已回包并返回 ok=false。
func (h *Handlers) resolveVersionListTarget(w http.ResponseWriter, r *http.Request) (string, string, string, string, bool) {
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return "", "", "", "", false
	}
	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return "", "", "", "", false
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return "", "", "", "", false
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return "", "", "", "", false
	}
	rel, relOK := baseTnt.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", "", "", false
	}
	verDir, dirOK := baseTnt.FeatureRel("version", remotePath)
	if !dirOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", "", "", false
	}
	return owner, remotePath, rel, verDir, true
}

// collectVersionEntriesForList 收集版本目录条目（A2-D 跨卷合并：扫描 owner 视图各卷的
// version/<rel> 目录并合并，version id 去重——文件被 move 到别的卷后，留在原卷的版本
// 仍可见）；读取失败记 Error 并 500，返回 ok=false。
func (h *Handlers) collectVersionEntriesForList(w http.ResponseWriter, r *http.Request, owner, remotePath string) ([]files.VersionEntry, bool) {
	entries, collErr := h.fileService().CollectVersionEntries(owner, remotePath)
	if collErr != nil {
		h.logger.Error("读取版本目录失败", "file_name", remotePath, "error", collErr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取版本目录失败"}, http.StatusInternalServerError)
		return nil, false
	}
	return entries, true
}

// buildVersionList 组装版本列表响应：与旧语义一致，无可列版本时 user 文件在视图内可见或
// 默认卷对 owner 开放 → 200 空列表；否则 404 fail-closed（默认卷被 ACL 排除且视图内无该
// 文件，不泄存在性）。逐条目补 checksum（per-tenant store，key = version/<rel>/<id>，与卷无关）。
func (h *Handlers) buildVersionList(w http.ResponseWriter, owner, remotePath, rel, verDir string, entries []files.VersionEntry) ([]VersionInfo, bool) {
	if len(entries) == 0 {
		_, fileFound := h.locateOwnerFile(owner, rel)
		if !fileFound && !h.defaultVolumeAllows(owner) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
			return nil, false
		}
	}

	csStore := h.checksumStoreFor(owner)
	versions := make([]VersionInfo, 0, len(entries))
	for _, e := range entries {
		fi := VersionInfo{
			Filename:  filepath.ToSlash(remotePath),
			VersionID: e.VersionID,
			Size:      e.Info.Size(),
			// 版本 ID 为毫秒时间戳×1000+随机后缀（见 pkg/files 的 newVersionID），/1000 还原毫秒
			// 时间戳。`VersionIDTime` 的"回落 mtime"分支在此**已不可达**（非正 ID 被列表侧的
			// `parseVersionID` 过滤掉，见 version > 0 不变量），保留它只作防御——供未来若出现
			// 直接调用 `VersionIDTime` 的旁路时仍有确定行为。
			CreatedAt: files.VersionIDTime(e.VersionID, e.Info.ModTime()).Format(time.RFC3339),
		}
		// 尝试获取 checksum（per-tenant store，key = version/<rel>/<id>，与卷无关）
		if csStore != nil {
			if cs, ok := csStore.Get(verDir + "/" + e.Name); ok {
				fi.Checksum = cs
			}
		}
		versions = append(versions, fi)
	}
	return versions, true
}

// restoreVersionHandler 处理 POST /api/versions/restore?filename=xxx&version_id=xxx。
func (h *Handlers) restoreVersionHandler(w http.ResponseWriter, r *http.Request) {
	owner, remotePath, targetRel, ok := h.resolveVersionRestoreTarget(w, r)
	if !ok {
		return
	}

	// 文件级互斥（T6c move 锁架构延伸）：restore 会写回 user 文件（可能与其 home 卷不同），
	// 与并发 move（复制→删源）共用同 rel 锁——无锁时 move 删源后 restore 可能把文件写回源卷，
	// 与目标卷副本并存（AD-4 破坏）；持锁后并发 move 期间 restore 409。
	release, locked := h.acquireVersionFileLock(w, r, owner, targetRel, remotePath)
	if !locked {
		return
	}
	defer release()

	versionIDStr := r.URL.Query().Get("version_id")
	// A2-D 跨卷定位：版本文件可能留在 user 文件曾所在的卷（跨卷 move 后版本不迁移）。
	verLoc, verRel, verInfo, ok := h.findRestoreVersionFile(w, r, owner, remotePath, versionIDStr)
	if !ok {
		return
	}

	// 目标卷 = user 文件**当前**所在卷（AD-4：不因跨卷恢复分叉出双份；版本字节不迁移，
	// 配额仍计在原卷池）。文件已不可定位（已删除）→ 回落版本文件所在卷（与旧孤儿恢复一致）。
	dstTnt, dstVol := h.resolveRestoreDestination(owner, targetRel, verLoc)
	dstRoot := dstTnt.Root()

	// 先保存当前版本（回滚前备份）到目标卷（文件所在卷），备份失败时返回 500 拒绝执行恢复
	if !h.backupBeforeRestore(w, r, remotePath, versionIDStr, owner, dstTnt) {
		return
	}

	// P4/P5 配额（I3 修复）：恢复把版本文件拷回 user 桶（覆盖当前文件），本质是新增
	// user 桶字节——缺失配额可反复 restore 突破租户上限。与 upload 对齐：TryReserve(版本大小)
	// 预留 → 拷贝成功后 Adjust(prev, actual)（覆盖写）/ Commit(actual)（新文件）；失败 Release()。
	// scope 按目标 user 桶 rel 解析（与 upload 同一键），子目录配额逐级检查自动生效。
	// 卷容量池同族（T6c 安全② reserve-then-commit）：恢复新增/覆盖的 user 字节先 TryReserve
	// **目标卷**容量池（user 字节物理落在目标卷，写前封顶），任一侧配额不足 → 507 拒绝恢复。
	scope, pool, res, poolRes, prev, releaseRes, ok := h.reserveRestoreQuota(w, r, remotePath, owner, targetRel, dstTnt, verInfo)
	if !ok {
		return
	}

	// 拷贝版本文件到目标位置：同卷 → 原地覆盖（既有语义，权限 0644）；跨卷 → 跨根复制
	// （temp + fsync + 原子 rename，MkdirAll 目标父目录），避免把文件写回源卷造成双份。
	written, ok := h.restoreCopyVersionFile(&versionRestoreCopyCtx{
		w: w, r: r, remotePath: remotePath, versionIDStr: versionIDStr,
		verLoc: verLoc, verRel: verRel, dstRoot: dstRoot, dstVol: dstVol,
		targetRel: targetRel, releaseRes: releaseRes,
	})
	if !ok {
		return
	}

	// P4/P5 配额对账：覆盖写 Adjust(prev, written) + Release 预留；新文件 Commit(written)。
	// owner 全局 scope 与卷容量池同语义（卷池侧同样 reserve-then-commit，物理字节入卷池）。
	h.settleRestoreQuota(scope, pool, res, poolRes, prev, written)

	// 更新 checksum（per-tenant store，key = user 桶相对路径）
	checksum, ok := h.updateRestoredChecksum(w, r, dstRoot, targetRel, remotePath, versionIDStr)
	if !ok {
		return
	}

	// C3 修复：版本恢复覆盖/新写目标文件后，重算配套 meta sidecar（主文件内容已变，
	// meta/<rel>.meta 须同步——否则 meta 描述旧版哈希/内容、主文件与 meta 永久不一致）。
	// 与上传 writeFileSettle 的到达即建同一入口（WriteMeta 幂等覆盖 + 配额 Adjust）。
	if mErr := h.filesMetaWriteAfterRestore(dstRoot, owner, targetRel); mErr != nil {
		h.logger.Warn("版本恢复后 meta 重算失败（读路径直算兜底）", "file_name", remotePath, "error", mErr)
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_restore", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + versionIDStr,
	})
	h.logger.Info("文件版本已恢复", "file_name", remotePath, "version_id", versionIDStr)
	// 版本恢复 = 文件内容变更（回滚到旧版本）——发布 version 事件供订阅者
	// （WebUI 实时刷新 / 连续同步 watch）感知。rel 用 targetRel 去 user/ 前缀
	// （与 files 层 publishFileEvent 的 rel 形态一致：无前缀相对路径）；
	// size = 恢复后文件大小。
	h.eventBus().Publish(files.EventVersion, normalizeOwner(owner), strings.TrimPrefix(targetRel, "user/"), written)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("已恢复版本 %s", versionIDStr), Checksum: checksum}, http.StatusOK)
}

// resolveVersionRestoreTarget 解析并校验版本恢复请求（filename / version_id / 路径 / 版本
// 特性 / 租户视图），任一失败已回包并返回 ok=false。
func (h *Handlers) resolveVersionRestoreTarget(w http.ResponseWriter, r *http.Request) (string, string, string, bool) {
	filename := r.URL.Query().Get("filename")
	versionIDStr := r.URL.Query().Get("version_id")
	if filename == "" || versionIDStr == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
		return "", "", "", false
	}

	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return "", "", "", false
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return "", "", "", false
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return "", "", "", false
	}
	targetRel, relOK := baseTnt.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", "", false
	}
	return owner, remotePath, targetRel, true
}

// acquireVersionFileLock 获取文件级互斥（T6c move 锁架构延伸）：restore 会写回 user 文件
// （可能与其 home 卷不同），与并发 move（复制→删源）共用同 rel 锁——无锁时 move 删源后
// restore 可能把文件写回源卷，与目标卷副本并存（AD-4 破坏）；持锁后并发 move 期间 409。
func (h *Handlers) acquireVersionFileLock(w http.ResponseWriter, r *http.Request, owner, targetRel, remotePath string) (func(), bool) {
	release, locked := h.acquireFileLock(owner, targetRel)
	if !locked {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "文件正在移动/上传中",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在移动/上传中，请稍后重试"}, http.StatusConflict)
		return nil, false
	}
	return release, true
}

// findRestoreVersionFile 跨卷定位版本文件（A2-D：版本文件可能留在 user 文件曾所在的卷，
// 跨卷 move 后版本不迁移）；stat 失败记 Error 并 500，未命中按「版本不存在」404。
func (h *Handlers) findRestoreVersionFile(w http.ResponseWriter, r *http.Request, owner, remotePath, versionIDStr string) (*files.VersionLocation, string, os.FileInfo, bool) {
	verLoc, verRel, verInfo, found, ferr := h.fileService().FindVersionFile(owner, remotePath, versionIDStr)
	if ferr != nil {
		h.logger.Error("stat 版本文件失败", "file_name", remotePath, "error", ferr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问版本文件失败"}, http.StatusInternalServerError)
		return nil, "", nil, false
	}
	if !found {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: msgVersionFileMissingPF + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgVersionFileMissing}, http.StatusNotFound)
		return nil, "", nil, false
	}
	return verLoc, verRel, verInfo, true
}

// resolveRestoreDestination 确定恢复目标卷：目标卷 = user 文件**当前**所在卷（AD-4：不因
// 跨卷恢复分叉出双份；版本字节不迁移，配额仍计在原卷池）。文件已不可定位（已删除）→
// 回落版本文件所在卷（与旧孤儿恢复一致）。
func (h *Handlers) resolveRestoreDestination(owner, targetRel string, verLoc *files.VersionLocation) (*storage.Tenant, string) {
	dstTnt, dstVol := verLoc.Tenant, verLoc.VolumeName
	if floc, ffound := h.locateOwnerFile(owner, targetRel); ffound && floc != nil && floc.tenant != nil && floc.tenant.Root() != nil {
		dstTnt, dstVol = floc.tenant, floc.volumeName
	}
	return dstTnt, dstVol
}

// backupBeforeRestore 在恢复执行前保存当前版本（回滚前备份）到目标卷（文件所在卷），
// 备份失败时记审计 + 500 拒绝执行恢复并返回 false。
func (h *Handlers) backupBeforeRestore(w http.ResponseWriter, r *http.Request, remotePath, versionIDStr, owner string, dstTnt *storage.Tenant) bool {
	if _, err := h.fileService().SaveVersion(remotePath, dstTnt, owner); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "恢复前备份失败: " + versionIDStr,
		})
		h.logger.Error("恢复版本前备份失败", "file_name", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复版本前备份失败，已中止"}, http.StatusInternalServerError)
		return false
	}
	return true
}

// reserveRestoreQuota 为恢复预留配额（P4/P5 I3 修复）：TryReserve(版本大小) → 覆盖写
// Adjust(prev, actual) / 新文件 Commit(actual) / 失败 Release()。scope 按目标 user 桶
// rel 解析（与 upload 同一键），卷容量池同族（T6c 安全② reserve-then-commit），任一侧
// 配额不足 → 507 拒绝恢复（ok=false）。
func (h *Handlers) reserveRestoreQuota(w http.ResponseWriter, r *http.Request, remotePath, owner, targetRel string, dstTnt *storage.Tenant, verInfo os.FileInfo) (*quota.Scope, *quota.Pool, *quota.Reservation, *quota.Reservation, int64, func(), bool) {
	scope := h.quotaScopeFor(owner, targetRel)
	pool := h.volumePoolForTenant(dstTnt)
	prev := int64(0)
	if st, statErr := dstTnt.Root().Stat(targetRel); statErr == nil {
		prev = st.Size()
	}
	var res, poolRes *quota.Reservation
	if scope != nil {
		rr, reserveErr := scope.TryReserve(verInfo.Size())
		if reserveErr != nil {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "存储配额不足，拒绝恢复",
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: msgStorageQuotaExceeded}, http.StatusInsufficientStorage)
			return nil, nil, nil, nil, 0, nil, false
		}
		res = rr
	}
	if pool != nil {
		rr, reserveErr := pool.TryReserve(verInfo.Size())
		if reserveErr != nil {
			if res != nil {
				res.Release()
			}
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "卷容量不足，拒绝恢复",
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: msgStorageQuotaExceeded}, http.StatusInsufficientStorage)
			return nil, nil, nil, nil, 0, nil, false
		}
		poolRes = rr
	}
	releaseRes := func() {
		if res != nil {
			res.Release()
		}
		if poolRes != nil {
			poolRes.Release()
		}
	}
	return scope, pool, res, poolRes, prev, releaseRes, true
}

// versionRestoreCopyCtx 承载版本恢复还原所需的上下文（响应端点 + 版本来源 + 目标位置 +
// 配额释放函数），由 restoreVersionHandler 构造、restoreCopyVersionFile 消费。
type versionRestoreCopyCtx struct {
	w            http.ResponseWriter
	r            *http.Request
	remotePath   string
	versionIDStr string
	verLoc       *files.VersionLocation
	verRel       string
	dstRoot      *storage.Root
	dstVol       string
	targetRel    string
	releaseRes   func()
}

// restoreCopyVersionFile 把版本文件拷贝到目标位置：同卷 → 原地覆盖（既有语义，权限 0644）；
// 跨卷 → 跨根复制（temp + fsync + 原子 rename，MkdirAll 目标父目录），避免把文件写回源卷
// 造成双份。任何失败已释放预留、记审计 + 500 并返回 ok=false。
func (h *Handlers) restoreCopyVersionFile(rc *versionRestoreCopyCtx) (int64, bool) {
	var written int64
	if rc.dstVol == rc.verLoc.VolumeName {
		src, oerr := rc.verLoc.Tenant.Root().Open(rc.verRel)
		if oerr != nil {
			rc.releaseRes()
			h.RecordAudit(rc.r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: rc.remotePath,
				Result: AuditResultError, Detail: "打开版本文件失败: " + rc.versionIDStr,
			})
			sendJSONResponse(rc.w, UploadResponse{Success: false, Message: "打开版本文件失败"}, http.StatusInternalServerError)
			return 0, false
		}
		defer src.Close()

		dst, derr := rc.dstRoot.OpenFile(rc.targetRel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if derr != nil {
			rc.releaseRes()
			h.RecordAudit(rc.r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: rc.remotePath,
				Result: AuditResultError, Detail: "创建目标文件失败: " + rc.versionIDStr,
			})
			sendJSONResponse(rc.w, UploadResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
			return 0, false
		}
		defer dst.Close()

		var err error
		written, err = io.Copy(dst, src)
		if err != nil {
			rc.releaseRes()
			h.RecordAudit(rc.r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: rc.remotePath,
				Result: AuditResultError, Detail: "恢复文件失败: " + rc.versionIDStr,
			})
			sendJSONResponse(rc.w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
			return 0, false
		}
		if dst.Sync() != nil {
			rc.releaseRes()
			h.RecordAudit(rc.r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: rc.remotePath,
				Result: AuditResultError, Detail: "同步文件失败: " + rc.versionIDStr,
			})
			sendJSONResponse(rc.w, UploadResponse{Success: false, Message: "同步文件失败"}, http.StatusInternalServerError)
			return 0, false
		}
	} else {
		var err error
		written, err = crossVolumeCopy(rc.r.Context(), rc.verLoc.Tenant.Root(), rc.dstRoot, rc.verRel, rc.targetRel)
		if err != nil {
			rc.releaseRes()
			h.RecordAudit(rc.r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: rc.remotePath,
				Result: AuditResultError, Detail: "跨卷恢复文件失败: " + rc.versionIDStr,
			})
			h.logger.Error("跨卷恢复版本失败", "file_name", rc.remotePath, "from", rc.verLoc.VolumeName,
				"to", rc.dstVol, "error", err)
			sendJSONResponse(rc.w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
			return 0, false
		}
	}
	return written, true
}

// settleRestoreQuota 恢复后的配额对账（P4/P5）：覆盖写 Adjust(prev, written) + Release
// 预留；新文件 Commit(written)。owner 全局 scope 与卷容量池同语义（卷池侧同样
// reserve-then-commit，物理字节入卷池）。
func (h *Handlers) settleRestoreQuota(scope *quota.Scope, pool *quota.Pool, res, poolRes *quota.Reservation, prev, written int64) {
	if res != nil {
		if prev > 0 {
			scope.Adjust(prev, written)
			res.Release()
		} else {
			res.Commit(written)
		}
	}
	if poolRes != nil {
		if prev > 0 {
			pool.Adjust(prev, written)
			poolRes.Release()
		} else {
			poolRes.Commit(written)
		}
	}
}

// updateRestoredChecksum 计算恢复后文件的 checksum 并写入 per-tenant store（key = user
// 桶相对路径）；计算失败记审计 + 500 并返回 ok=false。
func (h *Handlers) updateRestoredChecksum(w http.ResponseWriter, r *http.Request, dstRoot *storage.Root, targetRel, remotePath, versionIDStr string) (string, bool) {
	checksum, err := FileChecksumRoot(dstRoot, targetRel)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "计算文件校验和失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件校验和失败"}, http.StatusInternalServerError)
		return "", false
	}
	if cs := h.checksumStoreFor(ownerFromRequest(r)); cs != nil {
		cs.Set(targetRel, checksum)
	}
	return checksum, true
}

// gcAllExpiredVersionsPass 执行一轮整仓版本 GC：遍历默认卷租户缓存的已知 owner，对每个
// owner 的 version 桶根做保留期清理（files.VersionDirs 枚举各 rel，逐目录调
// GCExpiredVersions）。只做保留期清理、不做上限截断（上限截断随写入路径被动执行）；
// versioning 未启用或 retention<=0 时为空操作。
//
// owner 名单从默认卷根磁盘扫描取得（storage.ListOwners：内存缓存只有已访问的租户，
// 仅靠缓存会漏掉已落盘但尚未访问的租户）；VolSet 未装配（旧装配路径）时默认卷根即唯一根。
func (h *Handlers) gcAllExpiredVersionsPass() {
	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled || cfg.Versioning.Retention <= 0 {
		return
	}
	if h.globalRoot == nil {
		return
	}
	for _, owner := range storage.ListOwners(h.globalRoot) {
		for _, loc := range h.fileService().VersionDirs(owner) {
			h.fileService().GCExpiredVersions(owner, loc.Rel)
		}
	}
}

// deleteVersionHandler 处理 DELETE /api/versions?filename=xxx&version_id=xxx。
func (h *Handlers) deleteVersionHandler(w http.ResponseWriter, r *http.Request) {
	owner, remotePath, userRel, ok := h.resolveVersionDeleteTarget(w, r)
	if !ok {
		return
	}

	// 文件级互斥（T6c move 锁架构延伸）：版本删除与同 rel 的 move/上传/恢复共用锁，
	// 避免与并发 restore（恢复前备份）等操作交错。
	release, locked := h.acquireVersionDeleteLock(w, r, owner, userRel, remotePath)
	if !locked {
		return
	}
	defer release()

	versionIDStr := r.URL.Query().Get("version_id")
	// A2-D 跨卷定位：版本文件可能留在 user 文件曾所在的卷（跨卷 move 后版本不迁移）。
	verLoc, verRel, delSize, ok := h.findDeleteVersionFile(w, r, owner, remotePath, versionIDStr)
	if !ok {
		return
	}
	// P5 版本桶配额：删除前记录文件大小，删除成功后释放**版本所在卷**的版本桶 Scope 与卷池。
	if !h.removeVersionFileForRequest(w, r, remotePath, versionIDStr, verLoc, verRel) {
		return
	}
	h.fileService().ReleaseVersionUsage(verLoc.Tenant, owner, delSize)

	// 实删对象的**规范 id**：来自 FindVersionFile 回传的 verRel（路径段由 `FormatInt(id)` 生成），
	// 与 `SaveVersion` 记录 checksum 时使用的 key 形态**逐字一致**。
	canonicalID := filepath.Base(verRel)

	// 清理 checksumStore 中对应的版本记录（key = 版本文件的 rel，无 owner 前缀，与卷无关）。
	// **必须用规范 verRel**：写侧的 key 由 SaveVersion 生成；若此处用**原始请求串**拼 key，
	// 非规范拼写（如 `?version_id=%2B5` ⇒ 原始串 "+5"，而盘上与 checksum 里都是 "5"）会出现
	// "文件按规范名删掉、checksum 条目留在原名下"的**孤儿**（F37 复审发现的分叉）。
	if cs := h.checksumStoreFor(owner); cs != nil {
		cs.Delete(verRel)
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_delete", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + canonicalID,
	})
	// 版本删除非内容变更（版本文件移除）——发布 version 事件（size=0）供订阅者
	// 感知版本历史变化（WebUI 版本面板 / 连续同步 watch 需要）。
	h.eventBus().Publish(files.EventVersion, normalizeOwner(owner), strings.TrimPrefix(userRel, "user/"), 0)
	sendJSONResponse(w, UploadResponse{Success: true, Message: "版本已删除"}, http.StatusOK)
}

// resolveVersionDeleteTarget 解析并校验版本删除请求（filename / version_id / 路径 / 版本
// 特性 / 租户视图），任一失败已回包并返回 ok=false。
func (h *Handlers) resolveVersionDeleteTarget(w http.ResponseWriter, r *http.Request) (string, string, string, bool) {
	filename := r.URL.Query().Get("filename")
	versionIDStr := r.URL.Query().Get("version_id")
	if filename == "" || versionIDStr == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
		return "", "", "", false
	}

	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return "", "", "", false
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return "", "", "", false
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return "", "", "", false
	}
	userRel, relOK := baseTnt.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", "", false
	}
	return owner, remotePath, userRel, true
}

// acquireVersionDeleteLock 获取文件级互斥（T6c move 锁架构延伸）：版本删除与同 rel 的
// move/上传/恢复共用锁，避免与并发 restore（恢复前备份）等操作交错。
func (h *Handlers) acquireVersionDeleteLock(w http.ResponseWriter, r *http.Request, owner, userRel, remotePath string) (func(), bool) {
	release, locked := h.acquireFileLock(owner, userRel)
	if !locked {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "文件正在移动/上传中",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在移动/上传中，请稍后重试"}, http.StatusConflict)
		return nil, false
	}
	return release, true
}

// findDeleteVersionFile 跨卷定位版本文件（A2-D：版本文件可能留在 user 文件曾所在的卷，
// 跨卷 move 后版本不迁移）并记录删除大小；stat 失败记 Error 并 500，未命中按「版本
// 不存在」404。
func (h *Handlers) findDeleteVersionFile(w http.ResponseWriter, r *http.Request, owner, remotePath, versionIDStr string) (*files.VersionLocation, string, int64, bool) {
	verLoc, verRel, verInfo, found, ferr := h.fileService().FindVersionFile(owner, remotePath, versionIDStr)
	if ferr != nil {
		h.logger.Error("stat 版本文件失败", "file_name", remotePath, "error", ferr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问版本文件失败"}, http.StatusInternalServerError)
		return nil, "", 0, false
	}
	if !found {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: msgVersionFileMissingPF + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgVersionFileMissing}, http.StatusNotFound)
		return nil, "", 0, false
	}
	return verLoc, verRel, verInfo.Size(), true
}

// removeVersionFileForRequest 按请求路径删除版本文件本身（DELETE /api/versions 专用）：
// 已不存在按「版本不存在」404，其它删除失败 500，均返回 false（不再执行后续的配额释放
// 与 checksum 清理）。
func (h *Handlers) removeVersionFileForRequest(w http.ResponseWriter, r *http.Request, remotePath, versionIDStr string, verLoc *files.VersionLocation, verRel string) bool {
	if err := verLoc.Tenant.Root().Remove(verRel); err != nil {
		if os.IsNotExist(err) {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_delete", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: msgVersionFileMissingPF + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: msgVersionFileMissing}, http.StatusNotFound)
		} else {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_delete", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "删除版本文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "删除版本文件失败"}, http.StatusInternalServerError)
		}
		return false
	}
	return true
}
