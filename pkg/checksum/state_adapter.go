// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// state_adapter.go 是 checksum 台账的 StateStore 适配（statestore.md §5.1 P0）：
// 保持 ChecksumStoreIface 接口不动，新增 StateBackedChecksumStore 把
// `map[string]string` 全量快照整体序列化为一个 key（checksum/<owner>/all），
// 磁盘字节与既有 <meta>/checksums.json 的 JSON 逐字一致。
//
// 双读单写（零回归铁律，statestore.md §5.1）：
//   - 首次 Get：StateStore.Get 优先；ErrKeyNotFound → 回退读旧 <meta>/checksums.json
//     （legacyPath，迁移前存量零丢失）；
//   - 首次写：恒写 StateStore 新路径（首写即完成迁移；旧 meta 文件不再改写）。
//
// 序列化格式即既有 ChecksumStore.save 的 map JSON（json.MarshalIndent(map, "", "  ")），
// 保证「读旧格式兼容」（既有文件可直接被适配器载入）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/internal/savemerger"
	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// StateBackedChecksumStore 是 ChecksumStoreIface 的 StateStore 适配（装配层用）。
type StateBackedChecksumStore struct {
	st         state.StateStore
	key        string        // StateStore key（checksum/<owner>/all）
	legacyRoot *storage.Root // 旧 <meta>/checksums.json 所在**卷句柄**；nil = 不回退
	legacyRel  string        // 旧 <meta>/checksums.json 卷内相对路径；空 = 不回退
	logger     *slog.Logger

	mu        sync.RWMutex
	checksums map[string]string
	loaded    bool // 是否已从 StateStore 载入（防 Get 未命中时重复回退读旧文件）

	// put 是 StateStore 写入实现（构造注入，默认 st.Put；测试可计数）。
	put func(ctx context.Context, key string, data []byte) error
	// merger 把连续写合并为一次全量 Put（集群 KV 每次 Put 是一次 RTT + 全量序列化）。
	merger *savemerger.Merger
}

var _ ChecksumStoreIface = (*StateBackedChecksumStore)(nil)
var _ VolumeKeyedStore = (*StateBackedChecksumStore)(nil)
var _ Flusher = (*StateBackedChecksumStore)(nil)
var _ LenCounter = (*StateBackedChecksumStore)(nil)
var _ Iterator = (*StateBackedChecksumStore)(nil)
var _ BatchWriter = (*StateBackedChecksumStore)(nil)
var _ VolumeBatchWriter = (*StateBackedChecksumStore)(nil)

// NewStateBackedChecksumStore 构造 StateStore 后端 checksum 台账。
// legacyRoot/legacyRel 定位旧 <meta>/checksums.json（回退读）；legacyRoot 为 nil 或
// legacyRel 为空 = 不回退读旧 meta（纯 StateStore 形态）。回退读经**卷句柄**：
// 加密卷上旧文件是密文，裸 os 读会解析失败（迁移期台账静默丢失）。
func NewStateBackedChecksumStore(st state.StateStore, key string, legacyRoot *storage.Root, legacyRel string, logger *slog.Logger) *StateBackedChecksumStore {
	s := &StateBackedChecksumStore{
		st:         st,
		key:        key,
		legacyRoot: legacyRoot,
		legacyRel:  legacyRel,
		logger:     slogutil.Default(logger),
		checksums:  make(map[string]string),
	}
	s.put = st.Put
	s.merger = savemerger.New(savemerger.DefaultDebounce, s.logger, func(uint64) error { return s.save() })
	return s
}

// loadLocked 载入快照（调用方持写锁）：StateStore 优先；未命中回退旧 meta 文件。
// 旧文件也不存在 → 空台账（与既有 NewChecksumStore 的「文件不存在 = 空存储」对齐）。
func (s *StateBackedChecksumStore) loadLocked() {
	if s.loaded {
		return
	}
	ctx := context.Background()
	data, done := s.loadFromState(ctx)
	if done {
		s.loaded = true
		return
	}
	if len(data) > 0 {
		if jerr := json.Unmarshal(data, &s.checksums); jerr != nil {
			// 与既有 NewChecksumStore 同语义：解析失败记日志 + 空存储（尽力而为，非 fail-closed）。
			s.logger.Warn("解析 checksum 存储失败，将使用空存储", "key", s.key, "error", jerr)
			s.checksums = make(map[string]string)
		}
	}
	s.loaded = true
}

