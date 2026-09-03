// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// segNameOfBucketPath 取 bucket_limits 路径键最后一段（段名校验用）。
// 仅校验段名合法性——首段与功能桶根同名已由 Validate 显式拒绝（防覆盖）。
func segNameOfBucketPath(path string) string {
	return filepath.Base(path)
}

// quota_reconcile.go 实现启动/周期扫描后的 per-tenant 配额 Scope 校准：
// ScanAndRecalculate 把磁盘按租户桶归集的字节数交给 reconcileQuotaScopes，
// 通过 quota.Scope.Adjust 把各桶 committed 校准到磁盘实际占用（重启后 Scope 不回溯）。

// 校准语义：
//   - 先深后浅每键 scope.Adjust(scope.Usage(), diskSize) 收敛到磁盘实际（Adjust 净差、
//     adjustUp 沿父链传播，父层会吸收子层 delta——串联数学使各键 committed 恰为磁盘字节，
//     无双计，见上）；
//   - 有在途预留（Reserved>0）或子层已 skip 时本键跳过（磁盘 partial 已计入 reserved，
//     此时校准 committed 会造成双计）；skip 整体传播到前缀祖先；
//   - 注意"读-修正"两拍非原子（先读 scope.Usage() 再 Adjust diff）：对同一键同时有写路径
//     Commit/ReleaseUsage 时可能欠校/过校。磁盘与 Scope 在下次扫描自动收敛（扫描幂等），
//     且写路径与 reconcile 共用同一把 Scope 锁（adjustUp/reserveUp 锁内操作），故仅存的
//     竞态是"reconcile 读到 Usage 后、Adjust 前"写路径已落账——下次扫描自愈。如需强原子
//     可引入 SetCommittedTo（原子写 commanded=磁盘值，TODO：低风险不阻塞）。
func (h *Handlers) reconcileQuotaScopes(tenantBuckets map[string]map[string]int64) {
	// 先确保所有涉及租户装配了 quota BucketLimits 段树（lazy 装配：未触碰过的租户在
	// configuredBucketLimitKeys 前为空，导致子目录键缺失、校准退化为功能桶级）。装配由
	// ensureTenantQuotaLocked 完成（含 BucketLimits 子 Scope 懒建）。
	for tenant := range tenantBuckets {
		h.ensureTenantQuotaLocked(tenant)
	}
	for tenant, buckets := range tenantBuckets {
		// 仅校准合法租户（避免 legacy 目录/非法段名被当作租户建 Scope）。
		if !storage.ValidSegmentName(tenant) {
			continue
		}
		// 功能桶（quotaBucketNames）+ 该租户装配的 BucketLimits 子目录键（段深升序）。
		keys := append([]string{}, quotaBucketNames...)
		keys = append(keys, h.configuredBucketLimitKeys(tenant)...)
		// 先深后浅：子目录键（段深大）先于功能桶（段深 1）与无前缀键。
		sort.SliceStable(keys, func(i, j int) bool {
			return depthOfKey(keys[i]) > depthOfKey(keys[j])
		})
		// 按键校准；被 skip 的键及其前缀祖先全部跳过（已校准父层会因 skip 吸收 diff 双计，
		// 故跳过时标记该键"不可用"传播——实现为调整顺序：子层 skip 后父层不再 Adjust）。
		skipped := make(map[string]bool)
		for _, key := range keys {
			if skipped[key] {
				continue
			}
			scope := h.quotaBucketFor(tenant, key)
			if scope == nil {
				continue
			}
			if scope.Reserved() > 0 || h.anyChildSkipped(key, skipped) {
				// 在途预留或子层已被 skip → 本键及其前缀祖先跳过（双计保护）。
				skipped[key] = true
				if pfx := parentKeyOf(key); pfx != "" {
					skipped[pfx] = true
				}
				continue
			}
			diskSize := buckets[key]
			scope.Adjust(scope.Usage(), diskSize)
		}
	}
}

// configuredBucketLimitKeys 返回该租户装配的 BucketLimits 路径键（仅 user 子树，按
// quotaBuckets map 中路径键集合；未装配租户返回空）。
func (h *Handlers) configuredBucketLimitKeys(tenant string) []string {
	h.tenantMu.Lock()
	defer h.tenantMu.Unlock()
	buckets := h.quotaBuckets[tenant]
	if buckets == nil {
		return nil
	}
	var keys []string
	for k := range buckets {
		if strings.HasPrefix(k, "user/") {
			keys = append(keys, k)
		}
	}
	return keys
}

// anyChildSkipped 判断 key 是否有更深层的子键已被 skip（先深后浅序下仅需查已跳过集合）。
func (h *Handlers) anyChildSkipped(key string, skipped map[string]bool) bool {
	for k := range skipped {
		if k == key {
			continue
		}
		if strings.HasPrefix(k, key) {
			return true
		}
	}
	return false
}

// depthOfKey 返回路径键段数（"user" → 1、"user/videos/hd" → 3）。
func depthOfKey(key string) int {
	if key == "" {
		return 0
	}
	return strings.Count(key, "/") + 1
}

// parentKeyOf 返回路径键的父键（"user/videos/hd" → "user/videos"；一级键 → ""）。
func parentKeyOf(key string) string {
	if i := strings.LastIndexByte(key, '/'); i > 0 {
		return key[:i]
	}
	return ""
}
