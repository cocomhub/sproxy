// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cocomhub/sproxy/pkg/sync/internal/fsutil"
)

// SubFS 把底层卷 FS 的某个子目录（rel.subdir）作为新的逻辑根，所有操作都加 subdir 前缀
// 转发到底层。供「嵌套封装」使用：封装卷（secretdata/secrets）以「已创建卷 + 新子目录」
// 作为底层根，密文落在 <底层卷>/<subdir>/ 下，与底层卷既有数据隔离。
//
// 防穿越：入参 rel 必须是 FS 根相对路径（不允许 `..`/绝对路径/空字节）——subJoin 用
// internal/fsutil.SanitizeRelPath 校验后拼 subdir 前缀，保证任何操作都逃不出 subdir 边界。
//
// SubFS 是**可写**视图（封装卷需要落密文）。只读封装根（写保护）由同包 ReadonlySubFS 提供。
type SubFS struct {
	root   FS
	subdir string
}

// NewSubFS 构造以 root 的 subdir 为逻辑根的封装子视图（subdir 须为非空相对路径）。
// factory 去重后与新 FS 系列/测试直接构造。
func NewSubFS(root FS, subdir string) (*SubFS, error) {
	if root == nil {
		return nil, fmt.Errorf("SubFS: 底层 FS 为 nil")
	}
	clean, err := fsutil.SanitizeRelPath(subdir)
	if err != nil {
		return nil, fmt.Errorf("SubFS: 非法底层子目录 %q: %w", subdir, err)
	}
	if clean == "" {
		return nil, fmt.Errorf("SubFS: 底层子目录不能为空（需 <卷>/<新子目录>）")
	}
	return &SubFS{root: root, subdir: clean}, nil
}

// subJoin 把相对 rel 拼进 subdir 前缀（两者都相对各自根），先校验 rel 防穿越。
func (f *SubFS) subJoin(rel string) (string, error) {
	clean, err := fsutil.SanitizeRelPath(rel)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return f.subdir, nil
	}
	return f.subdir + "/" + clean, nil
}

// stripSubPrefix 把底层返回的「完整相对路径」条目剥成相对 SubFS 根的子项（对齐 FS 契约：
// ListDir 返回条目的 Path 相对调用方根 = subdir 视图）。底层条目 Path = <subdir>/<child...>，
// 剥掉 <subdir>/ 前缀即可。字段 Name/Size/IsDir 等原样透传。subdir 为 f.subdir。
func stripSubPrefix(subdir string, entries []Entry) []Entry {
	prefix := subdir + "/"
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		e.Path = strings.TrimPrefix(e.Path, prefix)
		out = append(out, e)
	}
	return out
}

func (f *SubFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	p, err := f.subJoin(path)
	if err != nil {
		return nil, err
	}
	entries, err := f.root.ListDir(ctx, p)
	if err != nil {
		return nil, err
	}
	return stripSubPrefix(f.subdir, entries), nil
}

func (f *SubFS) Stat(ctx context.Context, path string) (*Entry, error) {
	p, err := f.subJoin(path)
	if err != nil {
		return nil, err
	}
	return f.root.Stat(ctx, p)
}

func (f *SubFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	p, err := f.subJoin(path)
	if err != nil {
		return nil, err
	}
	return f.root.OpenRead(ctx, p)
}

func (f *SubFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	p, err := f.subJoin(path)
	if err != nil {
		return err
	}
	return f.root.WriteFile(ctx, p, r, size, mtime)
}

func (f *SubFS) Rename(ctx context.Context, from, to string) error {
	fromP, err := f.subJoin(from)
	if err != nil {
		return err
	}
	toP, err := f.subJoin(to)
	if err != nil {
		return err
	}
	return f.root.Rename(ctx, fromP, toP)
}

func (f *SubFS) Delete(ctx context.Context, path string) error {
	p, err := f.subJoin(path)
	if err != nil {
		return err
	}
	return f.root.Delete(ctx, p)
}

func (f *SubFS) MakeDir(ctx context.Context, path string) error {
	p, err := f.subJoin(path)
	if err != nil {
		return err
	}
	return f.root.MakeDir(ctx, p)
}

var _ FS = (*SubFS)(nil)
