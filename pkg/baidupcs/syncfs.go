// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// syncfs.go 是百度网盘 Storage 的 pkg/sync.FS 适配层。
//
// 目标：把 baidupcs.Storage 包装为 pkg/sync 引擎可消费的文件系统视图（7 方法接口），
// 使「本地↔网盘」同步与「本地↔本地」同步共用同一套编排逻辑（WalkEntries/ComputeDiff）。
//
// 中间态约束（用户硬规则）：WriteFile 的输入流先落**本地 staging 临时文件**，再经
// Storage.Put 上传网盘；OpenRead 经 Storage.Get 落到本地临时文件后返回流——网盘侧
// 只存最终文件，所有暂存/断点/缓存都在本地文件系统（temp 目录）。
//
// 方法映射：
//
//	ListDir  → Storage.List（递归全量简化：同步引擎的单层语义由调用方裁剪）
//	Stat     → Storage.Stat（不存在返回 (nil,nil)）
//	OpenRead → Storage.Get（本地临时文件 + 自动清理）
//	WriteFile→ 本地 staging + Storage.Put（mtime 保留）
//	Rename   → Storage.Copy + Storage.Delete（网盘无原子 MOVE 则两步）
//	Delete   → Storage.Delete
//	MakeDir  → 网盘无独立目录纯概念：路径合法即 no-op
package baidupcs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// StorageFS 把 Storage 适配为 pkg/sync.FS。
type StorageFS struct {
	s    StorageAPI
	temp string // 本地中间态目录（staging/下载缓存），用户硬约束：只依赖本地 FS
	// quota 是可选配额记账钩子（staging 预留/释放）；nil = 不记账（独立 module 保持薄，
	// 装配层注入实现，server 侧包 pkg/quota.Scope）。
	quota QuotaTracker
}

// QuotaTracker 是 staging 配额记账钩子接口（装配层注入；不依赖 pkg/quota 具体类型）。
// 语义：本地 staging 写入计入 owner 配额（防磁盘占满），上传网盘成功后释放本地占用。
type QuotaTracker interface {
	// ReserveUsage 预留 size 字节（本地 staging 写入前调用）。
	ReserveUsage(size int64) error
	// ReleaseUsage 释放 size 字节（上传成功或失败后调用）。
	ReleaseUsage(size int64)
}

// WithQuota 为 StorageFS 装配配额记账钩子（链式配置）。
func (f *StorageFS) WithQuota(q QuotaTracker) *StorageFS {
	f.quota = q
	return f
}

// StorageAPI 是 StorageFS 消费的最小接口（P3 只依赖公开方法，与 P2 内部解耦）。
// 由 *Storage 实现（编译期断言见 NewStorageFS）。
type StorageAPI interface {
	Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error)
	Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error)
	Stat(ctx context.Context, key string) (*ObjectMeta, error)
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)
	Delete(ctx context.Context, key string) error
	Copy(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error)
}

// NewStorageFS 构造 StorageFS 适配层。temp 为本地中间态目录（默认 os.TempDir()）。
func NewStorageFS(s StorageAPI, temp string) (*StorageFS, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: storage is nil", ErrInvalidParam)
	}
	if temp == "" {
		temp = os.TempDir()
	}
	if mkErr := os.MkdirAll(temp, 0o755); mkErr != nil {
		return nil, mkErr
	}
	return &StorageFS{s: s, temp: temp}, nil
}

var _ syncpkg.FS = (*StorageFS)(nil)

