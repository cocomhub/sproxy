// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

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
//
// **dlink 复用（2026-10-21 缺陷修复）**：同一 key 的多次段读（salt + N 个 blocklet 段）
// 复用同一张直链（`cachedDlink`，本地 TTL 60s）——真机实测每次 `LocateDownload` ~0.3s，
// 旧实现每段重签 ⇒ 单文件随机读凭空多付 (N+1)×0.3s。若 CDN 返回非 206（签名失效/被踢），
// **失效缓存并重签一次**（自愈；不把过期直链的失败暴露给调用方）。
//
// **长度契约**（2026-10-14 跨层扫雷修复，与 sync.RangeReader 注释同源）：
//   - `size == 0` ⇒ 空区间（不发请求，返回空流）——原先用「尾开区间」`bytes=offset-`，
//     会把**整个尾巴**当作请求区间返回（越出请求区间）；
//   - `size < 0` ⇒ 非法区间（错误）；
//   - `size > 0` ⇒ 校验 206 的 `Content-Range` 未被服务端**钳制**（钳制 = 静默短读，
//     原先直接返回 body ⇒ 上层看到「区间读完」而实际少字节），并用 RangeExactReader
//     兜底（Content-Range 缺失时也能拒掉提前 EOF）。
func (s *Storage) GetRange(ctx context.Context, key string, offset, size int64) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	if size < 0 || offset < 0 {
		return nil, fmt.Errorf("%w: 非法区间 offset=%d size=%d", ErrInvalidParam, offset, size)
	}
	if size == 0 {
		return syncpkg.EmptyRangeReader(), nil
	}
	// 第一次用（可能缓存命中的）直链；非 206 时失效重签一次（自愈）。
	dlink, ok, err := s.cachedDlink(ctx, key)
	if err != nil {
		return nil, err
	}
	if !ok || dlink == "" {
		return nil, fmt.Errorf("baidupcs: 无直链（无法 Range 读取 %s）", key)
	}
	rc, rerr := s.rangeGetWithDlink(ctx, key, dlink, offset, size)
	if rerr == nil {
		return rc, nil
	}
	// 直链可能已失效（签名过期 / 被服务端踢）：失效缓存 → 重签一次 → 重试一次。
	// 重试仍失败即如实上抛（不无限重试；签名失败是真故障）。
	s.invalidateDlink(key)
	dlink2, ok2, err2 := s.cachedDlink(ctx, key)
	if err2 != nil {
		return nil, err2
	}
	if !ok2 || dlink2 == "" {
		return nil, rerr
	}
	return s.rangeGetWithDlink(ctx, key, dlink2, offset, size)
}

// rangeGetWithDlink 用给定直链发一次 Range GET 并校验响应。
func (s *Storage) rangeGetWithDlink(ctx context.Context, key, dlink string, offset, size int64) (io.ReadCloser, error) {
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, dlink, nil)
	if rerr != nil {
		return nil, fmt.Errorf("baidupcs: 构造 Range 请求失败: %w", rerr)
	}
	// 区间 [offset, offset+size-1]（size>0 恒为闭区间——size≡0 已在上层按空区间返回）。
	req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-"+strconv.FormatInt(offset+size-1, 10))
	resp, derr := s.httpc.Do(req)
	if derr != nil {
		return nil, fmt.Errorf("baidupcs: Range GET %s 失败: %w", key, derr)
	}
	return validateRangeResponse(resp, key, offset, size, s.log)
}

// dlinkTTL 是本地直链缓存有效期（百度 dlink 自签有效 8h；本地取 60s——失效先于
// 签名到期，且远小于「同一文件的段读突发窗口」，能稳定复用）。
const dlinkTTL = 60 * time.Second

// cachedDlink 返回 key 的直链（命中未过期缓存即复用；否则签发并写入缓存）。
// 并发安全：同 key 并发段读只签发一次（持锁串行化签发）。
func (s *Storage) cachedDlink(ctx context.Context, key string) (string, bool, error) {
	s.dlinkMu.Lock()
	defer s.dlinkMu.Unlock()
	if e, ok := s.dlinkCache[key]; ok && time.Now().Before(e.exp) && e.url != "" {
		return e.url, true, nil
	}
	dlink, ok, err := s.DirectURL(ctx, key)
	if err != nil || !ok || dlink == "" {
		// 失败/不支持：不缓存（下次重试时重新试签发）。
		return dlink, ok, err
	}
	s.dlinkCache[key] = dlinkEntry{url: dlink, exp: time.Now().Add(dlinkTTL)}
	return dlink, true, nil
}

// invalidateDlink 清除 key 的直链缓存（直链失效时调用，下次取用即重签）。
func (s *Storage) invalidateDlink(key string) {
	s.dlinkMu.Lock()
	delete(s.dlinkCache, key)
	s.dlinkMu.Unlock()
}

// validateRangeResponse 校验 Range GET 响应并返回**精确长度**区间流（真实 Storage 与测试
// 替身 fakeStorage **共用**：2026-10-14 扫雷发现替身复制了一份 Range 语义 ⇒ 真实实现的
// 长度契约（size==0 尾开区间、无钳制校验）长期无覆盖且与替身行为不一致）。
func validateRangeResponse(resp *http.Response, key string, offset, size int64, log *slog.Logger) (io.ReadCloser, error) {
	if resp.StatusCode != http.StatusPartialContent {
		resp.Body.Close()
		return nil, fmt.Errorf("baidupcs: Range GET %s 返回 %d（期望 206）", key, resp.StatusCode)
	}
	// 服务端钳制判据（fail-closed）：206 的 Content-Range 必须覆盖请求区间。
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		start, end, cok := parseContentRange(cr)
		if !cok {
			resp.Body.Close()
			return nil, fmt.Errorf("baidupcs: Range GET %s 的 Content-Range %q 无法解析", key, cr)
		}
		if start != offset || end < offset+size-1 {
			resp.Body.Close()
			return nil, fmt.Errorf("baidupcs: Range GET %s 区间被钳制（请求 [%d,%d)，实际 [%d,%d]）——拒绝静默短读",
				key, offset, offset+size, start, end)
		}
	} else if log != nil {
		// 部分 CDN 不回 Content-Range（协议要求但现实有例外）：不改行为（不引入脆弱失败），
		// 由 RangeExactReader 的提前 EOF 判据兜底，并留可观测线索。
		log.Debug("baidupcs: Range GET 无 Content-Range，改由精确长度守卫校验", "key", key)
	}
	return syncpkg.ExactRangeReader(resp.Body, size), nil
}

// parseContentRange 解析 `bytes <start>-<end>/<total>`（total 允许 `*`），返回闭区间端点。
func parseContentRange(raw string) (start, end int64, ok bool) {
	s := strings.TrimSpace(raw)
	rest, found := strings.CutPrefix(strings.ToLower(s), "bytes ")
	if !found {
		return 0, 0, false
	}
	span, _, found := strings.Cut(strings.TrimSpace(rest), "/")
	if !found {
		return 0, 0, false
	}
	startS, endS, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	st, err := strconv.ParseInt(strings.TrimSpace(startS), 10, 64)
	if err != nil {
		return 0, 0, false
	}
	en, err := strconv.ParseInt(strings.TrimSpace(endS), 10, 64)
	if err != nil || en < st {
		return 0, 0, false
	}
	return st, en, true
}
