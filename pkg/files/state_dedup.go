// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// state_dedup.go 是去重台账的 StateStore 适配（statestore.md §5.1 P0）：
// 保持 *DedupStore 的领域接口与既有 dedup_test.go 行为套件不动，新增可选
// state 后端字段（st + stateKey）——nil = 本地 JSON 落盘（单节点零回归），
// 非 nil = 台账整体序列化为一个 key 写 StateStore。
//
// 双读单写（零回归铁律，statestore.md §5.1）：
//   - 首次载入：StateStore.Get 优先；ErrKeyNotFound → 回退读旧 <meta>/dedup.json
//     （legacyPath，迁移前存量零丢失）；旧文件也不存在 → 空台账；
//   - 首次写：恒写 StateStore 新路径（首写即完成迁移；旧 meta 文件不再改写）。
//
// 序列化格式与既有 save 的 map JSON 逐字一致（json.MarshalIndent(entries, "", "  ")），
// 保证「读旧格式兼容」（既有 dedup.json 可直接被适配器载入）。
//
// 引用计数跨节点强一致：本期靠「写面唯一（LeaderElector）」已够——单 key 快照 +
// 只有主节点写，无 CAS 争抢；逐 checksum CAS 留作 P1 演进（cluster-state-migration.md §2.2）。

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"os"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/state"
)

// DedupStateOptions 是 DedupStore 的 StateStore 后端装配选项（nil st = 本地 JSON 零回归）。
// legacyPath 为旧 <meta>/dedup.json 绝对路径（空 = 不回退读旧文件，纯 StateStore 形态）。
type DedupStateOptions struct {
	St         state.StateStore
	Key        string // StateStore key（dedup/<owner>/all）
	LegacyPath string
}

// stateDedupStore 是 DedupStore 内嵌的 StateStore 后端委托（nil = 未装配本地形态）。
type stateDedupStore struct {
	st         state.StateStore
	key        string
	legacyPath string
	logger     *slog.Logger
	loaded     bool // 是否已从 StateStore 载入（防 Get 未命中时重复回退读旧文件）
}

// loadStateLocked 载入 StateStore 快照（调用方持 ds.mu 写锁）：StateStore 优先；
// 未命中回退旧 meta 文件；都不存在 → 空台账。损坏值记日志 + 空台账（尽力而为，
// 与既有 NewDedupStore 语义对齐）。
func (sd *stateDedupStore) loadStateLocked(ds *DedupStore) {
	if sd.loaded {
		return
	}
	ctx := context.Background()
	data, err := sd.st.Get(ctx, sd.key)
	if err != nil {
		if !errors.Is(err, state.ErrKeyNotFound) {
			sd.logger.Warn("dedup 存储: StateStore 读取失败，将使用空存储", "key", sd.key, "error", err)
			sd.loaded = true
			return
		}
		var ok bool
		data, ok = sd.loadStateFromLegacy()
		if !ok {
			sd.loaded = true
			return
		}
	}
	sd.unmarshalStateEntries(ds, data)
	sd.loaded = true
}

// loadStateFromLegacy 回退读旧 meta 文件（StateStore 未命中时）。返回 (data, ok)；
// 未装配 legacyPath 或旧文件不存在/读取失败（非 IsNotExist 也记日志）→ (nil, false)，
// 调用方据此置 loaded 走空台账。
func (sd *stateDedupStore) loadStateFromLegacy() ([]byte, bool) {
	if sd.legacyPath == "" {
		return nil, false
	}
	data, err := os.ReadFile(sd.legacyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			sd.logger.Warn("dedup 存储: 读取旧 meta 文件失败，将使用空存储", "path", sd.legacyPath, "error", err)
		}
		return nil, false
	}
	return data, true
}

// unmarshalStateEntries 解析 StateStore/旧文件载入的数据为台账；损坏值记日志 + 空台账
// （尽力而为，与既有 NewDedupStore 语义对齐）。空数据不处理。
func (sd *stateDedupStore) unmarshalStateEntries(ds *DedupStore, data []byte) {
	if len(data) == 0 {
		return
	}
	if jerr := json.Unmarshal(data, &ds.entries); jerr != nil {
		sd.logger.Warn("解析 dedup 存储失败，将使用空存储", "key", sd.key, "error", jerr)
		ds.entries = make(map[string]*dedupEntry)
	}
}

// saveState 把全量快照写 StateStore（单写：迁移后旧 meta 不再改写）。
func (sd *stateDedupStore) saveState(ds *DedupStore) error {
	snapshot := make(map[string]*dedupEntry, len(ds.entries))
	maps.Copy(snapshot, ds.entries)
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	ctx := context.Background()
	if err := sd.st.Put(ctx, sd.key, data); err != nil {
		return errors.Join(nil, err) // 保持既有 save 的 error 形状
	}
	return nil
}

// withState 装配 StateStore 后端（dedup.go NewDedupStore 调用方传 opts 时生效）。
func withState(ds *DedupStore, opts *DedupStateOptions) {
	if opts == nil || opts.St == nil {
		return
	}
	logger := ds.logger
	if logger == nil {
		logger = slog.Default()
	}
	ds.state = &stateDedupStore{
		st:         opts.St,
		key:        opts.Key,
		legacyPath: opts.LegacyPath,
		logger:     slogutil.Default(logger),
	}
}

// StateBackend 返回已装配的 StateStore 后端（nil = 本地 JSON 零回归）。
// 仅供装配层测试断言使用（领域包内无生产消费方）。
func (ds *DedupStore) StateBackend() state.StateStore {
	if ds.state == nil {
		return nil
	}
	return ds.state.st
}
