// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// DiskUsageStats 磁盘使用统计。
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
}

// isStorageBucket 判断是否为新布局功能桶名（user/cloud/archive/chunk/version/meta）。
func isStorageBucket(bucket string) bool {
	switch bucket {
	case "user", "cloud", "archive", "chunk", "version", "meta":
		return true
	}
	return false
}

// statsBucketOf 返回统计遍历路径的桶段：兼容两种遍历根——
//   - 租户根相对路径（user/f.txt、version/doc/v1）：首段即功能桶名；
//   - 存储根相对路径（alice/user/f.txt）：第 2 段为功能桶名（复用 storageBucketOf）。
//
// 已知功能桶名优先按首段识别（租户根遍历），否则回退 storageBucketOf；未知返回 ""。
func statsBucketOf(rel string) string {
	if before, _, ok := strings.Cut(rel, "/"); ok {
		if isStorageBucket(before) {
			return before
		}
	} else if isStorageBucket(rel) {
		return rel
	}
	return storageBucketOf(rel)
}

// walkUploadStats 遍历 root 统计用户文件数与总大小。
// 新布局桶语义（storage.Root）：只统计 user/ 桶内的文件；跳过其它功能桶（cloud/archive/
// chunk/version/meta）、遗留 .__ 魔法目录、.checksums.json 与 LAYOUT_VERSION。
// 旧布局平铺文件（无桶结构）按用户文件计入。
func (h *Handlers) walkUploadStats(root string) (totalFiles int, totalSize int64) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			h.logger.Warn("stats: WalkDir 遍历错误，跳过", "path", path, "error", err)
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if d.IsDir() {
			// 跳过遗留 .__ 魔法目录（P5 后旧布局不再产生）与用户不可见内部目录。
			if strings.HasPrefix(d.Name(), ".__") {
				return filepath.SkipDir
			}
			// 新布局功能桶目录（非 user）在目录层直接跳过。
			if b := statsBucketOf(relSlash); isStorageBucket(b) && b != "user" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "checksums.json" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			h.logger.Warn("stats: 获取文件信息失败，跳过", "path", path, "error", err)
			return nil
		}
		b := statsBucketOf(relSlash)
		switch {
		case b == "user":
			// 用户桶文件计入。
		case isStorageBucket(b):
			// 其它功能桶（cloud/archive/chunk/version/meta）不计入用户文件数/大小。
			return nil
		case d.Name() == "LAYOUT_VERSION":
			// 存储根/租户根的布局版本标记（storage.OpenRoot 写入）不计入。
			return nil
		default:
			// 旧布局平铺用户文件计入。
		}
		totalFiles++
		totalSize += info.Size()
		return nil
	})
	return totalFiles, totalSize
}

// walkUploadStatsByCategory 遍历 root 按新布局桶前缀分类统计用户文件与分类用量
// （chunked/versions/cloud）。user/→userFiles、cloud/+archive/→cloud、chunk/→chunked、
// version/→versions；meta/ 与遗留 .__ 魔法目录跳过。无桶结构的旧布局平铺文件按
// 用户文件计入。
func (h *Handlers) walkUploadStatsByCategory(root string) (userFiles, chunked, versions, cloud int64) {
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			h.logger.Warn("stats: WalkDir 遍历错误，跳过", "path", path, "error", err)
			return nil
		}
		if d.IsDir() {
			// 遗留 .__ 魔法目录与 meta 桶不计入配额（对齐 storage_manager 扫描）。
			if strings.HasPrefix(d.Name(), ".__") {
				return filepath.SkipDir
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr == nil && statsBucketOf(filepath.ToSlash(rel)) == "meta" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == "checksums.json" {
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
		switch bucket := statsBucketOf(rel); bucket {
		case "user":
			userFiles += size
		case "cloud", "archive":
			cloud += size
		case "chunk":
			chunked += size
		case "version":
			versions += size
		case "meta":
			// 目录层已 SkipDir，兜底跳过
		default:
			// 无桶结构的旧布局平铺文件按用户文件计入（.__ 魔法目录已在目录层跳过）。
			userFiles += size
		}
		return nil
	})
	return userFiles, chunked, versions, cloud
}

