// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DiskUsageStats 磁盘使用统计。
type DiskUsageStats struct {
	UploadsDir string `json:"uploads_dir"`
	TotalFiles int    `json:"total_files"`
	TotalSize  int64  `json:"total_size"`
}

// RequestCounts 请求计数统计。
type RequestCounts struct {
	Total     int64 `json:"total"`
	Status2xx int64 `json:"2xx"`
	Status4xx int64 `json:"4xx"`
	Status5xx int64 `json:"5xx"`
}

// StatsResponse 是 GET /api/stats 的响应体。
type StatsResponse struct {
	DiskUsage       DiskUsageStats `json:"disk_usage"`
	RequestCounts   RequestCounts  `json:"request_counts"`
	ActiveConns     int64          `json:"active_connections"`
	FilesUploaded   int64          `json:"files_uploaded"`
	FilesDownloaded int64          `json:"files_downloaded"`
	FilesDeleted    int64          `json:"files_deleted"`
	BytesUploaded   int64          `json:"bytes_uploaded"`
	BytesDownloaded int64          `json:"bytes_downloaded"`

	// 存储空间统计
	MaxStorageBytes  int64      `json:"max_storage_bytes"`
	StorageUsage     int64      `json:"storage_usage"`
	StorageUserFiles int64      `json:"storage_user_files"`
	StorageChunked   int64      `json:"storage_chunked"`
	StorageVersions  int64      `json:"storage_versions"`
	StorageCloud     int64      `json:"storage_cloud"`
	ScannedAt        *time.Time `json:"scanned_at"`

	// 磁盘统计
	DiskTotal int64 `json:"disk_total"`
	DiskFree  int64 `json:"disk_free"`
	DiskUsed  int64 `json:"disk_used"`
}

// walkUploadStats 遍历 root 统计用户文件数与总大小，跳过任意层级出现的服务端内部
// 目录（.__cloud__/.__versions__/.__chunked__ 等，含 owner 子目录下的嵌套）与
// .checksums.json。审查 #5：此前仅按名字/根层跳过，owner 子目录下的版本文件被误计。
func (h *Handlers) walkUploadStats(root string) (totalFiles int, totalSize int64) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			h.logger.Warn("stats: WalkDir 遍历错误，跳过", "path", path, "error", err)
			return nil
		}
		if d.IsDir() {
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil && isInternalDirPathPrefix(filepath.ToSlash(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".checksums.json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			h.logger.Warn("stats: 获取文件信息失败，跳过", "path", path, "error", err)
			return nil
		}
		totalFiles++
		totalSize += info.Size()
		return nil
	})
	return totalFiles, totalSize
}

// walkUploadStatsByCategory 遍历 root 统计用户文件与分类用量（chunked/versions/cloud）。
// 与 walkUploadStats 不同：不跳过内部目录，而是按 rel 路径分类计数（对齐 StorageManager
// 分类语义）；跳过服务端任务状态持久化目录（.__downloads__/.__sync__）。
// 多租户（审查 M5 收敛）：认证用户 stats 的分类字段应只含自己 owner 根下的用量——
// cloud 分类在 owner 根下恒为 0（云任务文件存全局 .__cloud__，与 owner 根无关）。
func (h *Handlers) walkUploadStatsByCategory(root string) (userFiles, chunked, versions, cloud int64) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			h.logger.Warn("stats: WalkDir 遍历错误，跳过", "path", path, "error", err)
			return nil
		}
		if d.IsDir() {
			// 服务端任务状态持久化目录不计入配额（对齐 storage_manager 扫描）
			if base := d.Name(); base == downloadsDirName || base == ".__sync__" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".checksums.json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			h.logger.Warn("stats: 获取文件信息失败，跳过", "path", path, "error", err)
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		size := info.Size()
		switch {
		case hasInternalDirAtAnyDepth(rel, chunkedDirName):
			chunked += size
		case hasInternalDirAtAnyDepth(rel, versionsDirName):
			versions += size
		case strings.HasPrefix(rel, cloudDirName+"/"):
			cloud += size
		case strings.HasPrefix(rel, downloadsDirName+"/"):
			cloud += size
		case strings.HasPrefix(rel, cloudArchiveDirName+"/"):
			cloud += size
		default:
			userFiles += size
		}
		return nil
	})
	return userFiles, chunked, versions, cloud
}

