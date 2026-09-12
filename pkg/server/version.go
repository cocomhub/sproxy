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
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
)

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
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	rel, relOK := baseTnt.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	verDir, dirOK := baseTnt.FeatureRel("version", remotePath)
	if !dirOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// A2-D 跨卷合并：扫描 owner 视图各卷的 version/<rel> 目录并合并（version id 去重）——
	// 文件被 move 到别的卷后，留在原卷的版本仍可见。
	entries, collErr := h.fileService().CollectVersionEntries(owner, remotePath)
	if collErr != nil {
		h.logger.Error("读取版本目录失败", "file_name", remotePath, "error", collErr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取版本目录失败"}, http.StatusInternalServerError)
		return
	}
	// 与旧语义一致：无可列版本时，user 文件在视图内可见或默认卷对 owner 开放 → 200 空列表；
	// 否则 404 fail-closed（默认卷被 ACL 排除且视图内无该文件，不泄存在性）。
	if len(entries) == 0 {
		_, fileFound := h.locateOwnerFile(owner, rel)
		if !fileFound && !h.defaultVolumeAllows(owner) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
			return
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
			// 时间戳；历史遗留的非正 ID 无法还原时间，回落版本文件 mtime。
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

	sendJSONResponse(w, map[string]any{"versions": versions}, http.StatusOK)
}

// restoreVersionHandler 处理 POST /api/versions/restore?filename=xxx&version_id=xxx。
func (h *Handlers) restoreVersionHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	versionIDStr := r.URL.Query().Get("version_id")
	if filename == "" || versionIDStr == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
		return
	}

	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	targetRel, ok := baseTnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// 文件级互斥（T6c move 锁架构延伸）：restore 会写回 user 文件（可能与其 home 卷不同），
	// 与并发 move（复制→删源）共用同 rel 锁——无锁时 move 删源后 restore 可能把文件写回源卷，
	// 与目标卷副本并存（AD-4 破坏）；持锁后并发 move 期间 restore 409。
	release, locked := h.acquireFileLock(owner, targetRel)
	if !locked {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "文件正在移动/上传中",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在移动/上传中，请稍后重试"}, http.StatusConflict)
		return
	}
	defer release()

	// A2-D 跨卷定位：版本文件可能留在 user 文件曾所在的卷（跨卷 move 后版本不迁移）。
	verLoc, verRel, verInfo, found, ferr := h.fileService().FindVersionFile(owner, remotePath, versionIDStr)
	if ferr != nil {
		h.logger.Error("stat 版本文件失败", "file_name", remotePath, "error", ferr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问版本文件失败"}, http.StatusInternalServerError)
		return
	}
	if !found {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		return
	}

	// 目标卷 = user 文件**当前**所在卷（AD-4：不因跨卷恢复分叉出双份；版本字节不迁移，
	// 配额仍计在原卷池）。文件已不可定位（已删除）→ 回落版本文件所在卷（与旧孤儿恢复一致）。
	dstTnt, dstVol := verLoc.Tenant, verLoc.VolumeName
	if floc, ffound := h.locateOwnerFile(owner, targetRel); ffound && floc != nil && floc.tenant != nil && floc.tenant.Root() != nil {
		dstTnt, dstVol = floc.tenant, floc.volumeName
	}
	dstRoot := dstTnt.Root()

	// 先保存当前版本（回滚前备份）到目标卷（文件所在卷），备份失败时返回 500 拒绝执行恢复
	if _, err = h.fileService().SaveVersion(remotePath, dstTnt, owner); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "恢复前备份失败: " + versionIDStr,
		})
		h.logger.Error("恢复版本前备份失败", "file_name", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复版本前备份失败，已中止"}, http.StatusInternalServerError)
		return
	}

	// P4/P5 配额（I3 修复）：恢复把版本文件拷回 user 桶（覆盖当前文件），本质是新增
	// user 桶字节——缺失配额可反复 restore 突破租户上限。与 upload 对齐：TryReserve(版本大小)
	// 预留 → 拷贝成功后 Adjust(prev, actual)（覆盖写）/ Commit(actual)（新文件）；失败 Release()。
	// scope 按目标 user 桶 rel 解析（与 upload 同一键），子目录配额逐级检查自动生效。
	// 卷容量池同族（T6c 安全② reserve-then-commit）：恢复新增/覆盖的 user 字节先 TryReserve
	// **目标卷**容量池（user 字节物理落在目标卷，写前封顶），任一侧配额不足 → 507 拒绝恢复。
	scope := h.quotaScopeFor(owner, targetRel)
	pool := h.volumePoolForTenant(dstTnt)
	prev := int64(0)
	if st, statErr := dstRoot.Stat(targetRel); statErr == nil {
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
			sendJSONResponse(w, UploadResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
			return
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
			sendJSONResponse(w, UploadResponse{Success: false, Message: "存储配额不足"}, http.StatusInsufficientStorage)
			return
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

	// 拷贝版本文件到目标位置：同卷 → 原地覆盖（既有语义，权限 0644）；跨卷 → 跨根复制
	// （temp + fsync + 原子 rename，MkdirAll 目标父目录），避免把文件写回源卷造成双份。
	var written int64
	if dstVol == verLoc.VolumeName {
		src, oerr := verLoc.Tenant.Root().Open(verRel)
		if oerr != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "打开版本文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "打开版本文件失败"}, http.StatusInternalServerError)
			return
		}
		defer src.Close()

		dst, derr := dstRoot.OpenFile(targetRel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
		if derr != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "创建目标文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
			return
		}
		defer dst.Close()

		written, err = io.Copy(dst, src)
		if err != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "恢复文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
			return
		}
		if syncErr := dst.Sync(); syncErr != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "同步文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "同步文件失败"}, http.StatusInternalServerError)
			return
		}
	} else {
		written, err = crossVolumeCopy(r.Context(), verLoc.Tenant.Root(), dstRoot, verRel, targetRel)
		if err != nil {
			releaseRes()
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_restore", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "跨卷恢复文件失败: " + versionIDStr,
			})
			h.logger.Error("跨卷恢复版本失败", "file_name", remotePath, "from", verLoc.VolumeName,
				"to", dstVol, "error", err)
			sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
			return
		}
	}

	// P4/P5 配额对账：覆盖写 Adjust(prev, written) + Release 预留；新文件 Commit(written)。
	// owner 全局 scope 与卷容量池同语义（卷池侧同样 reserve-then-commit，物理字节入卷池）。
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

	// 更新 checksum（per-tenant store，key = user 桶相对路径）
	checksum, err := FileChecksumRoot(dstRoot, targetRel)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "计算文件校验和失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件校验和失败"}, http.StatusInternalServerError)
		return
	}
	if cs := h.checksumStoreFor(ownerFromRequest(r)); cs != nil {
		cs.Set(targetRel, checksum)
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_restore", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + versionIDStr,
	})
	h.logger.Info("文件版本已恢复", "file_name", remotePath, "version_id", versionIDStr)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("已恢复版本 %s", versionIDStr), Checksum: checksum}, http.StatusOK)
}

