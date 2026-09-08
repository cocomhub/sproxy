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

// adjustVolumePool 把卷容量池 committed 收敛到该卷磁盘物理占用：逐租户累加**功能桶顶层键**
// （quotaBucketNames：user/cloud/archive/chunk/version/meta）对应值，忽略 bucket_limits 子目录键
// （如 user/videos）与旧布局平铺键。
//
// 为什么只按功能桶键求和：StorageManager 扫描对嵌套 user 文件既累加功能桶键（user）又累加
// 子目录键（user/videos/…，storage_manager.go bucketDirKey）；reconcileQuotaScopes 用「先深后浅 +
// 串联 diff」消重，但卷池没有段树子层，若直接全键求和会把嵌套文件在 user 与 user/videos 两处各计
// 一次（双计 → 卷池虚假占满，T4 spread/容量上限误判）。功能桶键本身已含全部嵌套文件字节，故仅
// 累加功能桶键即得物理占用。
//
// 卷池在途预留 >0 时跳过（与 reconcileQuotaScopes 的双计保护同语义：磁盘 partial 已计入
// reserved，此时校准 committed 会造成双计）。卷池不存在（volSet nil / 未知卷名）时安全跳过。
// 卷池无子层（每卷独立根池，未挂 owner Scope 子层），故池 Usage() 即池自身 committed。
func (h *Handlers) adjustVolumePool(name string, tenantBuckets map[string]map[string]int64) {
	if h.volSet == nil {
		return
	}
	pool := h.volSet.Pool(name)
	if pool == nil || pool.Reserved() > 0 {
		return
	}
	var total int64
	for _, buckets := range tenantBuckets {
		for _, b := range quotaBucketNames {
			total += buckets[b]
		}
	}
	pool.Adjust(pool.Usage(), total)
}

// reconcileVolumePool 是单卷扫描回调（StorageManager 装配形态，RegisterRoutes 绑定默认卷）：
// 把该卷磁盘占用同时校准进 owner 全局 Scope（现有 reconcileQuotaScopes 语义——单卷模式下
// 该卷即 owner 全部占用）与该卷容量池（reconcile 双目标）。多卷跨卷聚合见 reconcileVolumes。
func (h *Handlers) reconcileVolumePool(name string, tenantBuckets map[string]map[string]int64) {
	h.reconcileQuotaScopes(tenantBuckets)
	h.adjustVolumePool(name, tenantBuckets)
}

// reconcileVolumes 是多卷 reconcile 双目标框架：volumeBuckets[卷名][tenant][bucket] = 该卷
// 扫描归集的字节数。
//
//  1. owner 全局 Scope 校准必须用**跨卷合计**——先把各卷按 owner/桶聚合成一份 tenantBuckets，
//     再单次执行现有 reconcileQuotaScopes。不能逐卷 Adjust 同一 Scope：每卷扫描各自回调会把
//     owner Scope 重复校准到最后一份卷的字节（跨卷合计语义丢失）。
//  2. 每卷容量池逐卷各自收敛到该卷磁盘占用（adjustVolumePool，含 Reserved>0 跳过）。
func (h *Handlers) reconcileVolumes(volumeBuckets map[string]map[string]map[string]int64) {
	agg := make(map[string]map[string]int64)
	for _, tenantBuckets := range volumeBuckets {
		for tenant, buckets := range tenantBuckets {
			dest := agg[tenant]
			if dest == nil {
				dest = make(map[string]int64)
				agg[tenant] = dest
			}
			for bucket, size := range buckets {
				dest[bucket] += size
			}
		}
	}
	h.reconcileQuotaScopes(agg)
	for volName, tenantBuckets := range volumeBuckets {
		h.adjustVolumePool(volName, tenantBuckets)
	}
}

// reconcileVolumesFromDisk 逐卷扫描全部卷根（scanStorageDir，与 StorageManager 同分类逻辑）
// 并把归集喂给 reconcileVolumes（F2：AD-7 重启/周期对账闭合——各卷容量池收敛到物理字节、
// owner 全局 Scope 收敛到跨卷合计）。volSet nil（旧装配路径）为空操作。RegisterRoutes 多卷
// 装配的 reconciler 直接消费本入口；测试亦可对无 StorageManager 的 Handlers 直接调用。
func (h *Handlers) reconcileVolumesFromDisk() {
	if h.volSet == nil {
		return
	}
	vols := h.volSet.All()
	volumeBuckets := make(map[string]map[string]map[string]int64, len(vols))
	for _, v := range vols {
		buckets, _, err := scanStorageDir(v.RootDir)
		if err != nil {
			h.logger.Error("逐卷扫描失败，跳过该卷校准", "volume", v.Name, "error", err)
			continue
		}
		volumeBuckets[v.Name] = buckets
	}
	h.reconcileVolumes(volumeBuckets)
}
