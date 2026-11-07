// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// anonymous_limit.go 是「匿名分享直链可下载区间上限」的**唯一探测实现**（用户裁定 2026-10-28）。
//
// 背景：PikPak 分享直链对匿名请求只开放前一段字节（实测约 55%，会随平台策略收紧），
// 超出部分返回 416。hybrid 下载据此把文件切成「匿名区 [0,limit) + 账号区 [limit,total)」。
//
// 能力归属：探测属**领域能力**，实现在 sproxy（本文件）；CLI（pikget）只消费结果，不重复实现。
package pikpak

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// newProbeTransport 构造探测传输层（隔离连接池；不复用下载连接池，避免探测失败污染在途下载）。
func newProbeTransport() http.RoundTripper {
	tr := netutil.IsolatedTransport()
	tr.IdleConnTimeout = 10 * time.Second
	return tr
}

// defaultShareRatioFallback 是探测不可用时的保守回退比例（历史默认值）。
const defaultShareRatioFallback = 0.5

// probeSize 是边界探测请求的探测字节数（1KB，足够拿状态码）。
const probeSize = 1024

// anonLimitProber 用 Range 请求探测匿名直链可读上限（抽成结构体便于注入 client 做测试）。
type anonLimitProber struct{ client *http.Client }

// ProbeAnonymousLimit 探测匿名分享直链**可下载字节上限**：返回 [0,limit)，
// limit=total 表示整文件匿名可下；探测失败返回错误（调用方按保守比例回退）。
//
// 算法：先测末字节（206 ⇒ 全可下）；否则按 1MB 步进二分（lo=可下、hi=不可下），
// 请求数为 O(log(total/1MB))（2GB ≈ 11 次），每次只读 1KB。
func ProbeAnonymousLimit(ctx context.Context, client *http.Client, directLink string, total int64) (int64, error) {
	if directLink == "" {
		return 0, fmt.Errorf("pikpak probe: empty direct link")
	}
	if total <= 0 {
		return 0, fmt.Errorf("pikpak probe: invalid total %d", total)
	}
	if client == nil {
		tr := NewAnonymousProbeClient()
		client = tr
	}
	p := anonLimitProber{client: client}
	return p.probe(ctx, directLink, total)
}

// ProgProbeStep 是探测步进（1MB；导出便于文档/测试对齐）。
const ProgProbeStep = 1 << 20

// probe 执行二分探测。任何**传输层/协议错误**（非 206/416）都返回错误 ——
// 调用方据此按保守比例回退（宁可少用匿名区，也不能把不可读区间当成可读）。
func (p anonLimitProber) probe(ctx context.Context, link string, total int64) (int64, error) {
	lo, hi := int64(0), total
	okLast, err := p.rangeProbe(ctx, link, hi-1, total)
	if err != nil {
		return 0, err
	}
	if okLast {
		return total, nil
	}
	for lo+ProgProbeStep < hi {
		mid := lo + (hi-lo)/2
		mid -= mid % ProgProbeStep
		if mid <= lo {
			break
		}
		ok, perr := p.rangeProbe(ctx, link, mid, total)
		if perr != nil {
			return 0, perr
		}
		if ok {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// rangeProbe 判断从 offset 起能否匿名读取：206 ⇒ 可读；416 ⇒ 不可读；其它 ⇒ 错误。
func (p anonLimitProber) rangeProbe(ctx context.Context, link string, offset, total int64) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return false, fmt.Errorf("pikpak probe: build request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://mypikpak.com/")
	end := min(offset+probeSize-1, total-1)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	resp, err := p.client.Do(req)
	if err != nil {
		return false, fmt.Errorf("pikpak probe: range %d: %w", offset, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeSize))
	switch resp.StatusCode {
	case http.StatusPartialContent:
		return true, nil
	case http.StatusRequestedRangeNotSatisfiable:
		return false, nil
	default:
		return false, fmt.Errorf("pikpak probe: range %d: unexpected status %d", offset, resp.StatusCode)
	}
}

// NewAnonymousProbeClient 构造探测用 HTTP client（隔离连接池 + 短超时：探测失败要快，
// 不能把下载启动拖住）。CLI（pikget）可直接用它做「只探测不下载」。
func NewAnonymousProbeClient() *http.Client {
	tr := newProbeTransport()
	return &http.Client{Timeout: 20 * time.Second, Transport: tr}
}
