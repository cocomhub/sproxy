// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// dedup.go 是「内容寻址去重」（roadmap §2 P1）的域实现：
//
//   - **DedupStore 台账**（per-tenant meta 桶 `dedup.json`）：SHA-256 checksum → 引用列表
//     （`{volume, rel}`），原子落盘、重启可恢复；
//   - **上传去重**：`dedup.enabled` 时，同 owner 同卷同内容 → 硬链接（`os.Root.Link`）
//     零拷贝复用 + 台账追加引用；配额**只计首份物理占用**（引用不额外计）；
//   - **删除**：引用计数 > 1 只摘引用（文件保留 + 配额不减）；归零才真正删 inode +
//     释放配额。
//
// 安全边界（写代码必须遵守）：
//   - **owner 隔离**：台账 per-tenant，不同 owner 永不共享 inode；
//   - **同卷硬链**：硬链接仅同卷（同物理文件系统）可用，跨卷内容相同不合并；
//   - **生命周期同步**：引用列表必须与文件生命周期同步（上传 / 覆盖写 / 删除 /
//     重命名 / 分块 complete 全部维护），否则引用泄漏 → 文件永不释放。
//
// 台账 key 语义与 checksum 台账一致：rel 为租户根内相对路径（`user/...`），无 owner 前缀。
// `FirstRel` 是内容索引入口：上传新文件时按 (checksum, 卷) 反查已存引用做硬链接源。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// dedupRef 是去重台账中的单个引用：文件逻辑路径 + 所在卷。
type dedupRef struct {
	// Volume 是文件所在卷名（首份与引用同卷；跨卷不合并）。
	Volume string `json:"volume"`
	// Rel 是租户根内相对路径（user/...），与 checksum 台账同 key 语义。
	Rel string `json:"rel"`
}

// dedupEntry 是一个 checksum 的全部引用列表。物理 inode 由首份承载；
// RefCount = len(Refs)（引用计数归零才删 inode）。
type dedupEntry struct {
	Refs []dedupRef `json:"refs"`
}

// DedupStore 维护「checksum → 引用列表」的 per-tenant 去重台账。
// 并发安全：内部 RWMutex；save 前深拷贝释放锁再做 I/O（与 checksum.ChecksumStore 同模式）。
type DedupStore struct {
	mu        sync.RWMutex
	saveMu    sync.Mutex // 串行化 save 的 WriteFile + Rename（Windows 并发 Rename 失败防护）
	storePath string
	entries   map[string]*dedupEntry // checksum -> refs
	logger    *slog.Logger
}

// NewDedupStore 构造去重台账（装配层用）。加载已有记录并清理崩溃残留 .tmp。
func NewDedupStore(storePath string, logger *slog.Logger) *DedupStore {
	ds := &DedupStore{
		storePath: storePath,
		entries:   make(map[string]*dedupEntry),
		logger:    slogutil.Default(logger),
	}

	tmpResidue := storePath + ".tmp"
	if _, err := os.Stat(tmpResidue); err == nil {
		if rmErr := os.Remove(tmpResidue); rmErr != nil {
			ds.logger.Warn("清理 dedup tmp 残留失败", "path", tmpResidue, "error", rmErr)
		}
	}

	data, err := os.ReadFile(storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			ds.logger.Warn("读取 dedup 存储文件失败", "path", storePath, "error", err)
		}
		return ds
	}
	if len(data) == 0 {
		return ds
	}
	if err := json.Unmarshal(data, &ds.entries); err != nil {
		ds.logger.Warn("解析 dedup 存储文件失败，将使用空存储", "path", storePath, "error", err)
		ds.entries = make(map[string]*dedupEntry)
	}
	return ds
}

// Add 向台账追加一个引用。返回 true 表示这是该 checksum 的**首份引用**（调用方需创建
// 物理文件）；false 表示已有首份（调用方应做硬链接零拷贝）。
func (ds *DedupStore) Add(rel, vol, checksum string) bool {
	ds.mu.Lock()
	entry, ok := ds.entries[checksum]
	if !ok {
		entry = &dedupEntry{}
		ds.entries[checksum] = entry
	}
	entry.Refs = append(entry.Refs, dedupRef{Volume: vol, Rel: rel})
	first := !ok
	ds.mu.Unlock()

	if err := ds.save(); err != nil {
		ds.logger.Error("dedup 存储持久化失败", "op", "add", "checksum", checksum, "error", err)
		if retryErr := ds.save(); retryErr != nil {
			ds.logger.Error("重试持久化失败", "op", "add", "checksum", checksum, "error", retryErr)
		}
	}
	return first
}

// RemoveRef 从台账移除一个引用（删除 / 覆盖写前调用）。返回该 checksum 剩余的引用数。
func (ds *DedupStore) RemoveRef(rel, vol, checksum string) int {
	ds.mu.Lock()
	entry, ok := ds.entries[checksum]
	if !ok {
		ds.mu.Unlock()
		return 0
	}
	filtered := entry.Refs[:0]
	for _, ref := range entry.Refs {
		if ref.Rel != rel || ref.Volume != vol {
			filtered = append(filtered, ref)
		}
	}
	remaining := len(filtered)
	if remaining == 0 {
		delete(ds.entries, checksum)
	} else {
		entry.Refs = filtered
	}
	ds.mu.Unlock()

	if err := ds.save(); err != nil {
		ds.logger.Error("dedup 存储持久化失败", "op", "remove", "checksum", checksum, "error", err)
		if retryErr := ds.save(); retryErr != nil {
			ds.logger.Error("重试持久化失败", "op", "remove", "checksum", checksum, "error", retryErr)
		}
	}
	return remaining
}

