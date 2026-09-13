// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"fmt"
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
// 写方法当前返回 `ErrUnsupported`（对端只读面）；接口形状已按最终形态固化，写批次
// （P3）只需填实现，不必改调用方。
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

// 写方法：接口形状已固化，实现待写批次（P3）。
//
// 返回 ErrUnsupported 而非 nil：静默成功会让上层把"没写"当成"已写"（数据丢失型缺陷）。

func (f *remoteFS) WriteFile(context.Context, string, io.Reader, int64, int64) error {
	return fmt.Errorf("WriteFile: %w", ErrUnsupported)
}

func (f *remoteFS) Rename(context.Context, string, string) error {
	return fmt.Errorf("Rename: %w", ErrUnsupported)
}

func (f *remoteFS) Delete(context.Context, string) error {
	return fmt.Errorf("Delete: %w", ErrUnsupported)
}

func (f *remoteFS) MakeDir(context.Context, string) error {
	return fmt.Errorf("MakeDir: %w", ErrUnsupported)
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
