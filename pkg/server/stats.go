// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
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
	MaxStorageBytes  int64 `json:"max_storage_bytes"`
	StorageUsage     int64 `json:"storage_usage"`
	StorageUserFiles int64 `json:"storage_user_files"`
	StorageChunked   int64 `json:"storage_chunked"`
	StorageVersions  int64 `json:"storage_versions"`
	StorageCloud     int64 `json:"storage_cloud"`

	// 磁盘统计
	DiskTotal int64 `json:"disk_total"`
	DiskFree  int64 `json:"disk_free"`
	DiskUsed  int64 `json:"disk_used"`
}

// statsHandler 处理 GET /api/stats。
// 文件数/总大小通过轻量 WalkDir 获取（仅统计用户文件，跳过内部目录）。
// 各分类存储使用量由 StorageManager 提供（已定期扫描缓存），避免每次请求遍历全目录计算分类大小。
func (h *Handlers) statsHandler(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()
	m := h.metrics

	// 遍历目录统计文件数和总大小，跳过版本目录、分块目录、checksum 文件
	totalFiles := 0
	var totalSize int64
	_ = filepath.WalkDir(cfg.UploadsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == chunkedDirName || name == versionsDirName || name == cloudDirName || name == downloadsDirName || name == cloudArchiveDirName {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == ".checksums.json" {
			return nil
		}
		// 跳过版本文件路径（父目录包含 versionsDirName 的文件）
		if strings.Contains(path, versionsDirName) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		totalFiles++
		totalSize += info.Size()
		return nil
	})

	resp := StatsResponse{
		DiskUsage: DiskUsageStats{
			UploadsDir: cfg.UploadsDir,
			TotalFiles: totalFiles,
			TotalSize:  totalSize,
		},
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

	// 存储空间统计 — 从 StorageManager 缓存读取（已由定期扫描校准），避免每次请求遍历全目录计算分类大小
	if h.storageMgr != nil {
		resp.MaxStorageBytes = h.storageMgr.MaxBytes()
		resp.StorageUsage = h.storageMgr.Usage()
		usageByCat := h.storageMgr.UsageByCategory()
		resp.StorageUserFiles = usageByCat[CategoryUserFiles]
		resp.StorageChunked = usageByCat[CategoryChunked]
		resp.StorageVersions = usageByCat[CategoryVersions]
		resp.StorageCloud = usageByCat[CategoryCloud]
	}

	// 磁盘统计
	total, free, used := diskStats(cfg.UploadsDir)
	resp.DiskTotal = total
	resp.DiskFree = free
	resp.DiskUsed = used

	sendJSONResponse(w, resp, http.StatusOK)
}
