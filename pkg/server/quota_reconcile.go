// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sort"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	volcapacity "github.com/cocomhub/sproxy/pkg/volume/capacity"
)

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
//     可引入 SetCommittedTo（原子写 commanded=磁盘值）；评估结论是低风险不阻塞，故未实现
//     （审计结论（原待办）——已定，2026-09-14：自愈路径已由本函数的幂等扫描保证）。
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
		h.reconcileTenantQuota(tenant, buckets)
	}
}

// reconcileTenantQuota 校准单个租户的配额 Scope：先深后浅按磁盘实际占用逐键 Adjust。
func (h *Handlers) reconcileTenantQuota(tenant string, buckets map[string]int64) {
	// 功能桶（quotaBucketNames）+ 该租户装配的 BucketLimits 子目录键（段深升序）。
	keys := append([]string{}, quotaBucketNames...)
	keys = append(keys, h.configuredBucketLimitKeys(tenant)...)
	sort.SliceStable(keys, func(i, j int) bool {
		return depthOfKey(keys[i]) > depthOfKey(keys[j])
	})
	// 按键校准；被 skip 的键及其前缀祖先全部跳过（已校准父层会因 skip 吸收 diff 双计，
	// 故跳过时标记该键"不可用"传播——实现为调整顺序：子层 skip 后父层不再 Adjust）。
	skipped := make(map[string]bool)
	for _, key := range keys {
		h.calibrateQuotaKey(tenant, key, buckets, skipped)
	}
}

// calibrateQuotaKey 校准单个配额键：有在途预留或子层已跳过时标记本键及其前缀祖先 skip
// （双计保护）；否则按磁盘实际占用 Adjust（先读 Usage 再 Adjust diff，幂等由下次扫描自愈）。
func (h *Handlers) calibrateQuotaKey(tenant, key string, buckets map[string]int64, skipped map[string]bool) {
	if skipped[key] {
		return
	}
	scope := h.quotaBucketFor(tenant, key)
	if scope == nil {
		return
	}
	if scope.Reserved() > 0 || h.anyChildSkipped(key, skipped) {
		// 在途预留或子层已被 skip → 本键及其前缀祖先跳过（双计保护）。
		skipped[key] = true
		if pfx := parentKeyOf(key); pfx != "" {
			skipped[pfx] = true
		}
		return
	}
	scope.Adjust(scope.Usage(), buckets[key])
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
// 子目录键（user/videos/…，pkg/storage/capacity/manager.go bucketDirKey）；reconcileQuotaScopes 用「先深后浅 +
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
	// **账本修正独立于 pool**（评审 S2）：用户卷无 pool（下方 early-return 前）此前完全
	// 不收敛 ⇒ 查询/强制面（LedgerCounter）恒逻辑口径；此处先行把账本收敛到真值。
	h.correctLedgerToTruth(name)
	pool := h.volSet.Pool(name)
	if pool == nil || pool.Reserved() > 0 {
		return
	}
	pool.Adjust(pool.Usage(), h.truthPreferredVolumeUsage(name, scannedVolumeUsage(tenantBuckets)))
}

// correctLedgerToTruth 把卷账本（LedgerCounter）收敛到真值报告者（评审 S2——此前仅
// pool 收敛、账本仍逻辑口径 ⇒ 展示与强制漂移）。`CorrectFrom` 对无报告者/未接线卷为
// no-op（回落 Used）。
func (h *Handlers) correctLedgerToTruth(name string) {
	rep, ok := h.volumeTrueUsage(name)
	if !ok {
		return
	}
	if lc := h.volumeLedgerCounter(name); lc != nil {
		lc.CorrectFrom(rep)
	}
}

// scannedVolumeUsage 归集卷的磁盘扫描占用（功能桶键之和）。
func scannedVolumeUsage(tenantBuckets map[string]map[string]int64) int64 {
	var total int64
	for _, buckets := range tenantBuckets {
		for _, b := range quotaBucketNames {
			total += buckets[b]
		}
	}
	return total
}

// truthPreferredVolumeUsage 取「真值优先」的卷占用：处理器层（secretdata 等）知道物理
// 落盘字节（内容分块 + meta pad + parity），优先采信并与扫描值交叉校验（差异超容差告警）；
// ok=false（无报告者）回落扫描值，**不静默降级为 0**（设计 2026-10-11 §10 与 §12.1）。
func (h *Handlers) truthPreferredVolumeUsage(name string, total int64) int64 {
	rep, ok := h.volumeTrueUsage(name)
	if !ok {
		return total
	}
	truth, tok := rep.TrueUsage()
	if !tok {
		return total
	}
	if diff := truth - total; diff != 0 && absInt64(diff) > volumeTruthTolerance(total) {
		h.logger.Warn("卷真值与磁盘扫描不一致，以真值为准（差异可能来自目录 meta/孤儿 blob/扫描口径）",
			"volume", name, "truth", truth, "scanned", total, "diff", diff)
	}
	return truth
}

// volumeLedgerCounter 返回某卷的账本计数器（分片日志账本；非 `*capacity.Backend` 装配
// ——封印包装等——返回 nil ⇒ 该卷不参与账本修正/压缩）。
func (h *Handlers) volumeLedgerCounter(name string) *volcapacity.LedgerCounter {
	if h.volSet == nil {
		return nil
	}
	be := h.volSet.External(name)
	if bc, ok := be.(*volcapacity.Backend); ok {
		if lc, ok := bc.Counter().(*volcapacity.LedgerCounter); ok {
			return lc
		}
	}
	return nil
}

