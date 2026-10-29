// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package checksum 是文件校验和台账：维护「文件名 → SHA-256」的映射并持久化到磁盘，
// 供上传/下载/删除/重命名等文件操作复用。
package checksum

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/internal/slogutil"
	"github.com/cocomhub/sproxy/pkg/storage"
)

const chkStorePersistFailed = "checksum 存储持久化失败"

// retryPersistFailed 是校验和台账重试持久化失败告警文案（state_adapter/store 共用）。
const retryPersistFailed = "重试持久化失败"

// ChecksumStoreIface 定义 ChecksumStore 的业务接口，方便测试替身。
type ChecksumStoreIface interface {
	Get(filename string) (string, bool)
	Set(filename, checksum string)
	Delete(filename string)
	Rename(from, to string)
	DeletePrefix(prefix string)
	GetAll() map[string]string
}

// VolumeKeyedStore 是**可选扩展**：卷维度键读写（设计 §15.1.1）。
//
// 为什么单独成接口而不是塞进 `ChecksumStoreIface`：后者被多处测试替身实现，扩接口会强制
// 所有替身补方法（噪音）；消费方按需断言本接口，未实现则回落无卷方法（零回归）。
type VolumeKeyedStore interface {
	GetInVolume(vol, filename string) (string, bool)
	SetInVolume(vol, filename, checksum string)
	DeleteInVolume(vol, filename string)
	DeletePrefixInVolume(vol, prefix string)
	RenameInVolume(vol, from, to string)
	SnapshotInVolume(vol string) map[string]string
	SnapshotLegacy() map[string]string
}

var _ VolumeKeyedStore = (*ChecksumStore)(nil)

// Flusher 是**可选扩展**：立即把写合并窗口内的改动落盘（未实现 ⇒ 调用方无需处理，
// 定时器到期/Close 时仍会落盘）。
type Flusher interface{ Flush() error }

var _ Flusher = (*ChecksumStore)(nil)

// Closer 是**可选扩展**：停止落盘合并定时器并做最终落盘（生命周期结束时调用）。
type Closer interface{ Close() error }

var _ Closer = (*ChecksumStore)(nil)

// ChecksumStore 维护「文件名 → SHA-256」台账并落盘到**租户卷内的 `<meta>/checksums.json`**，
// 供 upload/download/delete/rename 操作复用。
//
// **经 `storage.Root` 读写**：卷自己决定是否加密（使用方不理解加密态——非加密卷 = 普通读写；
// 加密卷 = 卷内透明加解密），同时天然继承封印判定（封装卷占用区间写入恒拒）。
// 此前是「绝对路径 + 裸 os I/O」：台账键是**用户可见相对路径**，在加密卷上明文落盘会让加密
// 承诺落空，并绕过封印。
type ChecksumStore struct {
	mu        sync.RWMutex
	saveMu    sync.Mutex // 串行化 save 的 tmp 写 + 原子替换（Windows 上并发 Rename 会抖动）
	root      *storage.Root
	rel       string            // 卷内相对路径（生产 = "meta/checksums.json"）
	checksums map[string]string // filename -> sha256 hex
	logger    *slog.Logger

	// writeSnap 是落盘实现（构造注入，测试可计数）；merger 负责写合并（见 save_merger.go）。
	writeSnap func(data []byte) error
	merger    *saveMerger
}

// volumeKeySep 是卷维度复合键的分隔符（NUL：卷名与 rel 都不含，无歧义）。
const volumeKeySep = "\x00"

// volumeKey 拼接卷维度键（`<vol>\x00<rel>`）。
//
// 为什么要卷维度（设计 2026-10-28-volume-state-maintenance §15.1.1）：旧台账键是**租户相对
// rel**（`user/<rel>`），同一 owner 在多卷上的同名文件会互相覆盖 ⇒ list 显示的 checksum
// 张冠李戴。新写入改用卷维度键；旧键保留为**只读兼容**（升级前的数据仍可读）。
func volumeKey(vol, filename string) string { return vol + volumeKeySep + filename }

