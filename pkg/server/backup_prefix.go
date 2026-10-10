// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"strings"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// prefixFS 把「调用方键空间」（备份引擎用的源相对路径 rel）映射到目标 FS 的**桶键空间**
// （`<prefix>/rel`），并在 ListDir/Stat 结果里剥回前缀，使引擎看到的键空间与源一致。
//
// 用于背备份目标（FS-CORE-3）：
//   - 外部卷目标：底层键空间恒 `<owner>/<bucket>/...`（Location.FSPath），前缀 = `<owner>/user/`；
//   - 本地卷目标：底层 root 是 tenant 根（`<storage_root>/<owner>`），前缀 = `user/`。
//
// 若直接把源相对路径（无 owner/bucket 段）喂给 Guard/Wrap：
//  1. Guard 的 BucketOf 会把 `docs/meta/x` 的第二段 meta 误判为 meta 桶 → 合法文件备份失败；
//  2. MetaPath 无桶段兜底使 sidecar 落到用户可见目录（不被隐藏、不在隔离桶）；
//  3. 无 owner 前缀 → 不同 owner 备份同名相对路径互相覆盖（跨租户破坏）。
//
// 前缀化后三者同时消除（Wrap 在 `<owner>/user/rel` 上写 sidecar 到 `<owner>/meta/rel.meta`）。
type prefixFS struct {
	syncpkg.FS
	prefix string
}

// Inner 透明暴露被包装 FS（使 trusted.Innermost / ApplyStagingQuota 能下探到原始卷能力）。
func (p *prefixFS) Inner() syncpkg.FS { return p.FS }

func (p *prefixFS) key(k string) string { return p.prefix + strings.TrimPrefix(k, "/") }
func (p *prefixFS) strip(k string) string {
	return strings.TrimPrefix(k, p.prefix)
}

func (p *prefixFS) entry(e *syncpkg.Entry) *syncpkg.Entry {
	if e != nil {
		e.Path = p.strip(e.Path)
	}
	return e
}

func (p *prefixFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	es, err := p.FS.ListDir(ctx, p.key(path))
	for i := range es {
		es[i].Path = p.strip(es[i].Path)
	}
	return es, err
}

func (p *prefixFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	e, err := p.FS.Stat(ctx, p.key(path))
	return p.entry(e), err
}

func (p *prefixFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return p.FS.OpenRead(ctx, p.key(path))
}

func (p *prefixFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	return p.FS.WriteFile(ctx, p.key(path), r, size, mtime)
}

func (p *prefixFS) WriteIfAbsent(ctx context.Context, path string, r io.Reader, size, mtime int64) (bool, error) {
	if w, ok := p.FS.(syncpkg.WriteIfAbsent); ok {
		return w.WriteIfAbsent(ctx, p.key(path), r, size, mtime)
	}
	return false, nil
}

func (p *prefixFS) Rename(ctx context.Context, from, to string) error {
	return p.FS.Rename(ctx, p.key(from), p.key(to))
}

func (p *prefixFS) Delete(ctx context.Context, path string) error {
	return p.FS.Delete(ctx, p.key(path))
}

func (p *prefixFS) MakeDir(ctx context.Context, path string) error {
	return p.FS.MakeDir(ctx, p.key(path))
}