// deleteVersionHandler 处理 DELETE /api/versions?filename=xxx&version_id=xxx。
func (h *Handlers) deleteVersionHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	versionIDStr := r.URL.Query().Get("version_id")
	if filename == "" || versionIDStr == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
		return
	}

	remotePath, err := pathguard.ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	owner := ownerFromRequest(r)
	baseTnt := h.tenantFor(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		return
	}
	userRel, relOK := baseTnt.UserRel(remotePath)
	if !relOK {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// 文件级互斥（T6c move 锁架构延伸）：版本删除与同 rel 的 move/上传/恢复共用锁，
	// 避免与并发 restore（恢复前备份）等操作交错。
	release, locked := h.acquireFileLock(owner, userRel)
	if !locked {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultDenied, Detail: "文件正在移动/上传中",
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件正在移动/上传中，请稍后重试"}, http.StatusConflict)
		return
	}
	defer release()

	// A2-D 跨卷定位：版本文件可能留在 user 文件曾所在的卷（跨卷 move 后版本不迁移）。
	verLoc, verRel, verInfo, found, ferr := h.fileService().FindVersionFile(owner, remotePath, versionIDStr)
	if ferr != nil {
		h.logger.Error("stat 版本文件失败", "file_name", remotePath, "error", ferr)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问版本文件失败"}, http.StatusInternalServerError)
		return
	}
	if !found {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_delete", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		return
	}
	// P5 版本桶配额：删除前记录文件大小，删除成功后释放**版本所在卷**的版本桶 Scope 与卷池。
	delSize := verInfo.Size()
	if err := verLoc.Tenant.Root().Remove(verRel); err != nil {
		if os.IsNotExist(err) {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_delete", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		} else {
			h.RecordAudit(r.Context(), AuditEvent{
				Action: "version_delete", ObjectType: "file", Object: remotePath,
				Result: AuditResultError, Detail: "删除版本文件失败: " + versionIDStr,
			})
			sendJSONResponse(w, UploadResponse{Success: false, Message: "删除版本文件失败"}, http.StatusInternalServerError)
		}
		return
	}
	h.fileService().ReleaseVersionUsage(verLoc.Tenant, owner, delSize)

	// 清理 checksumStore 中对应的版本记录（key = version/<rel>/<id>，无 owner 前缀，与卷无关）
	if verDir, dirOK := baseTnt.FeatureRel("version", remotePath); dirOK {
		if cs := h.checksumStoreFor(owner); cs != nil {
			cs.Delete(verDir + "/" + versionIDStr)
		}
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_delete", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + versionIDStr,
	})
	sendJSONResponse(w, UploadResponse{Success: true, Message: "版本已删除"}, http.StatusOK)
}