// loadFromState 从 StateStore 读取快照；未命中（ErrKeyNotFound）时回退读旧 meta 文件
// （legacyPath，迁移前存量）；旧文件也不存在 → 空台账。返回 (data, done)：done=true 表示
// 已确认无可用的快照来源（含读取失败回退空存储），调用方应标记 loaded 并直接返回；
// done=false 表示 data 来自某持久副本，需继续反序列化。
func (s *StateBackedChecksumStore) loadFromState(ctx context.Context) (data []byte, done bool) {
	var err error
	data, err = s.st.Get(ctx, s.key)
	if err != nil {
		if !errors.Is(err, state.ErrKeyNotFound) {
			s.logger.Warn("checksum store: StateStore 读取失败，将使用空存储", "key", s.key, "error", err)
			return nil, true
		}
		// 回退读旧 meta（迁移前存量；经卷句柄 ⇒ 加密卷上密文同样可读）。
		if s.legacyRoot == nil || s.legacyRel == "" {
			return nil, true
		}
		data, err = readRootFileBounded(s.legacyRoot, s.legacyRel, maxChecksumStoreBytes)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.logger.Warn("checksum store: 读取旧 meta 文件失败，将使用空存储", "path", s.legacyRel, "error", err)
			}
			return nil, true
		}
	}
	return data, false
}

// ensureLoaded 供读路径调用（返回后快照已就绪）。
func (s *StateBackedChecksumStore) ensureLoaded() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
}

// Get 查询指定文件的 checksum。
func (s *StateBackedChecksumStore) Get(filename string) (string, bool) {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.checksums[filename]
	return v, ok
}

// GetAll 返回全部 checksum 记录的副本（filename -> sha256）。
func (s *StateBackedChecksumStore) GetAll() map[string]string {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]string, len(s.checksums))
	maps.Copy(result, s.checksums)
	return result
}

// save 把全量快照写 StateStore（单写：迁移后旧 meta 不再改写）。
func (s *StateBackedChecksumStore) save() error {
	s.mu.RLock()
	snapshot := make(map[string]string, len(s.checksums))
	maps.Copy(snapshot, s.checksums)
	s.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := s.put(ctx, s.key, data); err != nil {
		return fmt.Errorf("checksum store: StateStore 写入失败: %w", err)
	}
	return nil
}

// Set 写入一条 checksum 记录并持久化（失败重试一次，与既有 ChecksumStore 同语义）。
func (s *StateBackedChecksumStore) Set(filename, checksum string) {
	s.mu.Lock()
	if !s.loaded {
		s.loadLocked()
	}
	s.checksums[filename] = checksum
	s.mu.Unlock()

	s.merger.Mark("set", "file_name", filename)
}

// Delete 删除指定文件的 checksum 记录并持久化。
func (s *StateBackedChecksumStore) Delete(filename string) {
	s.mu.Lock()
	if !s.loaded {
		s.loadLocked()
	}
	delete(s.checksums, filename)
	s.mu.Unlock()

	s.merger.Mark("delete", "file_name", filename)
}

// Rename 将一条 checksum 记录从 from 迁移到 to（to 已存在被覆盖，与 os.Rename 对齐）。
func (s *StateBackedChecksumStore) Rename(from, to string) {
	s.mu.Lock()
	if !s.loaded {
		s.loadLocked()
	}
	v, ok := s.checksums[from]
	if !ok {
		s.mu.Unlock()
		s.logger.Warn("ChecksumStore.Rename: from 路径不存在，跳过重命名", "from", from, "to", to)
		return
	}
	delete(s.checksums, from)
	s.checksums[to] = v
	s.mu.Unlock()

	s.merger.Mark("rename", "from", from, "to", to)
}

// DeletePrefix 删除指定前缀的所有 checksum 记录（目录删除）。
func (s *StateBackedChecksumStore) DeletePrefix(prefix string) {
	s.mu.Lock()
	if !s.loaded {
		s.loadLocked()
	}
	for k := range s.checksums {
		if strings.HasPrefix(k, prefix) {
			delete(s.checksums, k)
		}
	}
	s.mu.Unlock()

	s.merger.Mark("deletePrefix", "prefix", prefix)
}

// SetInVolume 以卷维度键记录 checksum（`<vol>\x00<rel>`，§15.1.1）。
//
// 为什么集群后端也必须有：`checksumSet/Get` 按 `VolumeKeyedStore` 类型断言选择卷维度路径，
// 未实现时会静默退化为无卷键 ⇒ 同一 owner 多卷上的同名文件互相覆盖（checksum 张冠李戴）。
func (s *StateBackedChecksumStore) SetInVolume(vol, filename, checksum string) {
	s.ensureLoaded()
	key := volumeKey(vol, filename)
	s.mu.Lock()
	s.checksums[key] = checksum
	s.mu.Unlock()
	s.merger.Mark("set_in_volume", "key", key)
}

// GetInVolume 读卷维度键；miss 时回退旧无卷键（迁移兼容）。
func (s *StateBackedChecksumStore) GetInVolume(vol, filename string) (string, bool) {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.checksums[volumeKey(vol, filename)]; ok {
		return v, true
	}
	v, ok := s.checksums[filename]
	return v, ok
}

