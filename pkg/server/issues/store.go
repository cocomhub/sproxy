// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package issues

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store 是事项的持久化后端。
//
// 语义要求：
//   - `Save` **整体覆盖**（调用方持 Manager 锁，不会并发）；
//   - `Load` 在文件不存在时返回空列表且 **不报错**（首次启动的正常路径）；
//   - 实现必须是**崩溃安全**的：写坏一次不能导致审计数据全丢（文件存储用 temp+rename 原子替换）。
type Store interface {
	Load() ([]Item, error)
	Save(items []Item) error
}

// MemStore 是内存存储（测试用；进程内共享，便于验证"重载后仍可见"）。
type MemStore struct {
	mu    sync.Mutex
	items []Item
}

// NewMemStore 构造内存存储。
func NewMemStore() *MemStore { return &MemStore{} }

// Load 返回副本（避免调用方改到内部切片）。
func (s *MemStore) Load() ([]Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Item, len(s.items))
	copy(out, s.items)
	return out, nil
}

// Save 整体覆盖。
func (s *MemStore) Save(items []Item) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Item, len(items))
	copy(out, items)
	s.items = out
	return nil
}

// FileStore 是文件存储（生产用）：单个 JSON 文件，temp+rename **原子替换**。
//
// 为什么是单文件：事项数量天然很小（每类问题每个对象一条，且同对象重复登记会合并），
// 全量覆盖的代价可忽略；换来的是实现简单、可人工直接查看/备份（运维友好）。
type FileStore struct {
	Path string
}

// NewFileStore 构造文件存储（path 为空 ⇒ 退化为内存存储由调用方处理；此处返回错误交由 New 兜底）。
func NewFileStore(path string) *FileStore { return &FileStore{Path: path} }

// Load 读取事项；文件不存在 ⇒ 空列表 + nil（首次启动）。
func (s *FileStore) Load() ([]Item, error) {
	if s == nil || s.Path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Clean(s.Path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("issues: 读取 %s: %w", s.Path, err)
	}
	if len(b) == 0 {
		return nil, nil
	}
	var items []Item
	if uerr := json.Unmarshal(b, &items); uerr != nil {
		// 解析失败不返回半截数据（避免"部分事项丢失"被误当正常）——直接报错，
		// 由 Manager 记录为可感知事项。
		return nil, fmt.Errorf("issues: 解析 %s: %w", s.Path, uerr)
	}
	return items, nil
}

// Save 原子写入（temp + rename）：崩溃/断电不会留下半截 JSON。
func (s *FileStore) Save(items []Item) error {
	if s == nil || s.Path == "" {
		return nil
	}
	// 排序让文件内容稳定（便于 diff/人工审阅）。
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	b, err := json.MarshalIndent(sorted, "", "  ")
	if err != nil {
		return fmt.Errorf("issues: 序列化: %w", err)
	}
	dir := filepath.Dir(s.Path)
	if merr := os.MkdirAll(dir, 0o700); merr != nil {
		return fmt.Errorf("issues: 创建目录 %s: %w", dir, merr)
	}
	tmp, terr := os.CreateTemp(dir, ".issues-*.tmp")
	if terr != nil {
		return fmt.Errorf("issues: 创建临时文件: %w", terr)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // rename 成功后 Remove 会失败（无害）
	if _, werr := tmp.Write(b); werr != nil {
		_ = tmp.Close()
		return fmt.Errorf("issues: 写入临时文件: %w", werr)
	}
	if serr := tmp.Sync(); serr != nil {
		_ = tmp.Close()
		return fmt.Errorf("issues: 同步临时文件: %w", serr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return fmt.Errorf("issues: 关闭临时文件: %w", cerr)
	}
	if rerr := os.Rename(tmpName, s.Path); rerr != nil {
		return fmt.Errorf("issues: 原子替换 %s: %w", s.Path, rerr)
	}
	return nil
}
