// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// trueusage.go 实现「**让知道真实占用的卷层修正账本**」机制（2026-10-11 用户要求）：
// 容量账本（LedgerCounter/Pool）在正常情况下按**声明/逻辑**大小记账，遇到会改变物理落盘量的
// 卷层（at-rest 加密的 meta pad、去重、纠删码、压缩、封装卷的封装开销…）就会出现
// 「逻辑记账 ≠ 真实占用」的失真（评审实证：加密卷 1GiB 限额实际可落 ~1.5TiB）。
//
// 机制（两条腿，互为兜底）：
//  1. **真值报告（本文件）**：卷装备自己「知道真实占用」的实现（`TrueUsageReporter`），
//     账本在写后结算与周期对账时优先采信真值，而不是估算——真值优于推断；
//  2. **对账修正（既有）**：`pkg/server/quota_reconcile.go` 的磁盘扫描（本地卷）与
//     backend `registry.VolumeStatsProvider`（外部卷，baidupcs 配额 / S3 bucket 用量）
//     把池 `Adjust` 收敛到实测字节；两者都有则**交叉校验**，不一致时真值优先并告警
//     （防「扫描口径遗漏封装开销」导致再次失真）。
//
// 可靠性保证（为什么可信）：
//   - 报告方**必须是掌握该卷物理布局的那一层**（后端/封装处理器），不得由装饰器估算；
//   - 报告可选（`ok=false` = 本层不掌握）→ 调用方回落既有记账，**不静默降级为 0**；
//   - 修正只动 committed（`Pool.Adjust` 沿父链、防负），不触碰在途 reserved（并发安全）；
//   - 修正后仍受上限校验约束（下一次预留按真值判定），超限即拒绝新写。

// TrueUsageReporter 由「知道真实占用的卷层」实现：报告该卷（或其某路径）的**物理字节**占用。
//
// 语义要点：
//   - 物理字节 = 实际落盘量（含密文膨胀/每块头/长度头/meta pad/纠删码 parity 等封装开销）；
//   - 不掌握真值时返回 ok=false（调用方回落估算/扫描），**禁止**用逻辑大小冒充真值；
//   - 实现必须并发安全（写路径与对账可并发调用）；成本应为 O(1) 或 O(索引) ，不得全卷扫描。
type TrueUsageReporter interface {
	// TrueUsage 返回该卷全部真实占用（物理字节）。ok=false = 不掌握。
	TrueUsage() (bytes int64, ok bool)
	// TrueSize 返回单个相对路径的真实占用（物理字节，含其 sidecar/pad 开销）。ok=false = 不掌握。
	TrueSize(rel string) (bytes int64, ok bool)
}

// TrueUsageOf 从（可能是装饰器链的）FS 中取真值报告者：**自外向内**逐层 Inner() 下探，
// 返回**第一个**实现 TrueUsageReporter 的层。
//
// 为什么取「最外层实现者」而不是最内层：掌握物理布局的是**处理器层**（如 secretdata 的
// 内容分块 + meta pad + XOR parity 合计），它在链中间/外层；最内层只是「被包装的底层存储」，
// 它并不知道上层为同一逻辑文件写了几个 blob、pad 了多少字节。装饰器（CapacityFS/
// StagingQuotaGateFS/Guard/quotaLocalFS）**不实现**本接口（它们只有估算语义），遍历天然
// 跳过它们——“不得由装饰器估算冒充真值”由「谁实现了接口」+ R24 门禁保证。
// 找不到实现返回 (nil, false)——调用方必须按「不掌握」处理（回落估算/扫描），不得当作 0。
func TrueUsageOf(fs syncpkg.FS) (TrueUsageReporter, bool) {
	if fs == nil {
		return nil, false
	}
	for cur := fs; cur != nil; {
		if r, ok := cur.(TrueUsageReporter); ok {
			return r, true
		}
		inner, ok := cur.(interface{ Inner() syncpkg.FS })
		if !ok {
			return nil, false
		}
		cur = inner.Inner()
	}
	return nil, false
}

// CorrectTo 把本池已确认占用**修正为真值** bytes（真值优先；可增可减）。
//
// 与 Adjust 同域、同约束（调用方须保证本池无挂载子层 Scope）：只动 committed，
// 不触碰 reserved（在途预留不被误伤），可正可负且防下溢归零（quota.adjustUp 语义），
// 修正后立即持久化快照（best-effort；无 path 为 no-op）。负值按 0 处理。
func (c *LedgerCounter) CorrectTo(bytes int64) {
	if bytes < 0 {
		bytes = 0
	}
	if c == nil || c.l == nil {
		return
	}
	// 账本语义：把**全局和**修正为真值 ⇒ 追加 (真值 − 当前和) 的增量（可正可负）。
	// 不再需要「修正后持久化」：账本每次 Add 即落盘。
	// **必须走唯一临界区**（独立审核 P2，2026-10-18）：此前读 Total 与 Add 在锁外，两个并发
	// CorrectTo 会各自读 cur=0 并各加真值 ⇒ 双计（Used=2×真值）。
	_ = c.withLock(func(cur int64) error {
		delta := bytes - cur
		if delta == 0 {
			return nil
		}
		if err := c.l.Add(delta); err != nil {
			return err
		}
		c.mirrorToPool(delta)
		return nil
	})
}

// CorrectFrom 用真值报告者修正账本，返回修正后的占用与是否发生修正：
//   - reporter == nil 或 ok=false → 不修正（返回当前占用, false）——**不静默降级为 0**；
//   - 修正成功 → (真值, true)。
//
// 这是「卷层修正账本」的推荐入口（装配层/对账路径调用），调用方在 false 时应回落
// 既有对账（磁盘扫描 / backend Stats）。
func (c *LedgerCounter) CorrectFrom(reporter TrueUsageReporter) (int64, bool) {
	if reporter == nil {
		return c.Used(), false
	}
	bytes, ok := reporter.TrueUsage()
	if !ok {
		return c.Used(), false
	}
	c.CorrectTo(bytes)
	return bytes, true
}
