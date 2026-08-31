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
	"time"
)

const versionsDirName = ".__versions__"

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
// owner 用于 checksum key 作用域隔离（跨租户同名文件版本独立）。
func (h *Handlers) saveVersion(remotePath, uploadsDir, owner string) (int64, error) {
	fullPath := joinSafePath(uploadsDir, remotePath)
	if fullPath == "" {
		return 0, fmt.Errorf("保存版本: 无效的文件路径: %s", remotePath)
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return 0, nil // 新文件，无需保存版本
	}

	versionID := time.Now().UnixNano()
	// 添加随机后缀（0-999），防止同一纳秒内多个请求产生冲突
	versionID = versionID*1000 + int64(rand.IntN(1000))
	verDir := joinSafePath(uploadsDir, filepath.Join(versionsDirName, remotePath))
	if verDir == "" {
		return 0, fmt.Errorf("保存版本: 无效的版本目录路径: %s/%s", versionsDirName, remotePath)
	}
	if err := os.MkdirAll(verDir, 0755); err != nil {
		return 0, fmt.Errorf("创建版本目录失败: %w", err)
	}

	verPath := filepath.Join(verDir, fmt.Sprintf("%d", versionID))

	src, err := os.Open(fullPath)
	if err != nil {
		return 0, fmt.Errorf("打开源文件失败: %w", err)
	}
	defer src.Close()

	dst, err := os.Create(verPath)
	if err != nil {
		return 0, fmt.Errorf("创建版本文件失败: %w", err)
	}
	defer dst.Close()

	// 流式计算 checksum：一边复制一边计算 SHA-256，避免重复读取
	hasher := sha256.New()
	multiWriter := io.MultiWriter(dst, hasher)
	if _, err = io.Copy(multiWriter, src); err != nil {
		os.Remove(verPath)
		return 0, fmt.Errorf("复制版本文件失败: %w", err)
	}
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// 写入 checksumStore（owner 作用域 key）
	csKey := checksumStoreKey(owner, fmt.Sprintf("__version__/%s/%d", remotePath, versionID))
	h.checksumStore.Set(csKey, checksum)

	// 显式 fsync 版本文件，确保崩溃时不会丢失已保存的版本
	if err := dst.Sync(); err != nil {
		os.Remove(verPath)
		return 0, fmt.Errorf("同步版本文件失败: %w", err)
	}

	// 同步父目录确保目录元数据落盘
	// 注意：syncParentDir 在 Windows 上可能失败（EINVAL/Access Denied），
	// 文件已成功写入磁盘，父目录 sync 是优化而非必要步骤，不应阻断流程。
	if err := syncParentDir(verPath); err != nil {
		h.logger.Warn("同步版本文件父目录失败", "path", verPath, "error", err)
	}

	// 清理超出上限的旧版本
	h.cleanupOldVersions(remotePath, uploadsDir)

	h.logger.Info("文件版本已保存", "file_name", remotePath, "version_id", versionID)
	return versionID, nil
}

// cleanupOldVersions 删除超出 max_versions 的旧版本。
func (h *Handlers) cleanupOldVersions(remotePath, uploadsDir string) {
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.MaxVersions <= 0 {
		return
	}

	verDir := joinSafePath(uploadsDir, filepath.Join(versionsDirName, remotePath))
	if verDir == "" {
		return
	}
	entries, err := os.ReadDir(verDir)
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
		if err := os.Remove(filepath.Join(verDir, entries[i].Name())); err != nil {
			h.logger.Warn("删除旧版本文件失败", "path", filepath.Join(verDir, entries[i].Name()), "error", err)
		}
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

	verDir := h.safePathFor(r, filepath.Join(versionsDirName, remotePath))
	if verDir == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	entries, err := os.ReadDir(verDir)
	if os.IsNotExist(err) {
		sendJSONResponse(w, map[string]any{"versions": []VersionInfo{}}, http.StatusOK)
		return
	}
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取版本目录失败"}, http.StatusInternalServerError)
		return
	}

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
		// 尝试获取 checksum（owner 作用域 key）
		csKey := h.checksumKeyFor(r, fmt.Sprintf("__version__/%s/%d", remotePath, versionID))
		if cs, ok := h.checksumStore.Get(csKey); ok {
			fi.Checksum = cs
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

	verFile := h.safePathFor(r, filepath.Join(versionsDirName, remotePath, versionIDStr))
	if verFile == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	if _, err = os.Stat(verFile); os.IsNotExist(err) {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "版本文件不存在: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		return
	}

	targetPath := h.safePathFor(r, remotePath)
	if targetPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	// 先保存当前版本（回滚前备份），备份失败时返回 500 拒绝执行恢复
	if _, err = h.saveVersion(remotePath, h.ownerUploadsDir(r), ownerFromRequest(r)); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "恢复前备份失败: " + versionIDStr,
		})
		h.logger.Error("恢复版本前备份失败", "file_name", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复版本前备份失败，已中止"}, http.StatusInternalServerError)
		return
	}

	// 拷贝版本文件到目标位置
	src, err := os.Open(verFile)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "打开版本文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "打开版本文件失败"}, http.StatusInternalServerError)
		return
	}
	defer src.Close()

	dst, err := os.Create(targetPath)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "创建目标文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "恢复文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
		return
	}
	if syncErr := dst.Sync(); syncErr != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "同步文件失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "同步文件失败"}, http.StatusInternalServerError)
		return
	}

	// 更新 checksum
	checksum, err := FileChecksum(targetPath)
	if err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "version_restore", ObjectType: "file", Object: remotePath,
			Result: AuditResultError, Detail: "计算文件校验和失败: " + versionIDStr,
		})
		sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件校验和失败"}, http.StatusInternalServerError)
		return
	}
	h.checksumStore.Set(h.checksumKeyFor(r, remotePath), checksum)

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

	verFile := h.safePathFor(r, filepath.Join(versionsDirName, remotePath, versionIDStr))
	if verFile == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}
	if err := os.Remove(verFile); err != nil {
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

	// 清理 checksumStore 中对应的版本记录（owner 作用域 key）
	verKey := h.checksumKeyFor(r, fmt.Sprintf("__version__/%s/%s", remotePath, versionIDStr))
	h.checksumStore.Delete(verKey)

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "version_delete", ObjectType: "file", Object: remotePath,
		Result: AuditResultSuccess, Detail: "version_id=" + versionIDStr,
	})
	sendJSONResponse(w, UploadResponse{Success: true, Message: "版本已删除"}, http.StatusOK)
}

// saveVersionBeforeOverwrite 在文件即将被覆盖前保存旧版本。
// 在 upload handler 中调用，如果版本管理启用则保存当前版本（按 owner 存储根隔离）。
func (h *Handlers) saveVersionBeforeOverwrite(r *http.Request, remotePath string) {
	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		return
	}
	fullPath := h.safePathFor(r, remotePath)
	if fullPath == "" {
		h.logger.Warn("saveVersionBeforeOverwrite: 无效路径", "remote_path", remotePath)
		return
	}
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return
	}
	if _, err := h.saveVersion(remotePath, h.ownerUploadsDir(r), ownerFromRequest(r)); err != nil {
		h.logger.Warn("保存文件版本失败", "file_name", remotePath, "error", err)
	}
}

// syncParentDir 对指定文件/目录的父目录执行 fsync，确保目录元数据落盘。
func syncParentDir(path string) error {
	parent := filepath.Dir(path)
	f, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
