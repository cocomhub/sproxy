// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"io"

	"github.com/cocomhub/sproxy/pkg/files"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// FS 返回一个以 `ref` 的 (节点, 卷) 为根的 **`sync.FS` 实现**。
//
// 这是「远程访问」与「同步编排」的唯一接缝：`pkg/sync` 的引擎只认 `sync.FS`，因此
// 「远程同步」与「本地同步」共用同一套枚举/差异/冲突/编排逻辑，**不需要第二份读写实现**。
//
// 路径语义：FS 的根 = 该卷内 owner 命名空间的根（对端按 mesh 授权把请求映射到该 owner），
// 所有 path 参数为**卷内相对路径**（正斜杠、无前导斜杠），与 `Ref.Path` 同构。
//
// 写方法自 Y 二期 P3-c 起已实现（走对端写面 `volwrite`，经 `WithWriteDialer` 注入）。
// 接口形状自 P1 起按最终形态固化，实现填充未改动任何调用方——这正是当初「写方法先返回
// 未实现错误」的防返工收益。
func (c *Client) FS(ref Ref) syncpkg.FS {
	return &remoteFS{c: c, ref: ref.Root()}
}

// remoteFS 是 sync.FS 的远程实现（只读部分已实现，写部分待写批次）。
type remoteFS struct {
	c   *Client
	ref Ref // 只含 Node/Volume
}

// 编译期断言：接口形状即最终形态（含 4 个写方法）。
var _ syncpkg.FS = (*remoteFS)(nil)

// ListDir 列出目录下的顶层条目（不递归）。
//
// 返回条目的 Path 为**FS 根相对的完整路径**（目录前缀 + 条目名），满足 sync.FS 的
// 路径契约（否则 Engine 的 src↔dst 映射与 conflict_rename 会错位）。
func (f *remoteFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	dir, err := normalizeRelPath(path)
	if err != nil {
		return nil, err
	}
	infos, err := f.c.List(ctx, Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: dir})
	if err != nil {
		return nil, err
	}
	out := make([]syncpkg.Entry, 0, len(infos))
	for _, fi := range infos {
		out = append(out, toEntry(dir, fi))
	}
	return out, nil
}

// Stat 返回条目元信息；不存在返回 (nil, nil)。
func (f *remoteFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	p, err := normalizeRelPath(path)
	if err != nil {
		return nil, err
	}
	if p == "" {
		// 卷内 owner 根：对端没有「根条目」语义（/remote/stat 需要具体路径），按 FS 契约
		// 以目录条目表达（Engine 只用 IsDir 决定是否递归、用 Path 做映射）。
		return &syncpkg.Entry{Path: "", IsDir: true}, nil
	}
	fi, err := f.c.Stat(ctx, Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: p})
	if err != nil {
		return nil, err
	}
	if fi == nil {
		return nil, nil
	}
	e := toEntry("", *fi)
	e.Path = p
	return &e, nil
}

// OpenRead 打开条目供流式读取。
func (f *remoteFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	p, err := normalizeRelPath(path)
	if err != nil {
		return nil, err
	}
	return f.c.Open(ctx, Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: p})
}

// 写方法（Y 二期 P3-c 已实现）：只做「路径归一 → 调 Client 写面方法」，**不实现写语义**
// （checksum 门禁/原子改名/版本/配额/文件锁/卷路由全在对端 `pkg/files` 的域方法里）。
//
// 未配置写面拨号器时返回 `ErrWriteNotConfigured`（fail-closed）：静默成功会让上层把"没写"
// 当成"已写"（数据丢失型缺陷），而回落读面链路会绕过「写面独立授权」的安全边界。

// WriteFile 写入 path；size 为调用方声明的字节数，mtime 为 Unix 秒（0 = 不设置）。
func (f *remoteFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	p, err := normalizeRelPath(path)
	if err != nil {
		return err
	}
	return f.c.WriteFile(ctx, Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: p}, r, size, mtime)
}

// Rename 改名/移动（同节点同卷；A 侧先经读面 Stat 取 checksum）。
func (f *remoteFS) Rename(ctx context.Context, from, to string) error {
	pFrom, err := normalizeRelPath(from)
	if err != nil {
		return err
	}
	pTo, err := normalizeRelPath(to)
	if err != nil {
		return err
	}
	return f.c.Rename(ctx,
		Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: pFrom},
		Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: pTo})
}

// Delete 删除条目（A 侧先经读面 Stat 取 checksum）。
func (f *remoteFS) Delete(ctx context.Context, path string) error {
	p, err := normalizeRelPath(path)
	if err != nil {
		return err
	}
	return f.c.Delete(ctx, Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: p})
}

// MakeDir 建目录（递归由对端 MkdirAll 语义保证，与本地区域方法一致）。
func (f *remoteFS) MakeDir(ctx context.Context, path string) error {
	p, err := normalizeRelPath(path)
	if err != nil {
		return err
	}
	return f.c.MakeDir(ctx, Ref{Node: f.ref.Node, Volume: f.ref.Volume, Path: p})
}

// toEntry 把对端的 FileInfo 转成 sync.Entry。
//
// dir 为空表示条目路径已是完整相对路径（Stat 用），否则用 dir 前缀拼出完整路径（ListDir 用）。
func toEntry(dir string, fi files.FileInfo) syncpkg.Entry {
	p := fi.Name
	if dir != "" {
		p = dir + "/" + fi.Name
	}
	return syncpkg.Entry{
		Name:     fi.Name,
		Path:     p,
		Size:     fi.Size,
		MTime:    fi.ModTime,
		Checksum: fi.Checksum,
		IsDir:    fi.IsDir,
	}
}
