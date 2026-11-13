// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// defaultMetaCacheBytes 是元数据内存缓存 L1b 的默认字节预算（256 MiB）。
//
// 为什么需要它（P2，2026-11-12）：P1 让快照不再承载冷结构 `Meta`，改为按需加载；
// 但加载后 `e.meta` 会被**持久挂上**（两个调用点都是「`e.meta == nil` ⇒ ensure」后长期使用），
// 于是「读过的文件」会永久常驻 —— 读 100 个大文件就回到 OOM。
// L1b 的作用有两个：①同一文件重复读 ⇒ 零远端 + 零反序列化；②**给常驻量设硬上限**。
//
// 计量依据（实测）：真实 meta 430,652 个 blocklets ⇒ Go 常驻 78 MB ⇒ **约 191 B/blocklet**，
// 故 sizeOf 按 blocklet/window 条目数 × 191 B 估算（另加固定开销）。
//
// 预算取值：单文件实测可达 78 MB，256 MiB 约容纳 3 个此类文件 —— 足够覆盖「同一批文件被
// 反复读取」的常见模式，同时把常驻量钉死在可控范围。可用 `Options.MetaCacheBytes` 覆盖
// （<=0 用默认，<0 关闭）。
const defaultMetaCacheBytes = 256 << 20

// metaSizeBytes 估算一个 Meta 在内存中的占用（实测系数 191 B/blocklet，见上）。
func metaSizeBytes(m *shardseal.Meta) int64 {
	if m == nil {
		return 0
	}
	n := int64(0)
	for _, c := range m.Chunks {
		n += int64(len(c.Blocklets)) + int64(len(c.Windows))
	}
	return n*191 + 4096
}

// metaEntryCache 是元数据的内存 LRU 缓存（L1b），键为**逻辑相对路径 rel**
// （与 `s.index` 同键 ⇒ 淘汰时可直接回查并清空 `metaEntry.meta`）。
type metaEntryCache struct {
	*budgetLRU[*shardseal.Meta]
}

// newMetaEntryCache 构造 L1b；budget<0 关闭（返回 nil，各方法对 nil 接收者安全）。
func newMetaEntryCache(budget int64) *metaEntryCache {
	inner := newBudgetLRU[*shardseal.Meta](budget, defaultMetaCacheBytes, metaSizeBytes)
	if inner == nil {
		return nil
	}
	return &metaEntryCache{budgetLRU: inner}
}

// metaLoadConcurrency 返回「同时加载 meta」的安全并发上限。
//
// 依据（实测 2026-11-13，见 docs/designs/2026-11-12-...lazy-meta-and-cache-layers.md §4.3）：
//   - 单条大文件 meta 的**加载瞬时峰值**可达 **~350 MB**（常驻 ~82 MB，含 JSON 中间物）；
//   - 并发吞吐在 **4 之后严重饱和**（8→16 只增 7%，却多花 1.5 GB）。
//
// ⇒ 上限取 `min(4, 预算 / 单条常驻)`，且**至少 1**。
// 为什么用「常驻」而非「峰值」做分母：L1b 的 `metaSizeBytes` 是**容量语义**（缓存能装多少条），
// 不宜按峰值放大（否则缓存会小到无用）；峰值风险改由**本闸**限制"同时在加载的条数"来兜。
func metaLoadConcurrency(budget int64) int {
	const typicalMetaBytes = 82 << 20 // 实测大文件 meta 的常驻规模
	n := max(int(budget/typicalMetaBytes), 1)
	if n > 4 {
		n = 4 // 硬性封顶：吞吐已饱和，再多只会与 OOM 抢空间
	}
	return n
}

// metaLoadGate 是「meta 加载」的有界并发闸（nil = 不限制）。
//
// 为什么需要：`ensureEntryMeta` 会被**并发请求**各自触发；若 N 个请求同时加载 N 个大 meta，
// 瞬时峰值 ≈ N × 350 MB ⇒ **直接 OOM**。L1b 的字节预算只约束"**已加载**的常驻量"，
// 约束不了"**正在加载**的峰值"，故需要本闸单独兜住。
type metaLoadGate chan struct{}

// acquire 取一个名额（ctx 取消即退出，不无限等待）。
func (g metaLoadGate) acquire(ctx context.Context) error {
	if g == nil {
		return nil
	}
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g metaLoadGate) release() {
	if g != nil {
		<-g
	}
}
