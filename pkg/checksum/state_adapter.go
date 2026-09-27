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

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/state"
)

// StateBackedChecksumStore 是 ChecksumStoreIface 的 StateStore 适配（装配层用）。
type StateBackedChecksumStore struct {
	st         state.StateStore
	key        string // StateStore key（checksum/<owner>/all）
	legacyPath string // 旧 <meta>/checksums.json（回退读）；空 = 不回退
	logger     *slog.Logger

	mu        sync.RWMutex
	checksums map[string]string
	loaded    bool // 是否已从 StateStore 载入（防 Get 未命中时重复回退读旧文件）
}

var _ ChecksumStoreIface = (*StateBackedChecksumStore)(nil)

// NewStateBackedChecksumStore 构造 StateStore 后端 checksum 台账。
// legacyPath 为空 = 不回退读旧 meta（纯 StateStore 形态）。
func NewStateBackedChecksumStore(st state.StateStore, key, legacyPath string, logger *slog.Logger) *StateBackedChecksumStore {
	return &StateBackedChecksumStore{
		st:         st,
		key:        key,
		legacyPath: legacyPath,
		logger:     slogutil.Default(logger),
		checksums:  make(map[string]string),
	}
}

// loadLocked 载入快照（调用方持写锁）：StateStore 优先；未命中回退旧 meta 文件。
// 旧文件也不存在 → 空台账（与既有 NewChecksumStore 的「文件不存在 = 空存储」对齐）。
func (s *StateBackedChecksumStore) loadLocked() {
	if s.loaded {
		return
	}
	ctx := context.Background()
	data, err := s.st.Get(ctx, s.key)
	if err != nil {
		if !errors.Is(err, state.ErrKeyNotFound) {
			s.logger.Warn("checksum store: StateStore 读取失败，将使用空存储", "key", s.key, "error", err)
			s.loaded = true
			return
		}
		// 回退读旧 meta（迁移前存量）。
		if s.legacyPath != "" {
			data, err = os.ReadFile(s.legacyPath)
			if err != nil {
				if !os.IsNotExist(err) {
					s.logger.Warn("checksum store: 读取旧 meta 文件失败，将使用空存储", "path", s.legacyPath, "error", err)
				}
				s.loaded = true
				return
			}
		} else {
			s.loaded = true
			return
		}
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
	if err := s.st.Put(ctx, s.key, data); err != nil {
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

	if err := s.save(); err != nil {
		s.logger.Error("checksum 存储持久化失败", "op", "set", "file_name", filename, "error", err)
		if retryErr := s.save(); retryErr != nil {
			s.logger.Error("重试持久化失败", "op", "set", "file_name", filename, "error", retryErr)
		}
	}
}

// Delete 删除指定文件的 checksum 记录并持久化。
func (s *StateBackedChecksumStore) Delete(filename string) {
	s.mu.Lock()
	if !s.loaded {
		s.loadLocked()
	}
	delete(s.checksums, filename)
	s.mu.Unlock()

	if err := s.save(); err != nil {
		s.logger.Error("checksum 存储持久化失败", "op", "delete", "file_name", filename, "error", err)
		if retryErr := s.save(); retryErr != nil {
			s.logger.Error("重试持久化失败", "op", "delete", "file_name", filename, "error", retryErr)
		}
	}
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

	if err := s.save(); err != nil {
		s.logger.Error("checksum 存储持久化失败", "op", "rename", "from", from, "to", to, "error", err)
		if retryErr := s.save(); retryErr != nil {
			s.logger.Error("重试持久化失败", "op", "rename", "from", from, "to", to, "error", retryErr)
		}
	}
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

	if err := s.save(); err != nil {
		s.logger.Error("checksum 存储持久化失败", "op", "deletePrefix", "prefix", prefix, "error", err)
		if retryErr := s.save(); retryErr != nil {
			s.logger.Error("重试持久化失败", "op", "deletePrefix", "prefix", prefix, "error", retryErr)
		}
	}
}
