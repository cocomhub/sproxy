// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/sync"
)

// NewRemoteHandler 构造「以 ref 的 (节点, 卷) 为根」的 WebDAV handler。
//
// dialer 是到 mesh 节点的拨号器（`remote.RelayDialer` 经 hub 中继；测试注入 mock）。
// ref 只取 Node/Volume（WebDAV 暴露整个卷根；Path 若指定则作为卷内子路径起点）。
// 返回 (handler, close, err)：close 关闭 remote.Client 的缓存链路（http.Server 关闭时调用）。
func NewRemoteHandler(dialer remote.Dialer, ref remote.Ref, opts ...remote.Option) (handler handlerWithClose, err error) {
	if dialer == nil {
		return handlerWithClose{}, fmt.Errorf("webdav: NewRemoteHandler 需要非 nil dialer")
	}
	c := remote.New(dialer, opts...)
	var fs sync.FS
	if ref.Path != "" {
		// 子路径起点：包装 FS 使路径前缀对齐（把卷根映射到 ref.Path 之下）。
		fs = &subPathFS{inner: c.FS(ref), root: ref.Path}
	} else {
		fs = c.FS(ref)
	}
	h := NewHandler(fs)
	if h == nil {
		_ = c.Close()
		return handlerWithClose{}, fmt.Errorf("webdav: NewHandler 返回 nil（fs 为空）")
	}
	return handlerWithClose{Handler: h, closeFn: func() error { return c.Close() }}, nil
}

// handlerWithClose 包装 http.Handler 与资源释放函数。
type handlerWithClose struct {
	http.Handler
	closeFn func() error
}

// Close 释放 remote.Client 的缓存链路（幂等）。
func (h handlerWithClose) Close() error {
	if h.closeFn != nil {
		return h.closeFn()
	}
	return nil
}

// subPathFS 把 inner FS 的根映射到 root 前缀之下：
// 请求 /x → inner 的 root/x。用于 `remote://node/vol/subdir` 把 WebDAV 根对准卷内子目录。
type subPathFS struct {
	inner sync.FS
	root  string
}

func (s *subPathFS) join(p string) string {
	if p == "" {
		return s.root
	}
	if s.root == "" {
		return p
	}
	return s.root + "/" + p
}

func (s *subPathFS) ListDir(ctx context.Context, p string) ([]sync.Entry, error) {
	entries, err := s.inner.ListDir(ctx, s.join(p))
	if err != nil {
		return nil, err
	}
	// 子路径下条目 Path 需相对新根（去 root 前缀）。
	for i := range entries {
		entries[i].Path = trimPrefix(entries[i].Path, s.root)
	}
	return entries, nil
}

func (s *subPathFS) Stat(ctx context.Context, p string) (*sync.Entry, error) {
	e, err := s.inner.Stat(ctx, s.join(p))
	if err != nil || e == nil {
		return e, err
	}
	cp := *e
	cp.Path = trimPrefix(cp.Path, s.root)
	return &cp, nil
}

func (s *subPathFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return s.inner.OpenRead(ctx, s.join(p))
}

func (s *subPathFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	return s.inner.WriteFile(ctx, s.join(p), r, size, mtime)
}

func (s *subPathFS) Rename(ctx context.Context, from, to string) error {
	return s.inner.Rename(ctx, s.join(from), s.join(to))
}

func (s *subPathFS) Delete(ctx context.Context, p string) error {
	return s.inner.Delete(ctx, s.join(p))
}

func (s *subPathFS) MakeDir(ctx context.Context, p string) error {
	return s.inner.MakeDir(ctx, s.join(p))
}

// NewSubPathFSForTest 导出 subPathFS 供测试验证（子路径映射语义）。
func NewSubPathFSForTest(inner sync.FS, root string) sync.FS {
	return &subPathFS{inner: inner, root: root}
}

// trimPrefix 去掉字符串前缀（无匹配时原样返回）。
func trimPrefix(s, prefix string) string {
	if prefix == "" {
		return s
	}
	if s == prefix {
		return ""
	}
	if len(s) > len(prefix) && s[:len(prefix)] == prefix && s[len(prefix)] == '/' {
		return s[len(prefix)+1:]
	}
	return s
}