// statsCategoriesFromBuckets 把 UsageByBucket 的 path→bytes 映射聚合为 stats 分类字节数。
// 路径末段即桶名：user/chunk/version 单列，cloud+archive 并入 storage_cloud，meta 忽略。
func statsCategoriesFromBuckets(buckets map[string]int64) (userFiles, cloud, chunked, versions int64) {
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

// statsRootFor 返回 stats 遍历的根目录：认证用户 → 租户根（<storageRoot>/<owner>/）；
// admin（空 owner）→ 存储根（<storageRoot>/）。globalRoot 未装配时回退 StorageRoot()。
func (h *Handlers) statsRootFor(owner string) string {
	if owner != "" {
		if tnt := h.tenantFor(owner); tnt != nil {
			if abs, ok := tnt.Root().Abs(""); ok {
				return abs
			}
		}
		// 租户不可用（globalRoot 未装配的旧测试装配 / 非法 owner fail-closed）时，
		// 按段名校验派生旧布局路径（<storageRoot>/<owner>/）；非法 owner 返回空（无统计根）。
		if !storage.ValidSegmentName(owner) {
			return ""
		}
		if cfg := h.cfgPtr.Load(); cfg != nil {
			return filepath.Join(cfg.StorageRoot, owner)
		}
		return ""
	}
	if h.globalRoot != nil {
		if abs, ok := h.globalRoot.Abs(""); ok {
			return abs
		}
	}
	if cfg := h.cfgPtr.Load(); cfg != nil {
		return cfg.StorageRoot
	}
	return ""
}

// statsHandler 处理 GET /api/stats。
// 文件数/总大小通过轻量 WalkDir 遍历获取（仅统计 user 桶用户文件，跳过内部桶）。
// 存储分类用量：认证用户（owner 非空）→ 本租户 quota Scope 的 Usage()/UsageByBucket()
// （防跨租户泄露）；空 owner（admin）→ GlobalPool.Usage() + UsageByBucket() 聚合
// （运维指标）。未装配 quota/globalPool 时回退磁盘遍历分类（旧测试/旧装配兼容）。
func (h *Handlers) statsHandler(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()
	m := h.metrics

	owner := ActorFrom(r.Context())
	root := h.statsRootFor(owner)
	totalFiles, totalSize := h.walkUploadStats(root)

	resp := StatsResponse{
		DiskUsage: DiskUsageStats{
			StorageRoot: cfg.StorageRoot,
			TotalFiles:  totalFiles,
			TotalSize:   totalSize,
		},
	}

	if h.storageMgr != nil {
		if scannedAt := h.storageMgr.LastScanTime(); scannedAt == nil {
			sendJSONResponse(w, map[string]any{
				"success": false, "message": "存储统计尚未完成首次扫描，请稍后重试",
			}, http.StatusServiceUnavailable)
			return
		} else {
			resp.ScannedAt = scannedAt
		}
	}

	if owner != "" {
		// 认证用户：分类用量与总用量按本租户 quota Scope 归集（防跨租户泄露）。
		if scope := h.quotaFor(owner); scope != nil {
			uf, cl, ch, ve := statsCategoriesFromBuckets(scope.UsageByBucket())
			resp.StorageUserFiles = uf
			resp.StorageCloud = cl
			resp.StorageChunked = ch
			resp.StorageVersions = ve
			resp.StorageUsage = scope.Usage()
		} else {
			// 未装配 quota：回退磁盘遍历分类。
			uf, ch, ve, cl := h.walkUploadStatsByCategory(root)
			resp.StorageUserFiles = uf
			resp.StorageCloud = cl
			resp.StorageChunked = ch
			resp.StorageVersions = ve
			resp.StorageUsage = uf + cl + ch + ve
		}
	} else {
		// admin：全局聚合（globalPool 权威；storageMgr 回退）。
		if h.globalPool != nil {
			uf, cl, ch, ve := statsCategoriesFromBuckets(h.globalPool.UsageByBucket())
			resp.StorageUserFiles = uf
			resp.StorageCloud = cl
			resp.StorageChunked = ch
			resp.StorageVersions = ve
			resp.StorageUsage = h.globalPool.Usage()
		} else if h.storageMgr != nil {
			usageByCat := h.storageMgr.UsageByCategory()
			resp.StorageUserFiles = usageByCat[CategoryUserFiles]
			resp.StorageChunked = usageByCat[CategoryChunked]
			resp.StorageVersions = usageByCat[CategoryVersions]
			resp.StorageCloud = usageByCat[CategoryCloud]
			resp.StorageUsage = h.storageMgr.Usage()
		}
	}

	// MaxStorageBytes 与 ScannedAt：优先 globalPool（P4 权威），回退 storageMgr。
	if h.globalPool != nil {
		resp.MaxStorageBytes = h.globalPool.MaxBytes()
	} else if h.storageMgr != nil {
		resp.MaxStorageBytes = h.storageMgr.MaxBytes()
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
	total, free, used, err := diskStats(cfg.StorageRoot)
	if err != nil {
		h.logger.Warn("stats: 获取磁盘统计失败", "error", err)
	} else {
		resp.DiskTotal = total
		resp.DiskFree = free
		resp.DiskUsed = used
	}

	sendJSONResponse(w, resp, http.StatusOK)
}