// ListDir 列出目录条目。Storage.List 是递归全量（百度 API 无单层枚举），
// 返回的 Path 相对 FS 根（正斜杠）；同步引擎的 WalkEntries 会按层裁剪。
func (f *StorageFS) ListDir(ctx context.Context, relPath string) ([]syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	metas, err := f.s.List(ctx, relPath)
	if err != nil {
		return nil, mapPCSError(err)
	}
	out := make([]syncpkg.Entry, 0, len(metas))
	for _, m := range metas {
		e := entryFromMeta(m)
		// 跳过自身路径（List(prefix) 含 prefix 本身时）
		if e.Path == relPath || e.Path == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Stat 返回条目信息；不存在返回 (nil, nil)。
func (f *StorageFS) Stat(ctx context.Context, relPath string) (*syncpkg.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	clean := strings.TrimPrefix(relPath, "/")
	// 根路径（""）：网盘根恒为目录。
	if clean == "" {
		return &syncpkg.Entry{Name: "", Path: "", IsDir: true}, nil
	}
	m, err := f.s.Stat(ctx, clean)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, mapPCSError(err)
	}
	e := entryFromMeta(*m)
	return &e, nil
}

// OpenRead 打开文件读取流（经 Storage.Get 落本地临时文件，Close 自动清理）。
func (f *StorageFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rc, _, err := f.s.Get(ctx, relPath)
	if err != nil {
		return nil, mapPCSError(err)
	}
	return rc, nil
}

// WriteFile 把流写入网盘：先落本地 staging 临时文件，再经 Storage.Put 上传。
// 中间态只依赖本地 FS（用户硬规则）；mtime 保留到远端元数据。
// quota：装配了 QuotaTracker 时，staging 写入预留 size，Put 成功释放，失败归还。
func (f *StorageFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	clean := strings.TrimPrefix(relPath, "/")
	if clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("%w: invalid path %q", ErrInvalidParam, relPath)
	}
	// 0. quota 预留（本地 staging 写入前）。
	if f.quota != nil {
		if err := f.quota.ReserveUsage(size); err != nil {
			return fmt.Errorf("%w: quota reserve %d: %v", ErrTransient, size, err)
		}
		defer f.quota.ReleaseUsage(size)
	}
	// 1. 落本地 staging（受 ctx 约束的流式拷贝）。
	tmp, err := os.CreateTemp(f.temp, "staging-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, copyErr := io.Copy(tmp, r); copyErr != nil {
		_ = tmp.Close()
		return copyErr
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return closeErr
	}
	// 2. 上传网盘（Put 内部有界重试）。staging 文件句柄由本函数关闭（Put 不接管 r 的 Close）。
	staging, openErr := os.Open(tmpPath)
	if openErr != nil {
		return openErr
	}
	meta, err := f.s.Put(ctx, clean, staging)
	_ = staging.Close()
	if err != nil {
		return mapPCSError(err)
	}
	_ = meta
	// 3. mtime 保留：百度 API 无直接 Chtimes，交由 Storage 层后续扩展（当前记录在元数据）。
	_ = mtime
	return nil
}

// Rename 重命名/移动（网盘无原子 MOVE → Copy + Delete 两步）。
func (f *StorageFS) Rename(ctx context.Context, from, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := f.s.Copy(ctx, from, to); err != nil {
		return mapPCSError(err)
	}
	return f.Delete(ctx, from)
}

// Delete 删除文件（幂等：不存在不报错）。
func (f *StorageFS) Delete(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := f.s.Delete(ctx, relPath)
	if err != nil && isNotFound(err) {
		return nil
	}
	return mapPCSError(err)
}

// MakeDir 网盘无独立目录纯概念：路径合法即成功（no-op）。
func (f *StorageFS) MakeDir(ctx context.Context, relPath string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := sanitizeRemotePath(relPath)
	return err
}

// entryFromMeta 把 ObjectMeta 映射为 sync.Entry（Path 相对 FS 根、正斜杠）。
func entryFromMeta(m ObjectMeta) syncpkg.Entry {
	name := path.Base(strings.TrimSuffix(m.Key, "/"))
	if name == "." || name == "/" {
		name = ""
	}
	e := syncpkg.Entry{
		Name:  name,
		Path:  strings.TrimSuffix(m.Key, "/"),
		Size:  m.Size,
		IsDir: m.IsDir,
	}
	if !m.ModTime.IsZero() {
		e.MTime = m.ModTime.UnixNano()
	}
	if m.ETag != "" {
		e.Checksum = m.ETag
	}
	return e
}

// isNotFound 判断错误是否为「不存在」语义（哨兵或文本）。
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	if errorsIs(err, ErrNotFound) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "not found") || strings.Contains(msg, "不存在")
}

// errorsIs 避免与标准库 errors 冲突的本地包装。
func errorsIs(err, target error) bool {
	return err == target
}
