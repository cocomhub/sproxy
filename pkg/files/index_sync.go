// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// index_sync.go 是集群索引一致性（roadmap 11.11 方案 A-④）：
//   - indexEnvelope：StateStore 承载的快照信封（rev 单调 + node 标识 + 条目）；
//   - IndexSync：集群装配层注入的可选钩子（nil = 零回归）；
//   - ReloadIndex：副本 rev 校验载入（旧 rev 忽略；损坏回退重建）；
//   - dirty 跟踪：写路径增量置脏 → publishDirty 只发变更 owner（防重载风暴）。
//
// 单节点（未装配 sync）全部新逻辑不装配，行为与现状逐字一致。

import (
	"context"
)

// IndexEnvelope 是 StateStore 承载的快照信封（index_persist.go 的 DTO 外包一层）。
// rev 由主节点 per-owner 单调递增（启动置 1，每次 Publish +1）；node 是发布者标识。
// 副本用 rev 去重/防乱序：旧 rev 信封到达必须忽略（不覆盖新索引）。
type IndexEnvelope struct {
	Rev     int64                          `json:"rev"`
	Node    string                         `json:"node"`
	Updated int64                          `json:"updated_at"` // UnixNano，诊断用
	Entries map[string]*IndexSnapshotEntry `json:"entries"`
}

// IndexEnvelope 别名（内部用 indexEnvelope，导出 IndexEnvelope 供装配层）。
type indexEnvelope = IndexEnvelope

// IndexSnapshotEntry 是快照条目导出形态（IndexEnvelope 的条目）。
type IndexSnapshotEntry = indexSnapshotEntry

// NewIndexEnvelope 构造信封（测试/装配层用）。
func NewIndexEnvelope(rev int64, node string, entries map[string]IndexSnapshotEntryCompat) *IndexEnvelope {
	env := &IndexEnvelope{Rev: rev, Node: node}
	env.Entries = make(map[string]*IndexSnapshotEntry, len(entries))
	for k, e := range entries {
		env.Entries[k] = &IndexSnapshotEntry{Name: e.Name, Base: e.Base, IsDir: e.IsDir, Size: e.Size, ModTime: e.ModTime, Volume: e.Volume, Tags: e.Tags}
	}
	return env
}

// IndexSnapshotEntryCompat 是构造信封的兼容输入（导出字段形态）。
type IndexSnapshotEntryCompat struct {
	Name    string
	Base    string
	IsDir   bool
	Size    int64
	ModTime int64
	Volume  string
	Tags    []string
}

// IndexSync 是集群装配层注入的可选钩子（searchIndex 上挂 nil = 零回归）。
type IndexSync interface {
	// Publish 由主节点写路径增量后调用：rev++ → StateStore.Put(index/<owner>, envelope)。
	// entries 是导出 DTO（快照条目形态），适配器直接序列化。
	Publish(ctx context.Context, owner string, entries map[string]*IndexSnapshotEntry) error
	// Load 由副本重载时调用：从 StateStore Get(index/<owner>) 读信封（不存在 → ErrKeyNotFound）。
	Load(ctx context.Context, owner string) (*IndexEnvelope, error)
}

// attachIndexSync 挂载集群同步钩子（nil = 不装配，零回归）。
func (ix *searchIndex) AttachIndexSync(sync IndexSync) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.sync = sync
	if ix.dirty == nil {
		ix.dirty = map[string]bool{}
	}
}

// appliedRev 是副本已应用的 per-owner 最高 rev（防乱序覆盖；ix.mu 已保护）。
type appliedRev struct {
	rev map[string]int64
}