// SetInVolume 以卷维度键记录 checksum（`<vol>\x00<rel>`）。
func (cs *ChecksumStore) SetInVolume(vol, filename, checksum string) {
	key := volumeKey(vol, filename)
	cs.mu.Lock()
	cs.checksums[key] = checksum
	cs.mu.Unlock()
	cs.persist("set_in_volume", key)
}

// DeletePrefixInVolume 删除该卷下以 prefix 开头的卷维度键（rmdir 子树清理用）。
func (cs *ChecksumStore) DeletePrefixInVolume(vol, prefix string) {
	full := volumeKey(vol, prefix)
	cs.mu.Lock()
	removed := 0
	for k := range cs.checksums {
		if strings.HasPrefix(k, full) {
			delete(cs.checksums, k)
			removed++
		}
	}
	cs.mu.Unlock()
	if removed > 0 {
		cs.persist("delete_prefix_in_volume", vol, prefix, removed)
	}
}

// SetManyInVolume 批量写卷维度键（一次落盘；rebuild 类全量回填用）。
func (cs *ChecksumStore) SetManyInVolume(vol string, entries map[string]string) {
	if len(entries) == 0 {
		return
	}
	cs.mu.Lock()
	for rel, sum := range entries {
		cs.checksums[volumeKey(vol, rel)] = sum
	}
	cs.mu.Unlock()
	cs.persist("set_many_in_volume", vol, len(entries))
}

// GetInVolume 读卷维度键；miss 时**回退旧无卷键**（迁移兼容：升级前的台账仍可读）。
func (cs *ChecksumStore) GetInVolume(vol, filename string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if v, ok := cs.checksums[volumeKey(vol, filename)]; ok {
		return v, true
	}
	v, ok := cs.checksums[filename]
	return v, ok
}

// DeleteInVolume 删除卷维度键（不动旧键——旧键可能被其它卷回退使用）。
func (cs *ChecksumStore) DeleteInVolume(vol, filename string) {
	key := volumeKey(vol, filename)
	cs.mu.Lock()
	if _, ok := cs.checksums[key]; !ok {
		cs.mu.Unlock()
		return
	}
	delete(cs.checksums, key)
	cs.mu.Unlock()
	cs.persist("delete_in_volume", key)
}

// RenameInVolume 同卷内重命名卷维度键。
func (cs *ChecksumStore) RenameInVolume(vol, from, to string) {
	fromKey, toKey := volumeKey(vol, from), volumeKey(vol, to)
	cs.mu.Lock()
	v, ok := cs.checksums[fromKey]
	if !ok {
		cs.mu.Unlock()
		cs.logger.Warn("ChecksumStore.RenameInVolume: from 路径不存在，跳过", "volume", vol, "from", from, "to", to)
		return
	}
	delete(cs.checksums, fromKey)
	cs.checksums[toKey] = v
	cs.mu.Unlock()
	cs.persist("rename_in_volume", vol, from, to)
}

// SplitVolumeKey 把卷维度复合键拆成 `(vol, rel)`；不是卷维度键 → ok=false。
func SplitVolumeKey(key string) (vol, rel string, ok bool) {
	v, r, found := strings.Cut(key, volumeKeySep)
	if !found {
		return "", key, false
	}
	return v, r, true
}

// SnapshotLegacy 返回**旧无卷键**的快照（过滤卷维度复合键）——迁移兼容用：
// 升级前写入的台账没有卷维度，读侧按「卷键优先 + 旧键兜底」合并。
func (cs *ChecksumStore) SnapshotLegacy() map[string]string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make(map[string]string, len(cs.checksums))
	for k, v := range cs.checksums {
		if !strings.Contains(k, volumeKeySep) {
			out[k] = v
		}
	}
	return out
}

// SnapshotInVolume 返回该卷的 `rel → checksum` 快照（只含卷维度键，键为**裸 rel**）。
func (cs *ChecksumStore) SnapshotInVolume(vol string) map[string]string {
	prefix := vol + volumeKeySep
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make(map[string]string, len(cs.checksums))
	for k, v := range cs.checksums {
		if rel, ok := strings.CutPrefix(k, prefix); ok {
			out[rel] = v
		}
	}
	return out
}

