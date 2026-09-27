// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// state_index.go 是搜索索引快照的 StateStore 适配（statestore.md §5.1 P1 /
// cluster-state-migration.md §3.3）：index/<owner> 单 key——save 委托 StateStore.Put
// （快照覆盖，无需 CAS）；load 委托 Get（未命中 → (nil, false)，调用方全量重建）。
//
// 双读单写（零回归铁律，statestore.md §5.1）：
//   - load：StateStore.Get 优先；ErrKeyNotFound → 回退读旧 <meta>/index/<owner>.json
//     （legacyDir，迁移前存量零丢失）；旧文件也不存在 → (nil, false)（空索引）；
//   - save：恒写 StateStore 新路径（首写即完成迁移；旧 meta 文件不再改写）。
//
// 序列化格式与既有 indexSnapshotFile JSON 逐字一致（读旧格式兼容）。

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/state"
)

// stateBackedIndexStore 是搜索索引快照的 StateStore 适配（装配层用）。
// legacyDir 为旧 <meta>/index 绝对路径（回退读）；空 = 不回退。
type stateBackedIndexStore struct {
	st        state.StateStore
	legacyDir string // 旧 <meta>/index（回退读）；空 = 不回退
}

// newStateBackedIndexStore 构造 StateStore 后端索引快照存储。
func newStateBackedIndexStore(st state.StateStore, legacyDir string) *stateBackedIndexStore {
	return &stateBackedIndexStore{st: st, legacyDir: legacyDir}
}

// indexKey 返回 owner 对应的 StateStore key（index/<owner> 两段式）。
func indexKey(owner string) string { return "index/" + owner }

// legacyPath 返回 owner 对应的旧落盘文件路径（legacyDir 为空时为空串）。
func (s *stateBackedIndexStore) legacyPath(owner string) string {
	if s.legacyDir == "" {
		return ""
	}
	return filepath.Join(s.legacyDir, owner+".json")
}

// load 载入 owner 索引快照：StateStore 优先；未命中回退旧 meta 文件；
// 都不存在/损坏 → (nil, false)（调用方全量重建）。
func (s *stateBackedIndexStore) load(owner string) (map[string]*indexEntry, bool) {
	ctx := context.Background()
	data, err := s.st.Get(ctx, indexKey(owner))
	if err != nil {
		if !errors.Is(err, state.ErrKeyNotFound) {
			return nil, false // StateStore 读失败 → 空索引（调用方全量重建，加速层尽力而为）
		}
		// 回退读旧 meta（迁移前存量）。
		p := s.legacyPath(owner)
		if p == "" {
			return nil, false
		}
		data, err = os.ReadFile(p)
		if err != nil {
			return nil, false
		}
	}
	var f indexSnapshotFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, false
	}
	if f.Entries == nil {
		return nil, false
	}
	return fromSnapshotEntries(f.Entries), true
}

// save 全量快照写 StateStore（快照覆盖，无需 CAS）。
func (s *stateBackedIndexStore) save(owner string, entries map[string]*indexEntry) error {
	data, err := json.Marshal(indexSnapshotFile{Entries: toSnapshotEntries(entries)})
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := s.st.Put(ctx, indexKey(owner), data); err != nil {
		return err
	}
	return nil
}

// delete 删除 owner 索引快照（失效语义：下次访问全量重建，防载入过期快照）。
func (s *stateBackedIndexStore) delete(owner string) error {
	ctx := context.Background()
	return s.st.Delete(ctx, indexKey(owner))
}