// statsHandler 处理 GET /api/stats。
// 文件数/总大小通过轻量 WalkDir 遍历获取（仅统计用户文件，跳过内部目录），确保实时准确性。
// 各分类存储使用量由 StorageManager 缓存提供（已由定期扫描校准），避免每次请求遍历全目录计算分类大小。
// 首次扫描完成前，storageMgr 相关字段返回 503 Service Unavailable。
//
// 多租户（审查 M5）：owner 非空（普通认证用户）只统计自己 owner 子目录的文件数/大小，
// 且分类用量字段（storage_user_files/chunked/versions/cloud）也按 owner 根作用域计算，
// 避免跨租户元数据泄露（他人用量不可见）；空 owner（admin/未认证）仍统计全局总目录
// 与 storageMgr 全局缓存（运维指标，快速路径）。
func (h *Handlers) statsHandler(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()
	m := h.metrics

	// 遍历目录统计文件数和总大小，跳过版本目录、分块目录、checksum 文件
	// 注意：内部目录通过 filepath.SkipDir 跳过，无需再用 strings.Contains 二次过滤
	owner := ActorFrom(r.Context())
	root := cfg.UploadsDir
	if owner != "" {
		root = h.ownerUploadsDirFor(owner)
	}
	totalFiles, totalSize := h.walkUploadStats(root)

	scannedAt := h.storageMgr.LastScanTime()
	if scannedAt == nil {
		sendJSONResponse(w, map[string]any{
			"success": false, "message": "存储统计尚未完成首次扫描，请稍后重试",
		}, http.StatusServiceUnavailable)
		return
	}

	resp := StatsResponse{
		DiskUsage: DiskUsageStats{
			UploadsDir: cfg.UploadsDir,
			TotalFiles: totalFiles,
			TotalSize:  totalSize,
		},
		MaxStorageBytes: h.storageMgr.MaxBytes(),
		ScannedAt:       scannedAt,
	}

	// 分类用量：认证用户按 owner 根 WalkDir 分类（防跨租户泄露）；空 owner 用全局缓存。
	if owner != "" {
		userFiles, chunked, versions, cloud := h.walkUploadStatsByCategory(root)
		resp.StorageUserFiles = userFiles
		resp.StorageChunked = chunked
		resp.StorageVersions = versions
		resp.StorageCloud = cloud
		resp.StorageUsage = userFiles + chunked + versions + cloud
	} else {
		usageByCat := h.storageMgr.UsageByCategory()
		resp.StorageUserFiles = usageByCat[CategoryUserFiles]
		resp.StorageChunked = usageByCat[CategoryChunked]
		resp.StorageVersions = usageByCat[CategoryVersions]
		resp.StorageCloud = usageByCat[CategoryCloud]
		resp.StorageUsage = h.storageMgr.Usage()
	}

	if m != nil {
		resp.ActiveConns = m.ActiveConnections.Load()
		resp.FilesUploaded = m.FilesUploaded.Load()
		resp.FilesDownloaded = m.FilesDownloaded.Load()
		resp.FilesDeleted = m.FilesDeleted.Load()
		resp.BytesUploaded = m.BytesUploaded.Load()
		resp.BytesDownloaded = m.BytesDownloaded.Load()
		resp.RequestCounts = RequestCounts{
			Total:     m.RequestsTotal.Load(),
			Status2xx: m.Requests2XX.Load(),
			Status4xx: m.Requests4XX.Load(),
			Status5xx: m.Requests5XX.Load(),
		}
	}

	// 磁盘统计
	total, free, used, err := diskStats(cfg.UploadsDir)
	if err != nil {
		h.logger.Warn("stats: 获取磁盘统计失败", "error", err)
	} else {
		resp.DiskTotal = total
		resp.DiskFree = free
		resp.DiskUsed = used
	}

	sendJSONResponse(w, resp, http.StatusOK)
}