// ReloadIndex 校验 rev > 本进程已应用 rev → 快照载入替换 owner 索引。
// 返回 false = 未载入（旧 rev 忽略 / 损坏 → 调用方 InvalidateIndex 全量重建）。
func (ix *searchIndex) ReloadIndex(owner string, env *IndexEnvelope) bool {
	if env == nil || len(env.Entries) == 0 {
		return false // 损坏信封 → 回退重建
	}
	ix.mu.Lock()
	// rev 幂等：旧 rev（<= applied）忽略，不覆盖新索引。
	if env.Rev <= ix.applied.rev[owner] {
		ix.mu.Unlock()
		return false
	}
	oi := &ownerIndex{entries: fromSnapshotEntries(env.Entries)}
	ix.owners[owner] = oi
	ix.built[owner] = true // 快照载入后保持 built（免全量重建）
	ix.applied.rev[owner] = env.Rev
	ix.mu.Unlock()
	if ix.logger != nil {
		ix.logger().Debug("索引重载自集群信封", "owner", owner, "rev", env.Rev, "node", env.Node, "entries", len(env.Entries))
	}
	return true
}

// markDirty 写路径增量后置脏（Publish 由周期 saveAll/事件驱动统一承担）。
func (ix *searchIndex) markDirty(owner string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.dirty == nil {
		ix.dirty = map[string]bool{}
	}
	ix.dirty[owner] = true
}

// publishDirty 发布全部 dirty 已构建 owner（清脏）。未装配 sync → no-op（零回归）。
func (ix *searchIndex) publishDirty(ctx context.Context) int {
	ix.mu.Lock()
	if ix.sync == nil {
		ix.mu.Unlock()
		return 0
	}
	owners := make([]string, 0, len(ix.dirty))
	for o := range ix.dirty {
		if ix.built[o] && ix.owners[o] != nil {
			owners = append(owners, o)
		}
		delete(ix.dirty, o)
	}
	snapshots := make(map[string]*ownerIndex, len(owners))
	for _, o := range owners {
		if oi := ix.owners[o]; oi != nil {
			snapshots[o] = cloneOwnerIndexLocked(oi)
		}
	}
	ix.mu.Unlock()
	published := 0
	for _, o := range owners {
		oi := snapshots[o]
		if oi == nil {
			continue
		}
		env := &indexEnvelope{
			Rev:     ix.nextRev(o),
			Node:    ix.nodeID(),
			Updated: nowUnixNano(),
			Entries: toSnapshotEntries(oi.entries),
		}
		if err := ix.sync.Publish(ctx, o, env.Entries); err != nil {
			ix.logger().Warn("索引 Publish 失败", "owner", o, "error", err)
			ix.markDirty(o) // 失败保持 dirty，下周期重试
			continue
		}
		_ = env // env 由 sync 实现序列化（Publish 收 entries）；信封结构供测试直调
		published++
	}
	return published
}

// nextRev 返回 owner 的下一个 rev（单调递增；启动从 1 起）。
func (ix *searchIndex) nextRev(owner string) int64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.revCounter == nil {
		ix.revCounter = map[string]int64{}
	}
	ix.revCounter[owner]++
	return ix.revCounter[owner]
}

// nodeID 返回发布者标识（装配层注入；默认 "local"）。
func (ix *searchIndex) nodeID() string {
	if ix.nodeIDFn != nil {
		return ix.nodeIDFn()
	}
	return "local"
}

// ensureOwnerIndex 是测试用快捷：建 owner 索引（built=true，entries 空）。
func (ix *searchIndex) ensureOwnerIndex(owner string) *ownerIndex {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.built[owner] {
		return ix.owners[owner]
	}
	oi := &ownerIndex{entries: map[string]*indexEntry{}}
	ix.owners[owner] = oi
	ix.built[owner] = true
	return oi
}

// getEntry 是测试用读取。
func (ix *searchIndex) getEntry(owner, rel string) *indexEntry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if oi := ix.owners[owner]; oi != nil {
		return oi.entries[rel]
	}
	return nil
}

// nowUnixNano 是时间助手（测试可替换？不——诊断字段，直用 time）。
var nowUnixNano = func() int64 { return timeNow().UnixNano() }

// markDirtyLocked 持锁置脏（调用方已持 ix.mu——写路径增量内调用，避免重入锁）。
func (ix *searchIndex) markDirtyLocked(owner string) {
	if ix.dirty == nil {
		ix.dirty = map[string]bool{}
	}
	ix.dirty[owner] = true
}