// persist 落盘（失败重试一次并记日志；与 Set/Rename 的既有重试语义一致）。
func (cs *ChecksumStore) persist(op string, args ...any) {
	cs.merger.mark(op, args...)
}

// Flush 立即落盘（有待持久化改动时）；供关键路径/测试显式同步。
func (cs *ChecksumStore) Flush() error { return cs.merger.Flush() }

// Close 停止合并定时器并做最终落盘（幂等）。关闭后的写仍更新内存但不再持久化。
func (cs *ChecksumStore) Close() error { return cs.merger.Close() }

// maxChecksumStoreBytes 是台账读取上界（防注入超大 JSON 的 OOM；每条 ~70B ⇒ 64MiB ≈ 90 万文件）。
// 超限视为损坏/不可用 → 空台账 + Warn（与「不存在/解析失败」同语义：无校验能力 ≠ 文件损坏）。
const maxChecksumStoreBytes = 64 << 20

// NewChecksumStore 创建 ChecksumStore（唯一构造入口）：root = 租户**卷句柄**，
// rel = 卷内相对路径（生产 "meta/checksums.json"）。加载已有记录、清理上次崩溃残留的 .tmp。
// root 为 nil = 未装配：save 恒返回错误（不静默丢写），读为空台账。
func NewChecksumStore(root *storage.Root, rel string, logger *slog.Logger) *ChecksumStore {
	cs := &ChecksumStore{
		root:      root,
		rel:       rel,
		checksums: make(map[string]string),
		logger:    slogutil.Default(logger),
	}
	cs.writeSnap = cs.writeSnapshotToRoot
	cs.merger = newSaveMerger(defaultSaveDebounce, cs.logger, func(uint64) error { return cs.save() })
	if root == nil {
		cs.logger.Warn("checksum 台账未装配卷句柄，将只读空台账且不可持久化")
		return cs
	}

	// 启动时清理上次崩溃残留的 tmp 文件（不影响最终 checksums.json）。
	tmpRel := rel + ".tmp"
	if rmErr := root.Remove(tmpRel); rmErr == nil {
		cs.logger.Info("已清理 checksum tmp 残留", "path", tmpRel)
	} else if !errors.Is(rmErr, os.ErrNotExist) {
		cs.logger.Warn("清理 checksum tmp 残留失败", "path", tmpRel, "error", rmErr)
	}

	data, err := readRootFileBounded(root, rel, maxChecksumStoreBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cs.logger.Info("checksum 存储文件不存在，将使用空存储", "path", rel)
		} else {
			cs.logger.Warn("读取 checksum 存储文件失败", "path", rel, "error", err)
		}
		return cs
	}

	if len(data) == 0 {
		return cs
	}

	if err := json.Unmarshal(data, &cs.checksums); err != nil {
		cs.logger.Warn("解析 checksum 存储文件失败，将使用空存储", "path", rel, "error", err)
		cs.checksums = make(map[string]string)
	}
	return cs
}

// readRootFileBounded 经卷句柄读取文件（卷决定是否解密）+ 上界保护。
func readRootFileBounded(root *storage.Root, rel string, max int64) ([]byte, error) {
	rc, err := root.OpenDecrypted(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("checksum 台账超出上界（%d 字节）: %s", max, rel)
	}
	return data, nil
}

// Get 查询指定文件的 checksum。
func (cs *ChecksumStore) Get(filename string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	v, ok := cs.checksums[filename]
	return v, ok
}

// Set 写入一条 checksum 记录并持久化到磁盘。
func (cs *ChecksumStore) Set(filename, checksum string) {
	cs.mu.Lock()
	cs.checksums[filename] = checksum
	cs.mu.Unlock()

	cs.persist("set", "file_name", filename)
}

