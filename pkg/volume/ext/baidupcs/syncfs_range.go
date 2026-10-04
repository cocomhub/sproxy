// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// directURLer 是 StorageFS 消费的可选直链能力（StorageAPI 的窄扩展，P3 解耦保持——
// 不把 *Storage 具体类型塞进 StorageFS；实现方 *Storage 满足编译期断言）。
type directURLer interface {
	// DirectURL 返回 key 的下载直链（明文外部卷 302 跳转）。ok=false = 不支持。
	DirectURL(ctx context.Context, key string) (string, bool, error)
}

// rangeGetter 是 StorageFS 消费的可选区间读取能力（RangeReader 底层：dlink + Range GET）。
// 实现方 *Storage 满足编译期断言。
type rangeGetter interface {
	// GetRange 返回 key 的 [offset, offset+size) 区间字节流（dlink Range GET）。
	GetRange(ctx context.Context, key string, offset, size int64) (io.ReadCloser, error)
}

var (
	_ syncpkg.RangeReader       = (*StorageFS)(nil)
	_ syncpkg.DirectURLProvider = (*StorageFS)(nil)
)

// DirectURL 实现 syncpkg.DirectURLProvider：返回 relPath 的下载直链（明文外部卷
// 302 跳转）。底层 Storage 无直链能力（binary-only）→ ok=false（调用方服务端转发）。
func (f *StorageFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", true, err
	}
	dl, ok := f.s.(directURLer)
	if !ok {
		return "", false, nil
	}
	return dl.DirectURL(ctx, relPath)
}

// OpenRangeRead 实现 syncpkg.RangeReader：定点读取 relPath 的 [offset, offset+size)
// 区间——经 Storage 直链（LocateDownload）发 Range GET，只拉含目标区间的数据段，
// 不下载整文件。区间越出文件 → 错误（fail-closed）。底层无直链能力 → 错误
// （调用方退整流 200；服务端转发场景由 RangeSeeker 消费）。
func (f *StorageFS) OpenRangeRead(ctx context.Context, relPath string, offset, size int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if offset < 0 || size < 0 {
		return nil, fmt.Errorf("%w: 非法区间 offset=%d size=%d", ErrInvalidParam, offset, size)
	}
	rg, ok := f.s.(rangeGetter)
	if !ok {
		return nil, fmt.Errorf("baidupcs: 底层存储不支持 Range 读取（无直链会话）")
	}
	return rg.GetRange(ctx, relPath, offset, size)
}

// GetRange 实现 rangeGetter：获取 relPath 的直链并发送 Range GET 请求。
// dlink 自包含签名短时有效；ctx 取消链路终止请求与 body 读取。
func (s *Storage) GetRange(ctx context.Context, key string, offset, size int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	dlink, ok, err := s.DirectURL(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok || dlink == "" {
		return nil, fmt.Errorf("baidupcs: 无直链（无法 Range 读取 %s）", key)
	}
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, dlink, nil)
	if rerr != nil {
		return nil, fmt.Errorf("baidupcs: 构造 Range 请求失败: %w", rerr)
	}
	// 区间 [offset, offset+size-1]（size==0 时用尾开区间语义：bytes=offset-）。
	if size > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+size-1, 10))
	} else {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, derr := http.DefaultClient.Do(req)
	if derr != nil {
		return nil, fmt.Errorf("baidupcs: Range GET %s 失败: %w", key, derr)
	}
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("baidupcs: Range GET %s 返回 %d（期望 206）", key, resp.StatusCode)
	}
	return resp.Body, nil
}
