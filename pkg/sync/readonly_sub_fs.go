// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrReadOnly 是只读封装根（写保护）哨兵错误：被封装卷引用的目录只读可查、禁止修改
// （WriteFile/Rename/Delete/MakeDir fail-closed）。ListDir/Stat/OpenRead 透传。
var ErrReadOnly = errors.New("sync: 目录已被封装卷占用，只读（禁止修改）")

// ReadonlySubFS 把底层卷 FS 的某个子目录作为**只读**封装根：读方法（ListDir/Stat/OpenRead）
// 拼 subdir 前缀透传到底层，写方法（WriteFile/Rename/Delete/MakeDir）恒返回 ErrReadOnly。
//
// 用途：嵌套封装中被占用子目录的**写保护视图**——底层卷的该子目录一旦被封装卷引用，经只读
// 视图访问即 fail-closed 拒绝写入，与互斥占用（volume_links）配套，防止重复占用/互相破坏。
//
// 防穿越：复用 SubFS.subJoin（internal/fsutil.SanitizeRelPath），保证任何读操作不出 subdir 边界。
type ReadonlySubFS struct {
	sub *SubFS
}

// NewReadonlySubFS 构造以 root 的 subdir 为逻辑根的只读封装视图（防穿越由 SubFS 承担）。
func NewReadonlySubFS(root FS, subdir string) (*ReadonlySubFS, error) {
	sub, err := NewSubFS(root, subdir)
	if err != nil {
		return nil, err
	}
	return &ReadonlySubFS{sub: sub}, nil
}

func (f *ReadonlySubFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	return f.sub.ListDir(ctx, path)
}

func (f *ReadonlySubFS) Stat(ctx context.Context, path string) (*Entry, error) {
	return f.sub.Stat(ctx, path)
}

func (f *ReadonlySubFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return f.sub.OpenRead(ctx, path)
}

func (f *ReadonlySubFS) WriteFile(ctx context.Context, path string, _ io.Reader, _ int64, _ int64) error {
	return fmt.Errorf("%w: %s", ErrReadOnly, path)
}

func (f *ReadonlySubFS) Rename(ctx context.Context, from, to string) error {
	return fmt.Errorf("%w: %s→%s", ErrReadOnly, from, to)
}

func (f *ReadonlySubFS) Delete(ctx context.Context, path string) error {
	return fmt.Errorf("%w: %s", ErrReadOnly, path)
}

func (f *ReadonlySubFS) MakeDir(ctx context.Context, path string) error {
	return fmt.Errorf("%w: %s", ErrReadOnly, path)
}

var _ FS = (*ReadonlySubFS)(nil)
