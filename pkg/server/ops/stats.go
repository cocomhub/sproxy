// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ops

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

type DiskUsageStats struct {
	StorageRoot string `json:"storage_root"`
	TotalFiles  int    `json:"total_files"`
	TotalSize   int64  `json:"total_size"`
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

	// 配额水位（roadmap P2 配额预警残余）：认证用户 → 本租户使用量/上限/水位。
	Quota QuotaStatus `json:"quota"`
}

// QuotaStatus 是 per-owner 配额水位（admin 空 owner = 全局聚合）。
type QuotaStatus struct {
	Usage     int64 `json:"usage"`
	MaxBytes  int64 `json:"max_bytes"`
	Watermark int   `json:"watermark"` // 百分比 0-100；MaxBytes<=0（不限）= 0
}

// QuotaStatusOf 计算配额水位（nil scope 或 MaxBytes<=0 = 不限 → watermark 0）。
func QuotaStatusOf(scope *quota.Scope) QuotaStatus {
	if scope == nil {
		return QuotaStatus{}
	}
	usage := scope.Usage()
	maxB := scope.MaxBytes()
	wm := 0
	if maxB > 0 {
		wm = int(usage * 100 / maxB)
	}
	return QuotaStatus{Usage: usage, MaxBytes: maxB, Watermark: wm}
}

// IsStorageBucket 判断是否为新布局功能桶名（user/cloud/archive/chunk/version/meta）。
func IsStorageBucket(bucket string) bool {
	switch bucket {
	case "user", "cloud", "archive", "chunk", "version", "meta", "backup":
		return true
	}
	return false
}

// BucketOf 返回统计遍历路径的桶段：兼容两种遍历根——
//   - 租户根相对路径（user/f.txt、version/doc/v1）：首段即功能桶名；
//   - 存储根相对路径（alice/user/f.txt）：第 2 段为功能桶名（复用 capacity.StorageBucketOf）。
//
// 已知功能桶名优先按首段识别（租户根遍历），否则回退 capacity.StorageBucketOf；未知返回 ""。
func BucketOf(rel string) string {
	if before, _, ok := strings.Cut(rel, "/"); ok {
		if IsStorageBucket(before) {
			return before
		}
	} else if IsStorageBucket(rel) {
		return rel
	}
	return capacity.StorageBucketOf(rel)
}

// walkUploadStats 遍历 root 统计用户文件数与总大小。
// 新布局桶语义（storage.Root）：只统计 user/ 桶内的文件；跳过其它功能桶（cloud/archive/
// chunk/version/meta）、遗留 .__ 魔法目录、.checksums.json 与 LAYOUT_VERSION。
// 旧布局平铺文件（无桶结构）按用户文件计入。
func WalkUploadStats(root string, logger *slog.Logger) (totalFiles int, totalSize int64) {
	acc := &uploadStatsAccumulator{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		return walkUploadStatsEntry(logger, root, path, d, err, acc)
	})
	return acc.files, acc.size
}

// uploadStatsAccumulator 是 walkUploadStats 的累计器（文件数 + 字节）。
type uploadStatsAccumulator struct {
	files int
	size  int64
}

