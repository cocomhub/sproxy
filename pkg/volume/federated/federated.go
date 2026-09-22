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

// FS 是联邦卷只读 sync.FS 适配：读方法转发 Reader；写方法恒 ErrReadOnly。
type FS struct {
	r Reader
}

// New 构造只读联邦卷适配（r 为注入的远端读实现；nil 拒绝——fail-fast）。
func New(r Reader) (*FS, error) {
	if r == nil {
		return nil, fmt.Errorf("federated: Reader 注入为空（联邦卷需远端读实现）")
	}
	return &FS{r: r}, nil
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

// WriteFile 联邦卷只读：恒 ErrReadOnly（fail-closed）。
func (*FS) WriteFile(context.Context, string, io.Reader, int64, int64) error {
	return ErrReadOnly
}

// Rename 联邦卷只读：恒 ErrReadOnly。
func (*FS) Rename(context.Context, string, string) error { return ErrReadOnly }

// Delete 联邦卷只读：恒 ErrReadOnly。
func (*FS) Delete(context.Context, string) error { return ErrReadOnly }

// MakeDir 联邦卷只读：恒 ErrReadOnly。
func (*FS) MakeDir(context.Context, string) error { return ErrReadOnly }
