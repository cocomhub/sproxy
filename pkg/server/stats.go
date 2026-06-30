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
	Total int64 `json:"total"`
	Xx2   int64 `json:"2xx"`
	Xx4   int64 `json:"4xx"`
	Xx5   int64 `json:"5xx"`
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
}

// statsHandler 处理 GET /api/stats。
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
			if name == chunkedDirName || name == versionsDirName || name == cloudDirName || name == ".__downloads__" {
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
			Total: m.RequestsTotal.Load(),
			Xx2:   m.Requests2XX.Load(),
			Xx4:   m.Requests4XX.Load(),
			Xx5:   m.Requests5XX.Load(),
		}
	}

	sendJSONResponse(w, resp, http.StatusOK)
}