// walkUploadStatsEntry 是 walkUploadStats 的 WalkDir 回调实现：按桶/文件类型判定是否
// 计入用户文件统计（.checksums.json 与 LAYOUT_VERSION 跳过、其它功能桶跳过）。
func walkUploadStatsEntry(logger *slog.Logger, root, path string, d os.DirEntry, err error, acc *uploadStatsAccumulator) error {
	if err != nil {
		logger.Warn("stats: WalkDir 遍历错误，跳过", "path", path, "error", err)
		return nil
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr != nil {
		return nil
	}
	relSlash := filepath.ToSlash(rel)
	if d.IsDir() {
		// 跳过遗留 .__ 魔法目录（P5 后旧布局不再产生）与用户不可见内部目录。
		if strings.HasPrefix(d.Name(), ".__") || strings.HasPrefix(d.Name(), "__") {
			return filepath.SkipDir
		}
		// 新布局功能桶目录（非 user）在目录层直接跳过。
		if b := BucketOf(relSlash); IsStorageBucket(b) && b != "user" {
			return filepath.SkipDir
		}
		return nil
	}
	if d.Name() == "checksums.json" {
		return nil
	}
	info, err := d.Info()
	if err != nil {
		logger.Warn("stats: 获取文件信息失败，跳过", "path", path, "error", err)
		return nil
	}
	b := BucketOf(relSlash)
	switch {
	case b == "user":
		// 用户桶文件计入。
	case IsStorageBucket(b):
		// 其它功能桶（cloud/archive/chunk/version/meta）不计入用户文件数/大小。
		return nil
	case d.Name() == "LAYOUT_VERSION":
		// 存储根/租户根的布局版本标记（storage.OpenRoot 写入）不计入。
		return nil
	default:
		// 旧布局平铺用户文件计入。
	}
	acc.files++
	acc.size += info.Size()
	return nil
}

// walkUploadStatsByCategory 遍历 root 按新布局桶前缀分类统计用户文件与分类用量
// （chunked/versions/cloud）。user/→userFiles、cloud/+archive/→cloud、chunk/→chunked、
// version/→versions；meta/ 与遗留 .__ 魔法目录跳过。无桶结构的旧布局平铺文件按
// 用户文件计入。
func WalkUploadStatsByCategory(root string, logger *slog.Logger) (userFiles, chunked, versions, cloud int64) {
	acc := &uploadCategoryAccum{}
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		return walkUploadStatsCategoryEntry(logger, root, path, d, err, acc)
	})
	return acc.userFiles, acc.chunked, acc.versions, acc.cloud
}

// uploadCategoryAccum 是 walkUploadStatsByCategory 的累计器（四类用量字节）。
type uploadCategoryAccum struct {
	userFiles, chunked, versions, cloud int64
}

// walkUploadStatsCategoryEntry 是 walkUploadStatsByCategory 的 WalkDir 回调：按桶前缀
// 分类累加 user/cloud/archive/chunk/version 字节，跳过 meta 与 .__ 魔法目录。
func walkUploadStatsCategoryEntry(logger *slog.Logger, root, path string, d os.DirEntry, err error, acc *uploadCategoryAccum) error {
	if err != nil {
		logger.Warn("stats: WalkDir 遍历错误，跳过", "path", path, "error", err)
		return nil
	}
	if d.IsDir() {
		// 遗留 .__ 魔法目录与 meta 桶不计入配额（对齐 pkg/storage/capacity 扫描）。
		if strings.HasPrefix(d.Name(), ".__") || strings.HasPrefix(d.Name(), "__") {
			return filepath.SkipDir
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr == nil && BucketOf(filepath.ToSlash(rel)) == "meta" {
			return filepath.SkipDir
		}
		return nil
	}
	if d.Name() == "checksums.json" {
		return nil
	}
	info, err := d.Info()
	if err != nil {
		logger.Warn("stats: 获取文件信息失败，跳过", "path", path, "error", err)
		return nil
	}
	rel, relErr := filepath.Rel(root, path)
	if relErr != nil {
		return nil
	}
	rel = filepath.ToSlash(rel)
	size := info.Size()
	switch bucket := BucketOf(rel); bucket {
	case "user":
		acc.userFiles += size
	case "cloud", "archive":
		acc.cloud += size
	case "chunk":
		acc.chunked += size
	case "version":
		acc.versions += size
	case "meta":
		// 目录层已 SkipDir，兜底跳过
	case "backup":
		// 备份 manifest 功能桶（服务端内部账本）：不计入用户/分类用量。
		// 目录层已由 IsStorageBucket 跳过，此处兜底。
	default:
		// 无桶结构的旧布局平铺文件按用户文件计入（.__ 魔法目录已在目录层跳过）。
		acc.userFiles += size
	}
	return nil
}

// CategoriesFromBuckets 把 UsageByBucket 的 path→bytes 映射聚合为 stats 分类字节数。
// 路径末段即桶名：user/chunk/version 单列，cloud+archive 并入 storage_cloud，meta 忽略。
func CategoriesFromBuckets(buckets map[string]int64) (userFiles, cloud, chunked, versions int64) {
	for path, size := range buckets {
		bucket := path
		if i := strings.LastIndexByte(path, '/'); i >= 0 {
			bucket = path[i+1:]
		}
		switch bucket {
		case "user":
			userFiles += size
		case "cloud", "archive":
			cloud += size
		case "chunk":
			chunked += size
		case "version":
			versions += size
		}
	}
	return userFiles, cloud, chunked, versions
}
