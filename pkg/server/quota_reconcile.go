// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import "github.com/cocomhub/sproxy/pkg/storage"

// quota_reconcile.go 实现启动/周期扫描后的 per-tenant 配额 Scope 校准：
// ScanAndRecalculate 把磁盘按租户桶归集的字节数交给 reconcileQuotaScopes，
// 通过 quota.Scope.Adjust 把各桶 committed 校准到磁盘实际占用（重启后 Scope 不回溯）。

// reconcileQuotaScopes 按租户桶归集磁盘占用，校准 per-tenant 配额 Scope 的 committed。
// 校准全部参与配额归集的功能桶（user/cloud/archive/chunk/version/meta，meta 为服务端账本
// 桶，任务 3 起随扫描计入；quotaBucketNames 白名单统一遍历）。
// 校准语义：scope.Adjust(prev=当前 committed, next=磁盘字节数) → committed 收敛到磁盘实际。
// 有在途预留（Reserved>0，如未 Commit 的云任务 partial）时跳过该桶——磁盘 partial 已计入
// reserved，此时校准 committed 会造成双计（预留 Commit 后再叠加）。
func (h *Handlers) reconcileQuotaScopes(tenantBuckets map[string]map[string]int64) {
	for tenant, buckets := range tenantBuckets {
		// 仅校准合法租户（避免 legacy 目录/非法段名被当作租户建 Scope）。
		// 段名校验单一权威在 pkg/storage.ValidSegmentName（P5 收敛）。
		if !storage.ValidSegmentName(tenant) {
			continue
		}
		for _, b := range quotaBucketNames {
			scope := h.quotaBucketFor(tenant, b)
			if scope == nil {
				continue
			}
			if scope.Reserved() > 0 {
				// 在途预留：跳过本桶校准（避免把 reserved 的 partial 双计为 committed）。
				continue
			}
			diskSize := buckets[b]
			scope.Adjust(scope.Usage(), diskSize)
		}
	}
}
