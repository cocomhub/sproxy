// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// 块级明文缓存的容量：按**字节预算** LRU 淘汰（默认 64MiB，见 `defaultBlockletCacheBytes`）
// —— 单块最大 4MiB ⇒ 默认约 16 块。
//
// 实现说明（2026-11-12，P2）：淘汰语义已抽到泛型 `budgetLRU`（`budget_lru.go`），
// 本类型退化为**薄包装**，对外 API（`get`/`put`/`invalidatePrefix`/`clear`/`budget`）
// 与行为**逐条保持不变** ⇒ 既有调用点（`erasure.go`/`features.go`）零改动。
// 这样「按字节预算淘汰的 LRU」在整个包内只有**一份实现**。

// defaultBlockletCacheBytes 是块级明文缓存的默认字节预算（64MiB）。
//
// 为什么取 64MiB：偏离线随机读（截图/抽帧）会反复命中同一批块；预算再大收益递减而内存
// 压力上升；再小则跨窗口重读明显。可用 `Options.BlockletCacheBytes` 覆盖（<=0 用默认，
// <0 关闭缓存）。
const defaultBlockletCacheBytes = 64 << 20

// blockletCache 是块级明文的按字节预算 LRU（嵌入泛型实现，字段 `budget` 仍可直接访问）。
type blockletCache struct {
	*budgetLRU[[]byte]
}

// newBlockletCache 构造缓存；budget<0 关闭（返回 nil，各方法对 nil 接收者安全）。
func newBlockletCache(budget int64) *blockletCache {
	inner := newBudgetLRU[[]byte](budget, defaultBlockletCacheBytes,
		func(b []byte) int64 { return int64(len(b)) })
	if inner == nil {
		return nil
	}
	return &blockletCache{budgetLRU: inner}
}