// SetMany 批量写入 checksum 记录（一次性导入/重建用）。落盘与其他写方法一样经 debounce 合并，
// 连续 Set 不再逐条全量重写 JSON。空 map 为空操作。
func (cs *ChecksumStore) SetMany(entries map[string]string) {
	if len(entries) == 0 {
		return
	}
	cs.mu.Lock()
	maps.Copy(cs.checksums, entries)
	cs.mu.Unlock()

	cs.persist("set_many", "count", len(entries))
}

// Delete 删除指定文件的 checksum 记录并持久化。
func (cs *ChecksumStore) Delete(filename string) {
	cs.mu.Lock()
	delete(cs.checksums, filename)
	cs.mu.Unlock()

	cs.persist("delete", "file_name", filename)
}

// Rename 将一条 checksum 记录从 from 路径迁移到 to 路径并持久化。
// 如果 to 已存在则被覆盖（与 os.Rename 行为对齐）。
// 注意：save() 内部会获取 cs.mu.RLock()，因此必须在调用 save() 前释放写锁。
func (cs *ChecksumStore) Rename(from, to string) {
	cs.mu.Lock()
	v, ok := cs.checksums[from]
	if !ok {
		cs.mu.Unlock()
		cs.logger.Warn("ChecksumStore.Rename: from 路径不存在，跳过重命名", "from", from, "to", to)
		return
	}
	delete(cs.checksums, from)
	cs.checksums[to] = v
	cs.mu.Unlock()

	cs.persist("rename", "from", from, "to", to)
}

// DeletePrefix 删除指定前缀的所有 checksum 记录（用于目录删除）。
func (cs *ChecksumStore) DeletePrefix(prefix string) {
	cs.mu.Lock()
	for key := range cs.checksums {
		if strings.HasPrefix(key, prefix) {
			delete(cs.checksums, key)
		}
	}
	cs.mu.Unlock()

	cs.persist("delete_prefix", prefix)
}

// Len 返回台账条目数（O(1)；用于「是否需要自愈重建」的判空，避免 GetAll 全量拷贝）。
func (cs *ChecksumStore) Len() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.checksums)
}

// GetAll 返回全部 checksum 记录的副本（filename -> sha256）。
func (cs *ChecksumStore) GetAll() map[string]string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make(map[string]string, len(cs.checksums))
	maps.Copy(result, cs.checksums)
	return result
}

// save 将 checksums map 持久化到磁盘。
// 先持 RLock 深拷贝 map，释放锁后再做 I/O，避免持锁执行磁盘操作。
// 持 saveMu 串行化磁盘写入，防止 Windows 上并发 Rename 覆盖已有的 .checksums.json 失败。
func (cs *ChecksumStore) save() error {
	cs.saveMu.Lock()
	defer cs.saveMu.Unlock()

	cs.mu.RLock()
	snapshot := make(map[string]string, len(cs.checksums))
	maps.Copy(snapshot, cs.checksums)
	cs.mu.RUnlock()

	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	if cs.root != nil {
		if dir := filepath.ToSlash(filepath.Dir(cs.rel)); dir != "." && dir != "" {
			if err := cs.root.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
	}
	if err := cs.writeSnap(data); err != nil {
		return err
	}
	return nil
}

// writeSnapshotToRoot 是默认落盘实现：经卷句柄写 tmp（卷决定是否加密）+ 原子替换。
func (cs *ChecksumStore) writeSnapshotToRoot(data []byte) error {
	if cs.root == nil {
		return errors.New("checksum 台账未装配卷句柄（root 为 nil）")
	}
	tmpRel := cs.rel + ".tmp"
	w, werr := cs.root.OpenFileEncrypted(tmpRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		return werr
	}
	if _, cerr := w.Write(data); cerr != nil {
		_ = w.Close()
		_ = cs.root.Remove(tmpRel)
		return cerr
	}
	if cerr := w.Close(); cerr != nil {
		_ = cs.root.Remove(tmpRel)
		return cerr
	}
	if rerr := cs.root.AtomicRename(tmpRel, cs.rel); rerr != nil {
		_ = cs.root.Remove(tmpRel)
		return rerr
	}
	return nil
}