// compactVolumeLedger 周期压缩账本分片 + 回收空分片（评审 S1/S3——分片/索引随重启单调增长）。
// best-effort：压缩/回收失败只留日志，不阻塞对账主流程。
func (h *Handlers) compactVolumeLedger(name string) {
	lc := h.volumeLedgerCounter(name)
	if lc == nil {
		return
	}
	if cerr := lc.Compact(); cerr != nil {
		h.logger.Warn("账本分片压缩失败（分片继续增长，对账仍正确）", "volume", name, "error", cerr)
	}
	if rerr := lc.ReapEmptyShards(); rerr != nil {
		h.logger.Warn("账本空分片回收失败", "volume", name, "error", rerr)
	}
}

// volumeTruthTolerance 是「真值 vs 磁盘扫描」交叉校验的容差：绝对下限 1MiB 或扫描值的 5%，
// 取大者。为什么需要容差而不是零偏差：真值口径明确**不含**目录 meta blob、GC 未回收的孤儿
// blob、并发未命中去重留下的冗余分块（见 secretdata/trueusage.go 的不计项说明），这些都会
// 使扫描值略高于真值；容差内的差异属已知边界，不告警。
func volumeTruthTolerance(scanned int64) int64 {
	if scanned < 0 {
		scanned = 0
	}
	return max(int64(1)<<20, scanned/20)
}

// absInt64 返回绝对值（防 MinInt64 溢出对判据无意义——差异量级恒远小于该值）。
func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}

// volumeTrueUsage 取某卷后端 FS 的**真值报告者**（链上最外层实现者，见 capacity.TrueUsageOf）。
// 处理器卷（secretdata 等）都在 registry.Set 的 external 表里（装配经 capacity.WrapBackend）；
// 本地裸卷无处理器层 → 无报告者（其物理真值由磁盘扫描直接给出）。
func (h *Handlers) volumeTrueUsage(name string) (volcapacity.TrueUsageReporter, bool) {
	if h.volSet == nil {
		return nil, false
	}
	be := h.volSet.External(name)
	if be == nil || be.FS() == nil {
		return nil, false
	}
	return volcapacity.TrueUsageOf(be.FS())
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
		// 账本分片压缩 + 空分片回收（评审 S1/S3）：周期对账是自然触发点。
		h.compactVolumeLedger(volName)
	}
}

// reconcileVolumesFromDisk 逐卷扫描全部卷根（capacity.ScanStorageDir，与 StorageManager 同分类逻辑）
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
		// 只扫「有本地根」的卷：外部卷（secrets / secretdata / baidupcs / webdav …）无
		// storage.Root，RootDir 恒为空——其容量由各自后端/容量池语义管理，不参与本地桶扫描。
		// **必须显式跳过**：空串经 capacity.ScanStorageDir 的 filepath.EvalSymlinks("") 在 Unix
		// 归一为 "."（见 symlink.go `if dest == "" { dest = "." }`），WalkDir(".") 会扫描
		// sproxy 进程的**当前工作目录**——实测默认 cwd=/root（5.4G）时启动挂起 >90s，且与卷
		// 语义无关（Windows 侧 EvalSymlinks("") 报错回落空串不扫，故两端表现不一致）。
		// M10（2026-10-06）：封装卷（externalVolumeKey 委托池）容量经底层池**委托子 Scope**
		// 计，不参与本卷池磁盘校准，避免与底层池 reconcile 双计。
		//
		// **取舍记录（评审 A4 Minor #1，勿删——判断依据的载体）**：wrapper 委托池 committed
		// **不随 reconcile 校准**。后果：删除「重启前任一 wrapper 文件」后，委托池的
		// ReleaseCommitted 会钳到本层剩余（可能为 0）→ 底座池不降、overcharge 延续到下次重启
		// （方向保守「宁可拒绝写入」，仅在底层卷设了容量上限时可见）。为何不校准委托子池：
		// 委托池承载的物理字节**已计入底层池校准**（封装卷数据落在底层卷根内），要单独校准
		// 就得按容器拆分子目录字节，反而与底层池形成双计；复杂度/风险高于收益，故按可记录
		// 取舍保留跳过（对账收敛的正确性由写入/删除两侧同口径 + 重启重建兜底）。
		if v.RootDir == "" {
			// **外部/封装卷也要收敛账本**（2026-10-18 修复）：此前直接 `continue` ⇒ 连
			// `correctLedgerToTruth`（账本 → 真值报告者）都不执行；而该函数的**唯一**调用点是
			// `adjustVolumePool`，后者只对出现在 volumeBuckets 里的卷执行 ⇒ 外部/加密卷的
			// LedgerCounter **永不收敛** ✗（展示与强制面长期漂移：高估 ⇒ 提前 507；低估 ⇒ 可超限）。
			// 此处对这类卷**只**做账本真值收敛（不做本地桶扫描——它们没有本地根，扫描语义不适用）；
			// 无真值报告者时 `correctLedgerToTruth` 是 no-op（未接线卷零回归）。
			h.correctLedgerToTruth(v.Name)
			continue
		}
		buckets, _, err := capacity.ScanStorageDir(v.RootDir)
		if err != nil {
			h.logger.Error("逐卷扫描失败，跳过该卷校准", "volume", v.Name, "error", err)
			continue
		}
		volumeBuckets[v.Name] = buckets
	}
	h.reconcileVolumes(volumeBuckets)
}
