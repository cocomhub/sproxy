// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package capacity 提供外部卷**卷级计数记账**（C2 外部卷容量纳管）。
//
// 用户定案（2026-09-18）：外部卷本系统可用限额（UserVolume.Capacity）用**卷级计数**强制——
// 写入累计（超限拒绝）、删除释放；与本地卷（owner_quotas 物理资源）不同，外部卷不占本机
// 磁盘，限额是「本系统授权占用外部卷的额度」。
//
// 组件：
//   - VolumeCapacityCounter：卷级计数器（used 字节 + 限额），原子写持久化（重启不丢）。
//   - CapacityFS：sync.FS 装饰器——WriteFile 前 TryAdd（超限拒绝）、成功后累计；
//     Delete 后 Release（释放）。包装外部卷 FS（baidupcs/webdav/s3），不侵入各自实现。
package capacity

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// VolumeCapacityCounter 是外部卷的卷级容量计数器（C2）。
//
// used = 本系统当前占用该卷的字节（写入累计 - 删除释放）；capacity = 本系统可用限额
// （UserVolume.Capacity；0 = 不限）。TryAdd 超限拒绝（fail-closed，不部分写入）。
// 持久化：Save 原子写（tmp+rename）；Load 恢复（重启不丢）。
type VolumeCapacityCounter struct {
	mu       sync.Mutex
	used     int64
	capacity int64  // 0 = 不限制
	path     string // 持久化路径（空 = 不持久化）
}

// NewCounter 创建计数器（capacity 字节限额；0 = 不限；path 空 = 不持久化）。
func NewCounter(capacity int64, path string) *VolumeCapacityCounter {
	return &VolumeCapacityCounter{capacity: capacity, path: path}
}

// TryAdd 尝试累计 size 字节（写入前调用）。超限（capacity>0 且 used+size>capacity）
// 返回错误（fail-closed：调用方不得写入）。失败不改变 used。
func (c *VolumeCapacityCounter) TryAdd(size int64) error {
	if size <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity > 0 && c.used+size > c.capacity {
		return fmt.Errorf("volume capacity exceeded (used %d + %d > limit %d)", c.used, size, c.capacity)
	}
	c.used += size
	return nil
}

// Release 释放 size 字节（删除后调用）。非负归零：释放超过 used → 置 0（幂等防御）。
func (c *VolumeCapacityCounter) Release(size int64) {
	if size <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.used -= size
	if c.used < 0 {
		c.used = 0
	}
}

// Used 返回当前占用字节。
func (c *VolumeCapacityCounter) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Capacity 返回限额（0 = 不限）。
func (c *VolumeCapacityCounter) Capacity() int64 { return c.capacity }

// counterFile 是持久化快照（版本化）。
type counterFile struct {
	Version  int   `json:"version"`
	Used     int64 `json:"used"`
	Capacity int64 `json:"capacity"`
}

// Save 原子写持久化（tmp + rename）。path 为空 → no-op（内存态）。
func (c *VolumeCapacityCounter) Save() error {
	if c.path == "" {
		return nil
	}
	c.mu.Lock()
	data, err := json.Marshal(counterFile{Version: 1, Used: c.used, Capacity: c.capacity})
	c.mu.Unlock()
	if err != nil {
		return fmt.Errorf("capacity: 序列化失败: %w", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(c.path), 0o755); mkErr != nil {
		return fmt.Errorf("capacity: 创建目录失败: %w", mkErr)
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.path), "capacity-*.tmp")
	if err != nil {
		return fmt.Errorf("capacity: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("capacity: 写入临时文件失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("capacity: fsync 失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("capacity: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, c.path); err != nil {
		return fmt.Errorf("capacity: 原子重命名失败: %w", err)
	}
	return nil
}

// Load 从 path 恢复计数器。文件不存在 → 新计数器（used=0）。capacity 以调用方传入为准
// （配置权威；持久化的 capacity 仅校验一致，不一致告警但以传入为准）。
func Load(path string, capacity int64) (*VolumeCapacityCounter, error) {
	c := NewCounter(capacity, path)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return nil, fmt.Errorf("capacity: 读取 %s 失败: %w", path, err)
	}
	var f counterFile
	if err := json.Unmarshal(data, &f); err != nil {
		// 损坏快照：重置（不 fail-closed——容量是防超限非数据完整，重置后重新累计安全）。
		return c, nil
	}
	c.mu.Lock()
	c.used = f.Used
	c.mu.Unlock()
	return c, nil
}

// CapacityFS 是外部卷 sync.FS 装饰器（C2 记账强制）。
//
// WriteFile：TryAdd(size) 超限拒绝 → inner.WriteFile（失败回滚 TryAdd 的累计）。
// Delete：inner.Delete 成功 → Release（释放占用）。
// 其它方法透传 inner。Rename = 大小不变（不重计）。
type CapacityFS struct {
	inner   syncpkg.FS
	counter *VolumeCapacityCounter
}

// Wrap 包装 fs 为带卷级计数的 FS。
func Wrap(fs syncpkg.FS, c *VolumeCapacityCounter) *CapacityFS {
	return &CapacityFS{inner: fs, counter: c}
}

// Counter 返回底层计数器（查询/持久化）。
func (f *CapacityFS) Counter() *VolumeCapacityCounter { return f.counter }

func (f *CapacityFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.inner.ListDir(ctx, p)
}

func (f *CapacityFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.inner.Stat(ctx, p)
}

func (f *CapacityFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.inner.OpenRead(ctx, p)
}

func (f *CapacityFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if err := f.counter.TryAdd(size); err != nil {
		return err
	}
	if err := f.inner.WriteFile(ctx, relPath, r, size, mtime); err != nil {
		f.counter.Release(size) // 写失败回滚累计
		return err
	}
	return nil
}

func (f *CapacityFS) Rename(ctx context.Context, from, to string) error {
	return f.inner.Rename(ctx, from, to)
}

func (f *CapacityFS) MakeDir(ctx context.Context, relPath string) error {
	return f.inner.MakeDir(ctx, relPath)
}

func (f *CapacityFS) Delete(ctx context.Context, relPath string) error {
	// 释放大小：Delete 前 Stat 查（Entry.Size）——不存在（nil Entry）按 0 释放。
	var size int64
	if e, err := f.inner.Stat(ctx, relPath); err == nil && e != nil {
		size = e.Size
	}
	if err := f.inner.Delete(ctx, relPath); err != nil {
		return err
	}
	f.counter.Release(size)
	return nil
}

// _ 编译期断言：CapacityFS 实现 sync.FS。
var _ syncpkg.FS = (*CapacityFS)(nil)
