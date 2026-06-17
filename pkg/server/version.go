// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
func (h *Handlers) saveVersion(remotePath, uploadsDir string) (int64, error) {
	fullPath := filepath.Join(uploadsDir, remotePath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return 0, nil // 新文件，无需保存版本
	}

	versionID := time.Now().UnixNano()
	verDir := filepath.Join(uploadsDir, versionsDirName, remotePath)
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

	if _, err = io.Copy(dst, src); err != nil {
		os.Remove(verPath)
		return 0, fmt.Errorf("复制版本文件失败: %w", err)
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

	verDir := filepath.Join(uploadsDir, versionsDirName, remotePath)
	entries, err := os.ReadDir(verDir)
	if err != nil {
		return
	}

	if len(entries) <= cfg.Versioning.MaxVersions {
		return
	}

	// 按文件名（时间戳）排序，删除最旧的
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	excess := len(entries) - cfg.Versioning.MaxVersions
	for i := range excess {
		_ = os.Remove(filepath.Join(verDir, entries[i].Name()))
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
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本管理未启用"}, http.StatusNotImplemented)
		return
	}

	verDir := filepath.Join(cfg.UploadsDir, versionsDirName, remotePath)
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
		var versionID int64
		_, _ = fmt.Sscanf(e.Name(), "%d", &versionID)

		fi := VersionInfo{
			Filename:  filepath.ToSlash(remotePath),
			VersionID: versionID,
			Size:      info.Size(),
			CreatedAt: time.Unix(0, versionID).Format(time.RFC3339),
		}
		// 尝试获取 checksum
		csKey := fmt.Sprintf("__version__/%s/%d", remotePath, versionID)
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
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本管理未启用"}, http.StatusNotImplemented)
		return
	}

	verFile := filepath.Join(cfg.UploadsDir, versionsDirName, remotePath, versionIDStr)
	if _, err = os.Stat(verFile); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		return
	}

	targetPath := filepath.Join(cfg.UploadsDir, remotePath)

	// 先保存当前版本（回滚前备份）
	_, _ = h.saveVersion(remotePath, cfg.UploadsDir)

	// 拷贝版本文件到目标位置
	src, err := os.Open(verFile)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "打开版本文件失败"}, http.StatusInternalServerError)
		return
	}
	defer src.Close()

	dst, err := os.Create(targetPath)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
		return
	}

	// 更新 checksum
	checksum, err := fileChecksum(targetPath)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "计算文件校验和失败"}, http.StatusInternalServerError)
		return
	}
	h.checksumStore.Set(remotePath, checksum)

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
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "版本管理未启用"}, http.StatusNotImplemented)
		return
	}

	verFile := filepath.Join(cfg.UploadsDir, versionsDirName, remotePath, versionIDStr)
	if err := os.Remove(verFile); err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "删除版本文件失败"}, http.StatusInternalServerError)
		}
		return
	}

	sendJSONResponse(w, UploadResponse{Success: true, Message: "版本已删除"}, http.StatusOK)
}

// saveVersionBeforeOverwrite 在文件即将被覆盖前保存旧版本。
// 在 upload handler 中调用，如果版本管理启用则保存当前版本。
func (h *Handlers) saveVersionBeforeOverwrite(remotePath string) {
	cfg := h.cfgPtr.Load()
	if !cfg.Versioning.Enabled {
		return
	}
	fullPath := filepath.Join(cfg.UploadsDir, remotePath)
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		return
	}
	if _, err := h.saveVersion(remotePath, cfg.UploadsDir); err != nil {
		h.logger.Warn("保存文件版本失败", "file_name", remotePath, "error", err)
	}
}

// fileChecksum 计算文件的 SHA-256 十六进制摘要。
func fileChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