// FirstRel 返回指定卷内该 checksum 的第一个引用 rel（硬链接源）。跨卷不返回（不同卷
// 物理独立）。未命中 ok=false。
func (ds *DedupStore) FirstRel(vol, checksum string) (string, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	entry, ok := ds.entries[checksum]
	if !ok {
		return "", false
	}
	for _, ref := range entry.Refs {
		if ref.Volume == vol {
			return ref.Rel, true
		}
	}
	return "", false
}

// RelExists 判断指定 rel 是否是该 checksum 的引用（重命名/幂等校验用）。
func (ds *DedupStore) RelExists(rel, vol, checksum string) (int, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	entry, ok := ds.entries[checksum]
	if !ok {
		return 0, false
	}
	for _, ref := range entry.Refs {
		if ref.Rel == rel && ref.Volume == vol {
			return len(entry.Refs), true
		}
	}
	return len(entry.Refs), false
}

// RelCS 返回指定 (rel, vol) 所属的 checksum（覆盖写摘除旧引用用）。未命中 ok=false。
func (ds *DedupStore) RelCS(rel, vol string) (string, bool) {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	for checksum, entry := range ds.entries {
		for _, ref := range entry.Refs {
			if ref.Rel == rel && ref.Volume == vol {
				return checksum, true
			}
		}
	}
	return "", false
}

// RefCount 返回某 checksum 的引用总数。
func (ds *DedupStore) RefCount(checksum string) int {
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	entry, ok := ds.entries[checksum]
	if !ok {
		return 0
	}
	return len(entry.Refs)
}

// Rename 把台账内引用路径 fromRel → toRel（重命名成功路径同步；与 checksum 台账 Rename
// 同生命周期）。未命中时静默（与 ChecksumStore.Rename 对齐）。
func (ds *DedupStore) Rename(fromRel, toRel string) {
	ds.mu.Lock()
	moved := false
	for _, entry := range ds.entries {
		for i := range entry.Refs {
			if entry.Refs[i].Rel == fromRel {
				entry.Refs[i].Rel = toRel
				moved = true
			}
		}
	}
	ds.mu.Unlock()
	if !moved {
		return
	}
	if err := ds.save(); err != nil {
		ds.logger.Error("dedup 存储持久化失败", "op", "rename", "from", fromRel, "to", toRel, "error", err)
		if retryErr := ds.save(); retryErr != nil {
			ds.logger.Error("重试持久化失败", "op", "rename", "from", fromRel, "to", toRel, "error", retryErr)
		}
	}
}

// DeletePrefix 删除指定前缀的全部引用（目录删除用，与 checksum 台账 DeletePrefix 对齐）。
func (ds *DedupStore) DeletePrefix(prefix string) {
	ds.mu.Lock()
	changed := false
	for checksum, entry := range ds.entries {
		filtered := entry.Refs[:0]
		for _, ref := range entry.Refs {
			if !strings.HasPrefix(ref.Rel, prefix) {
				filtered = append(filtered, ref)
			}
		}
		if len(filtered) == 0 {
			delete(ds.entries, checksum)
			changed = true
		} else if len(filtered) != len(entry.Refs) {
			entry.Refs = filtered
			changed = true
		}
	}
	ds.mu.Unlock()
	if !changed {
		return
	}
	if err := ds.save(); err != nil {
		ds.logger.Error("dedup 存储持久化失败", "op", "deletePrefix", "prefix", prefix, "error", err)
		if retryErr := ds.save(); retryErr != nil {
			ds.logger.Error("重试持久化失败", "op", "deletePrefix", "prefix", prefix, "error", retryErr)
		}
	}
}

// save 把 dedup 台账持久化到磁盘（原子写：tmp + Rename）。
func (ds *DedupStore) save() error {
	ds.saveMu.Lock()
	defer ds.saveMu.Unlock()

	ds.mu.RLock()
	snapshot := make(map[string]*dedupEntry, len(ds.entries))
	maps.Copy(snapshot, ds.entries)
	ds.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := ds.storePath + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, ds.storePath); err != nil {
		ds.logger.Warn("原子重命名失败，回退到直接写入", "error", err)
		if writeErr := os.WriteFile(ds.storePath, data, 0o644); writeErr != nil {
			return fmt.Errorf("回退写入失败: %w", writeErr)
		}
	}
	return nil
}

// exportSnapshot 是 DedupStore 的全量快照（checksum → 引用副本），供重复报告
// ReportFromLedger 枚举使用（读锁内深拷贝，与 save 的同构模式）。
func (ds *DedupStore) exportSnapshot() map[string][]DupRef {
	out := map[string][]DupRef{}
	if ds == nil {
		return out
	}
	ds.mu.RLock()
	defer ds.mu.RUnlock()
	for checksum, entry := range ds.entries {
		refs := make([]DupRef, 0, len(entry.Refs))
		for _, r := range entry.Refs {
			refs = append(refs, DupRef{Volume: r.Volume, Rel: r.Rel})
		}
		out[checksum] = refs
	}
	return out
}

// newDedupStore 是 NewDedupStore 的私有别名（包内测试沿用旧名）。
func newDedupStore(storePath string, logger *slog.Logger) *DedupStore {
	return NewDedupStore(storePath, logger)
}
