// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
)

// StatsResponse 是 GET /api/stats 的响应结构体。
type StatsResponse struct {
	DiskUsage struct {
		UploadsDir string `json:"uploads_dir"`
		TotalFiles int64  `json:"total_files"`
		TotalSize  int64  `json:"total_size"`
	} `json:"disk_usage"`
	RequestCounts struct {
		Total     int64 `json:"total"`
		Status2xx int64 `json:"2xx"`
		Status4xx int64 `json:"4xx"`
		Status5xx int64 `json:"5xx"`
	} `json:"request_counts"`
	ActiveConns      int64 `json:"active_connections"`
	FilesUploaded    int64 `json:"files_uploaded"`
	FilesDownloaded  int64 `json:"files_downloaded"`
	FilesDeleted     int64 `json:"files_deleted"`
	BytesUploaded    int64 `json:"bytes_uploaded"`
	BytesDownloaded  int64 `json:"bytes_downloaded"`
	MaxStorageBytes  int64 `json:"max_storage_bytes"`
	StorageUsage     int64 `json:"storage_usage"`
	StorageUserFiles int64 `json:"storage_user_files"`
	StorageChunked   int64 `json:"storage_chunked"`
	StorageVersions  int64 `json:"storage_versions"`
	StorageCloud     int64 `json:"storage_cloud"`
	DiskTotal        int64 `json:"disk_total"`
	DiskFree         int64 `json:"disk_free"`
	DiskUsed         int64 `json:"disk_used"`
}

// GetStats 查询服务器统计信息。
func (c *FileClient) GetStats(ctx context.Context) (*StatsResponse, error) {
	var stats StatsResponse
	if err := c.doJSON(ctx, "GET", "/api/stats", nil, &stats); err != nil {
		return nil, fmt.Errorf("获取统计信息失败: %w", err)
	}
	return &stats, nil
}
