// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package federated 实现联邦卷只读后端（roadmap 3.3 P2 联邦卷 F1 片）。
//
// 联邦卷 = 把远端 hub 的卷以只读挂载暴露到本地卷视图。本包是**只读适配层**
// （不触碰装配/网络）：Reader 接口由装配层注入（经 FileClient 隧道调远端 hub
// API），federated.FS 把它适配为 sync.FS（写方法恒 ErrReadOnly——fail-closed）。
package federated

import (
	"context"
	"errors"
	"fmt"
	"io"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ErrReadOnly 是联邦卷只读语义的哨兵错误（写方法恒返回；调用方按只读处理）。
var ErrReadOnly = errors.New("federated: 联邦卷只读（写操作不支持）")

// Reader 是联邦卷读能力的注入接口（装配层实现：经 FileClient 隧道调远端 hub）。
type Reader interface {
	// ListDir 列远端卷内目录（path 为卷内相对路径，"" = 卷根）。
	ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error)
	// Stat 取远端卷内路径元信息。
	Stat(ctx context.Context, path string) (*syncpkg.Entry, error)
	// OpenRead 打开远端卷内路径读流。
	OpenRead(ctx context.Context, path string) (io.ReadCloser, error)
}

// FS 是联邦卷 sync.FS 适配：读方法转发 Reader；写方法转发 Writer（roadmap P2
// 联邦卷回写：装配层注入 remote.Client.FS(ref) 含写面），未注入 Writer 恒
// ErrReadOnly（零回归 fail-closed）。
type FS struct {
	r Reader
	w syncpkg.FS
}

// New 构造只读联邦卷适配（r 为注入的远端读实现；nil 拒绝——fail-fast）。
func New(r Reader) (*FS, error) {
	if r == nil {
		return nil, fmt.Errorf("federated: Reader 注入为空（联邦卷需远端读实现）")
	}
	return &FS{r: r}, nil
}

// WithWriter 注入写面（sync.FS 含 WriteFile/Rename/Delete/MakeDir；装配层传
// remote.Client.FS(ref)）。未调用 → 写方法恒 ErrReadOnly。
func (f *FS) WithWriter(w syncpkg.FS) *FS {
	f.w = w
	return f
}

// ListDir 列远端卷目录（只读转发）。
func (f *FS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	return f.r.ListDir(ctx, path)
}

// Stat 取远端卷路径元信息（只读转发）。
func (f *FS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return f.r.Stat(ctx, path)
}

// OpenRead 打开远端卷读流（只读转发）。
func (f *FS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return f.r.OpenRead(ctx, path)
}

// WriteFile 写文件：注入 Writer 时转发；否则 ErrReadOnly（fail-closed）。
func (f *FS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	if f.w == nil {
		return ErrReadOnly
	}
	return f.w.WriteFile(ctx, path, r, size, mtime)
}

// Rename 联邦卷只读：恒 ErrReadOnly。
func (f *FS) Rename(ctx context.Context, from, to string) error {
	if f.w == nil {
		return ErrReadOnly
	}
	return f.w.Rename(ctx, from, to)
}

// Delete 联邦卷只读：恒 ErrReadOnly。
func (f *FS) Delete(ctx context.Context, path string) error {
	if f.w == nil {
		return ErrReadOnly
	}
	return f.w.Delete(ctx, path)
}

// MakeDir 联邦卷只读：恒 ErrReadOnly。
func (f *FS) MakeDir(ctx context.Context, path string) error {
	if f.w == nil {
		return ErrReadOnly
	}
	return f.w.MakeDir(ctx, path)
}