// DeleteInVolume 删除卷维度键（不动旧键——旧键可能被其它卷回退使用）。
func (s *StateBackedChecksumStore) DeleteInVolume(vol, filename string) {
	s.ensureLoaded()
	key := volumeKey(vol, filename)
	s.mu.Lock()
	if _, ok := s.checksums[key]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.checksums, key)
	s.mu.Unlock()
	s.merger.Mark("delete_in_volume", "key", key)
}

// DeletePrefixInVolume 删除该卷下以 prefix 开头的卷维度键（rmdir 子树清理用）。
func (s *StateBackedChecksumStore) DeletePrefixInVolume(vol, prefix string) {
	s.ensureLoaded()
	full := volumeKey(vol, prefix)
	s.mu.Lock()
	removed := 0
	for k := range s.checksums {
		if strings.HasPrefix(k, full) {
			delete(s.checksums, k)
			removed++
		}
	}
	s.mu.Unlock()
	if removed > 0 {
		s.merger.Mark("delete_prefix_in_volume", "volume", vol, "prefix", prefix, "removed", removed)
	}
}

// RenameInVolume 同卷内重命名卷维度键。
func (s *StateBackedChecksumStore) RenameInVolume(vol, from, to string) {
	s.ensureLoaded()
	fromKey, toKey := volumeKey(vol, from), volumeKey(vol, to)
	s.mu.Lock()
	v, ok := s.checksums[fromKey]
	if !ok {
		s.mu.Unlock()
		s.logger.Warn("ChecksumStore.RenameInVolume: from 路径不存在，跳过", "volume", vol, "from", from, "to", to)
		return
	}
	delete(s.checksums, fromKey)
	s.checksums[toKey] = v
	s.mu.Unlock()
	s.merger.Mark("rename_in_volume", "volume", vol, "from", from, "to", to)
}

// SnapshotLegacy 返回旧无卷键快照（迁移兼容）。
func (s *StateBackedChecksumStore) SnapshotLegacy() map[string]string {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.checksums))
	for k, v := range s.checksums {
		if !strings.Contains(k, volumeKeySep) {
			out[k] = v
		}
	}
	return out
}

// SnapshotInVolume 返回该卷的 `rel → checksum` 快照（键为裸 rel）。
func (s *StateBackedChecksumStore) SnapshotInVolume(vol string) map[string]string {
	s.ensureLoaded()
	prefix := vol + volumeKeySep
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.checksums))
	for k, v := range s.checksums {
		if rel, ok := strings.CutPrefix(k, prefix); ok {
			out[rel] = v
		}
	}
	return out
}

// Iterate 分批遍历台账（锁外回调；语义同 ChecksumStore.Iterate）。
func (s *StateBackedChecksumStore) Iterate(batch int, fn func(key, checksum string) bool) {
	if batch <= 0 {
		batch = 256
	}
	s.ensureLoaded()
	s.mu.RLock()
	keys := make([]string, 0, len(s.checksums))
	for k := range s.checksums {
		keys = append(keys, k)
	}
	s.mu.RUnlock()

	type kv struct{ k, v string }
	page := make([]kv, 0, batch)
	for i := 0; i < len(keys); i += batch {
		end := min(i+batch, len(keys))
		s.mu.RLock()
		page = page[:0]
		for _, k := range keys[i:end] {
			if v, ok := s.checksums[k]; ok {
				page = append(page, kv{k: k, v: v})
			}
		}
		s.mu.RUnlock()
		for _, p := range page {
			if !fn(p.k, p.v) {
				return
			}
		}
	}
}

// Len 返回台账条目数（必要时先载入；O(1) 判空用）。
func (s *StateBackedChecksumStore) Len() int {
	s.ensureLoaded()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.checksums)
}

// SetMany 批量写入（一次 Put），供 rebuild 类全量回填。
func (s *StateBackedChecksumStore) SetMany(entries map[string]string) {
	if len(entries) == 0 {
		return
	}
	s.mu.Lock()
	if !s.loaded {
		s.loadLocked()
	}
	maps.Copy(s.checksums, entries)
	s.mu.Unlock()
	s.merger.Mark("set_many", "count", len(entries))
}

// SetManyInVolume 按卷维度键批量写入（`<vol>\x00<rel>`，§15.1.1：跨卷同名不串值）。
func (s *StateBackedChecksumStore) SetManyInVolume(vol string, entries map[string]string) {
	if len(entries) == 0 {
		return
	}
	s.mu.Lock()
	if !s.loaded {
		s.loadLocked()
	}
	for rel, sum := range entries {
		s.checksums[volumeKey(vol, rel)] = sum
	}
	s.mu.Unlock()
	s.merger.Mark("set_many_in_volume", "volume", vol, "count", len(entries))
}

// Flush 立即把合并窗口内的改动写入 StateStore（有待持久化改动时）。
func (s *StateBackedChecksumStore) Flush() error { return s.merger.Flush() }

// Close 停止合并定时器并做最终落盘（幂等）。
func (s *StateBackedChecksumStore) Close() error { return s.merger.Close() }
