// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"path/filepath"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/ops"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

type (
	DiskUsageStats = ops.DiskUsageStats
	RequestCounts  = ops.RequestCounts
	StatsResponse  = ops.StatsResponse
	QuotaStatus    = ops.QuotaStatus
)

// statsRootFor 返回 stats 遍历的根目录：认证用户 → 租户根（<默认卷根>/<owner>/）；
// admin（空 owner）→ 存储根（<默认卷根>/）。globalRoot 未装配时回退
// resolveDefaultVolumeRoot(cfg)（默认卷根——显式 volumes[0].root ≠ storage_root 分叉时
// stats 必须遍历默认卷而非 cfg.StorageRoot，PR-B 终审建议 9）。
//
// 排除面边界（F3 review 成文）：stats 聚合属默认卷 ACL 排除面的**设计内例外**——默认卷被排除时
// 仍返回 owner 默认卷根的聚合总量（文件数/字节，无文件名/内容）。见 volumes.go defaultVolumeAllows
// 边界注释。不改行为，仅记录避免未来误判为漏洞。
func (h *Handlers) statsRootFor(owner string) string {
	if owner != "" {
		if tnt := h.tenantFor(owner); tnt != nil {
			if abs, ok := tnt.Root().Abs(""); ok {
				return abs
			}
		}
		// 租户不可用（globalRoot 未装配的旧测试装配 / 非法 owner fail-closed）时，
		// 按段名校验派生旧布局路径（<默认卷根>/<owner>/）；非法 owner 返回空（无统计根）。
		if !storage.ValidSegmentName(owner) {
			return ""
		}
		if cfg := h.cfgPtr.Load(); cfg != nil {
			return filepath.Join(resolveDefaultVolumeRoot(cfg), owner)
		}
		return ""
	}
	if h.globalRoot != nil {
		if abs, ok := h.globalRoot.Abs(""); ok {
			return abs
		}
	}
	if cfg := h.cfgPtr.Load(); cfg != nil {
		return resolveDefaultVolumeRoot(cfg)
	}
	return ""
}

// statsHandler 处理 GET /api/stats。
// 文件数/总大小通过轻量 WalkDir 遍历获取（仅统计 user 桶用户文件，跳过内部桶）。
// 存储分类用量：认证用户（owner 非空）→ 本租户 quota Scope 的 Usage()/UsageByBucket()
// （防跨租户泄露）；空 owner（admin）→ GlobalPool.Usage() + UsageByBucket() 聚合

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
	totalFiles, totalSize := ops.WalkUploadStats(root, h.logger)

	resp := StatsResponse{
		DiskUsage: DiskUsageStats{
			StorageRoot: resolveDefaultVolumeRoot(cfg),
			TotalFiles:  totalFiles,
			TotalSize:   totalSize,
		},
	}

	if scannedAt, ok := h.statsScannedAt(); !ok {
		sendJSONResponse(w, map[string]any{
			"success": false, "message": "存储统计尚未完成首次扫描，请稍后重试",
		}, http.StatusServiceUnavailable)
		return
	} else if scannedAt != nil {
		resp.ScannedAt = scannedAt
	}

	if owner != "" {
		// 认证用户：分类用量与总用量按 owner 租户 quota Scope 归集（防跨租户泄露）。
		h.fillOwnerStatsUsage(&resp, owner, root)
	} else {
		// admin：全局聚合（quota 权威；storageMgr 回退）。
		h.fillAdminStatsUsage(&resp)
	}
	h.fillMaxBytes(&resp)

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

	// 磁盘统计（默认卷根——分叉配置下 stats 反映默认卷所在磁盘，而非 cfg.StorageRoot）
	// h.log()：直接构造的 Handlers（测试）logger 可能为 nil，归一后再传给 diskStats。
	total, free, used, err := diskStats(r.Context(), resolveDefaultVolumeRoot(cfg), h.log())
	if err != nil {
		h.log().WarnContext(r.Context(), "stats: 获取磁盘统计失败", "error", err)
	} else {
		resp.DiskTotal = total
		resp.DiskFree = free
		resp.DiskUsed = used
	}

	sendJSONResponse(w, resp, http.StatusOK)
}

// statsScannedAt 返回最近一次存储扫描时间与可用标记（storageMgr 未装配 → 可用但无时间）。
func (h *Handlers) statsScannedAt() (*time.Time, bool) {
	if h.storageMgr == nil {
		return nil, true
	}
	scannedAt := h.storageMgr.LastScanTime()
	if scannedAt == nil {
		return nil, false
	}
	return scannedAt, true
}

// fillOwnerStatsUsage 填充认证用户的分类/总量用量（quota Scope 权威；未装配回退磁盘遍历分类）。
func (h *Handlers) fillOwnerStatsUsage(resp *StatsResponse, owner, root string) {
	if scope := h.quotaFor(owner); scope != nil {
		uf, cl, ch, ve := ops.CategoriesFromBuckets(scope.UsageByBucket())
		resp.StorageUserFiles = uf
		resp.StorageCloud = cl
		resp.StorageChunked = ch
		resp.StorageVersions = ve
		resp.StorageUsage = scope.Usage()
		resp.Quota = ops.QuotaStatusOf(scope)
		return
	}
	// 未装配 quota：回退磁盘遍历分类。
	uf, ch, ve, cl := ops.WalkUploadStatsByCategory(root, h.logger)
	resp.StorageUserFiles = uf
	resp.StorageCloud = cl
	resp.StorageChunked = ch
	resp.StorageVersions = ve
	resp.StorageUsage = uf + cl + ch + ve
}

// fillAdminStatsUsage 填充 admin 的全局聚合分类用量（globalPool 权威；storageMgr 回退）。
func (h *Handlers) fillAdminStatsUsage(resp *StatsResponse) {
	if h.globalPool != nil {
		uf, cl, ch, ve := ops.CategoriesFromBuckets(h.globalPool.UsageByBucket())
		resp.StorageUserFiles = uf
		resp.StorageCloud = cl
		resp.StorageChunked = ch
		resp.StorageVersions = ve
		resp.StorageUsage = h.globalPool.Usage()
		return
	}
	if h.storageMgr != nil {
		usageByCat := h.storageMgr.UsageByCategory()
		resp.StorageUserFiles = usageByCat[capacity.CategoryUserFiles]
		resp.StorageChunked = usageByCat[capacity.CategoryChunked]
		resp.StorageVersions = usageByCat[capacity.CategoryVersions]
		resp.StorageCloud = usageByCat[capacity.CategoryCloud]
		resp.StorageUsage = h.storageMgr.Usage()
	}
}

// fillMaxBytes 填充存储上限（优先 globalPool，回退 storageMgr）。
func (h *Handlers) fillMaxBytes(resp *StatsResponse) {
	if h.globalPool != nil {
		resp.MaxStorageBytes = h.globalPool.MaxBytes()
	} else if h.storageMgr != nil {
		resp.MaxStorageBytes = h.storageMgr.MaxBytes()
	}
}
