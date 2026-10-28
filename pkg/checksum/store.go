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
}

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

	if err := cs.save(); err != nil {
		cs.logger.Error(chkStorePersistFailed, "op", "set", "file_name", filename, "error", err)
		if retryErr := cs.save(); retryErr != nil {
			cs.logger.Error(retryPersistFailed, "op", "set", "file_name", filename, "error", retryErr)
		}
	}
}

// SetMany 批量写入 checksum 记录并**只持久化一次**（一次性导入/重建用：逐条 Set 会产生 N 次
// 全量 JSON 重写）。空 map 为空操作。
func (cs *ChecksumStore) SetMany(entries map[string]string) {
	if len(entries) == 0 {
		return
	}
	cs.mu.Lock()
	maps.Copy(cs.checksums, entries)
	cs.mu.Unlock()

	if err := cs.save(); err != nil {
		cs.logger.Error(chkStorePersistFailed, "op", "set_many", "count", len(entries), "error", err)
		if retryErr := cs.save(); retryErr != nil {
			cs.logger.Error(retryPersistFailed, "op", "set_many", "count", len(entries), "error", retryErr)
		}
	}
}

// Delete 删除指定文件的 checksum 记录并持久化。
func (cs *ChecksumStore) Delete(filename string) {
	cs.mu.Lock()
	delete(cs.checksums, filename)
	cs.mu.Unlock()

	if err := cs.save(); err != nil {
		cs.logger.Error(chkStorePersistFailed, "op", "delete", "file_name", filename, "error", err)
		if retryErr := cs.save(); retryErr != nil {
			cs.logger.Error(retryPersistFailed, "op", "delete", "file_name", filename, "error", retryErr)
		}
	}
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

	if err := cs.save(); err != nil {
		cs.logger.Error(chkStorePersistFailed, "op", "rename", "from", from, "to", to, "error", err)
		if retryErr := cs.save(); retryErr != nil {
			cs.logger.Error(retryPersistFailed, "op", "rename", "from", from, "to", to, "error", retryErr)
		}
	}
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

	if err := cs.save(); err != nil {
		cs.logger.Error(chkStorePersistFailed, "op", "deletePrefix", "prefix", prefix, "error", err)
		if retryErr := cs.save(); retryErr != nil {
			cs.logger.Error(retryPersistFailed, "op", "deletePrefix", "prefix", prefix, "error", retryErr)
		}
	}
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
	if cs.root == nil {
		return errors.New("checksum 台账未装配卷句柄（root 为 nil）")
	}
	if dir := filepath.ToSlash(filepath.Dir(cs.rel)); dir != "." && dir != "" {
		if err := cs.root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmpRel := cs.rel + ".tmp"
	// 经卷句柄写（卷决定是否加密）+ tmp→原子替换（AtomicRename 覆盖 Windows 目标已存在语义，
	// 且自带短退避重试；故不再保留历史上的「重命名失败就直写」回退——失败即真实失败）。
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
