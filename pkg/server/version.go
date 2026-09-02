// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// VersionInfo 版本信息。
type VersionInfo struct {
	Filename  string `json:"filename"`
	VersionID int64  `json:"version_id"` // UnixNano timestamp
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum,omitempty"`
	CreatedAt string `json:"created_at"`
}

// saveVersion 在上传覆盖前保存当前文件版本。
// 返回保存的版本 ID（UnixNano），如果没有旧文件则返回 0。
// userRel 是相对 user 桶的路径（如 dir/f.txt，无 user/ 前缀）；tnt 为请求者租户。
// 版本文件落 version 桶（version/<userRel>/<id>），checksum key = version/<userRel>/<id>
// （相对租户根，无 owner 前缀，per-tenant store）——消除旧 __version__ 前缀的 R4 碰撞。
func (h *Handlers) saveVersion(userRel string, tnt *storage.Tenant, owner string) (int64, error) {
	if tnt == nil || tnt.Root() == nil {
		return 0, fmt.Errorf("保存版本: 租户不可用")
	}
	root := tnt.Root()
	fullRel, ok := tnt.UserRel(userRel)
	if !ok {
		return 0, fmt.Errorf("保存版本: 无效的文件路径: %s", userRel)
	}
	srcInfo, statErr := root.Stat(fullRel)
	if os.IsNotExist(statErr) {
		return 0, nil // 新文件，无需保存版本
	} else if statErr != nil {
		return 0, fmt.Errorf("检查源文件失败: %w", statErr)
	}
	srcSize := srcInfo.Size()

	versionID := time.Now().UnixNano()
	// 添加随机后缀（0-999），防止同一纳秒内多个请求产生冲突
	versionID = versionID*1000 + int64(rand.IntN(1000))
	verDir, ok := tnt.FeatureRel("version", userRel)
	if !ok {
		return 0, fmt.Errorf("保存版本: 无效的版本目录路径: %s", userRel)
	}
	if err := root.MkdirAll(verDir, 0o755); err != nil {
		return 0, fmt.Errorf("创建版本目录失败: %w", err)
	}

	verRel := verDir + "/" + strconv.FormatInt(versionID, 10)

	// P5 版本桶配额：写版本文件前预留源文件大小（版本是旧文件的拷贝，字节计入租户
	// version 桶 Scope），写入成功后 Commit(actual)；失败/放弃 Release。配额不足时
	// 拒绝保存版本（调用方 best-effort：覆盖写路径跳过版本，恢复路径 500 中止）。
	var res *quota.Reservation
	if scope := h.quotaBucketFor(owner, "version"); scope != nil {
		rr, reserveErr := scope.TryReserve(srcSize)
		if reserveErr != nil {
			return 0, fmt.Errorf("保存版本: 存储配额不足: %w", reserveErr)
		}
		res = rr
	}

	src, err := root.Open(fullRel)
	if err != nil {
		if res != nil {
			res.Release()
		}
		return 0, fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	dst, err := root.OpenFile(verRel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if res != nil {
			res.Release()
		}
		return 0, fmt.Errorf("创建版本文件失败: %w", err)
	}
	defer dst.Close()

	// 流式计算 checksum：一边复制一边计算 SHA-256，避免重复读取。
	// 审查 #9 结论（勿再分析）：此处是**单遍**复制+哈希（io.MultiWriter），并无
	// "先哈希再复制"的重复读取；old 文件 checksum 记录须独立重算（旧版本删过
	// checksum，store 中无可靠值），新文件由上传方提供 expectedChecksum 校验。
	// 无重复哈希可省，维持现状。
	hasher := sha256.New()
	multiWriter := io.MultiWriter(dst, hasher)
	written, err := io.Copy(multiWriter, src)
	if err != nil {
		_ = root.Remove(verRel)
		if res != nil {
			res.Release()
		}
		return 0, fmt.Errorf("复制版本文件失败: %w", err)
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// 写入 checksumStore（per-tenant store，key = version/<userRel>/<id>，无 owner 前缀）
	csKey := verRel
	if cs := h.checksumStoreFor(owner); cs != nil {
		cs.Set(csKey, checksum)
	} else {
		h.logger.Warn("per-tenant checksum store 不可用，跳过版本 checksum 记录", "file_name", userRel)
	}

	// 显式 fsync 版本文件，确保崩溃时不会丢失已保存的版本
	if err := dst.Sync(); err != nil {
		_ = root.Remove(verRel)
		if res != nil {
			res.Release()
		}
		return 0, fmt.Errorf("同步版本文件失败: %w", err)
	}

	// P5 配额对账：Commit(actual)（多预留部分自动归还）。
	if res != nil {
		res.Commit(written)
		res = nil
	}

	// 清理超出上限的旧版本（删除的旧版本按文件大小释放 version 桶 Scope）。
	h.cleanupOldVersions(userRel, tnt, owner)

	h.logger.Info("文件版本已保存", "file_name", userRel, "version_id", versionID)
	return versionID, nil
}

// releaseVersionUsage 释放 version 桶 Scope 中已确认占用的版本文件字节（P5）。
// 删除版本文件后按删除前 stat 的文件大小释放，避免 version 桶 committed 虚高
// 依赖周期扫描自愈。size<=0 时为空操作。
func (h *Handlers) releaseVersionUsage(owner string, size int64) {
	if size <= 0 {
		return
	}
	if scope := h.quotaBucketFor(owner, "version"); scope != nil {
		scope.ReleaseUsage(size)
	}
}

// cleanupOldVersions 删除超出 max_versions 的旧版本。
// userRel 为相对 user 桶的路径；版本文件在 version/<userRel>/ 目录下。
// P5：删除的旧版本按文件大小释放 version 桶 Scope（不依赖周期扫描自愈）。
func (h *Handlers) cleanupOldVersions(userRel string, tnt *storage.Tenant, owner string) {
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.MaxVersions <= 0 {
		return
	}
	if tnt == nil || tnt.Root() == nil {
		return
	}
	root := tnt.Root()
	verDir, ok := tnt.FeatureRel("version", userRel)
	if !ok {
		return
	}
	abs, ok := root.Abs(verDir)
	if !ok {
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return
	}

	if len(entries) <= cfg.Versioning.MaxVersions {
		return
	}

	// 按文件名（UnixNano 时间戳）排序，删除最旧的
	// 使用 ParseInt 解析为 int64 后做数值比较，消除字符串字典序与数值序不一致的隐患。
	// 使用 SliceStable 保持相等元素的原始顺序，避免排序不稳定带来的不确定性。
	sort.SliceStable(entries, func(i, j int) bool {
		vi, erri := strconv.ParseInt(entries[i].Name(), 10, 64)
		vj, errj := strconv.ParseInt(entries[j].Name(), 10, 64)
		if erri != nil && errj != nil {
			return false
		}
		if erri != nil {
			return false
		}
		if errj != nil {
			return true
		}
		return vi < vj
	})
	excess := len(entries) - cfg.Versioning.MaxVersions
	for i := range excess {
		delRel := verDir + "/" + entries[i].Name()
		// 删除旧版本前记录文件大小，删除后释放 version 桶 Scope。
		var delSize int64
		if info, sErr := root.Stat(delRel); sErr == nil {
			delSize = info.Size()
		}
		if err := root.Remove(delRel); err != nil {
			h.logger.Warn("删除旧版本文件失败", "path", delRel, "error", err)
			continue
		}
		h.releaseVersionUsage(owner, delSize)
	}
}

// listVersionsHandler 处理 GET /api/versions?filename=xxx。
func (h *Handlers) listVersionsHandler(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	root := tnt.Root()
	verDir, ok := tnt.FeatureRel("version", remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	abs, ok := root.Abs(verDir)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(abs)
	if os.IsNotExist(err) {
		sendJSONResponse(w, map[string]any{"versions": []VersionInfo{}}, http.StatusOK)
		return
	}
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取版本目录失败"}, http.StatusInternalServerError)
		return
	}

	csStore := h.checksumStoreFor(ownerFromRequest(r))
	versions := make([]VersionInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		versionID, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue
		}

		fi := VersionInfo{
			Filename:  filepath.ToSlash(remotePath),
			VersionID: versionID,
			Size:      info.Size(),
			CreatedAt: time.Unix(0, versionID).Format(time.RFC3339),
		}
		// 尝试获取 checksum（per-tenant store，key = version/<rel>/<id>）
		if csStore != nil {
			csKey := verDir + "/" + e.Name()
			if cs, ok := csStore.Get(csKey); ok {
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

	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	root := tnt.Root()
	verDir, ok := tnt.FeatureRel("version", remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	verRel := verDir + "/" + versionIDStr
	verInfo, err := root.Stat(verRel)
	if os.IsNotExist(err) {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		return
	} else if err != nil {
		h.logger.Error("stat 版本文件失败", "file_name", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "访问版本文件失败"}, http.StatusInternalServerError)
		return
	}

	targetRel, ok := tnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// 先保存当前版本（回滚前备份），备份失败时返回 500 拒绝执行恢复
	if _, err = h.saveVersion(remotePath, tnt, ownerFromRequest(r)); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "恢复前备份失败: " + versionIDStr,
		})
		h.logger.Error("恢复版本前备份失败", "file_name", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复版本前备份失败，已中止"}, http.StatusInternalServerError)
		return
	}

	// P4/P5 配额（I3 修复）：恢复把版本文件拷回 user 桶（O_TRUNC 覆盖当前文件），本质是新增
	// user 桶字节——缺失配额可反复 restore 突破租户上限。与 upload 对齐：TryReserve(版本大小)
	// 预留 → 拷贝成功后 Adjust(prev, actual)（覆盖写）/ Commit(actual)（新文件）；失败 Release()。
	scope := h.quotaBucketFor(ownerFromRequest(r), "user")
	prev := int64(0)
	var res *quota.Reservation
	if scope != nil {
		if st, statErr := root.Stat(targetRel); statErr == nil {
			prev = st.Size()
		}
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

	// 拷贝版本文件到目标位置
	src, err := root.Open(verRel)
	if err != nil {
		if res != nil {
			res.Release()
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "打开版本文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "打开版本文件失败"}, http.StatusInternalServerError)
		return
	}
	defer src.Close()

	dst, err := root.OpenFile(targetRel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		if res != nil {
			res.Release()
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "创建目标文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	written, err := io.Copy(dst, src)
	if err != nil {
		if res != nil {
			res.Release()
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "恢复文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
		return
	}
	if syncErr := dst.Sync(); syncErr != nil {
		if res != nil {
			res.Release()
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "同步文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "同步文件失败"}, http.StatusInternalServerError)
		return
	}

	// P4/P5 配额对账：覆盖写 Adjust(prev, written)；新文件 Commit(written)。
	if res != nil {
		if prev > 0 {
			scope.Adjust(prev, written)
			res.Release()
		} else {
			res.Commit(written)
		}
	}

	// 更新 checksum（per-tenant store，key = user 桶相对路径）
	checksum, err := FileChecksumRoot(root, targetRel)
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

	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgVersioningDisabled}, http.StatusNotImplemented)
		return
	}

	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	root := tnt.Root()
	verDir, ok := tnt.FeatureRel("version", remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	verRel := verDir + "/" + versionIDStr
	// P5 版本桶配额：删除前记录文件大小，删除成功后释放 version 桶 Scope。
	var delSize int64
	if info, sErr := root.Stat(verRel); sErr == nil {
		delSize = info.Size()
	}
	if err := root.Remove(verRel); err != nil {
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
	h.releaseVersionUsage(ownerFromRequest(r), delSize)

	// 清理 checksumStore 中对应的版本记录（key = version/<rel>/<id>，无 owner 前缀）
	if cs := h.checksumStoreFor(ownerFromRequest(r)); cs != nil {
		cs.Delete(verDir + "/" + versionIDStr)
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_delete", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + versionIDStr,
	})
	sendJSONResponse(w, UploadResponse{Success: true, Message: "版本已删除"}, http.StatusOK)
}

// saveVersionBeforeOverwrite 在文件即将被覆盖前保存旧版本。
// 在 upload handler 中调用，如果版本管理启用则保存当前版本（按请求者租户根隔离）。
func (h *Handlers) saveVersionBeforeOverwrite(r *http.Request, remotePath string) {
	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		return
	}
	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		h.logger.Warn("saveVersionBeforeOverwrite: 租户不可用", "remote_path", remotePath)
		return
	}
	fullRel, ok := tnt.UserRel(remotePath)
	if !ok {
		h.logger.Warn("saveVersionBeforeOverwrite: 无效路径", "remote_path", remotePath)
		return
	}
	if _, err := tnt.Root().Stat(fullRel); err != nil {
		if os.IsNotExist(err) {
			return
		}
		h.logger.Warn("saveVersionBeforeOverwrite: 检查文件失败", "remote_path", remotePath, "error", err)
		return
	}
	userRel := strings.TrimPrefix(fullRel, tnt.UserRoot()+"/")
	if _, err := h.saveVersion(userRel, tnt, ownerFromRequest(r)); err != nil {
		h.logger.Warn("保存文件版本失败", "file_name", remotePath, "error", err)
	}
}
