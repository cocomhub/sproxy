// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// range_http_retry_test.go 锁定 Range GET（dlink 下行）客户端的**重试语义**（2026-11-08）：
// dlink 来自百度 CDN，瞬时 5xx/连接抖动此前会让整个分块读失败（上层要重签直链 + 重试），
// 现由 httpc 编排按 GET 幂等自动重试；4xx（签名错/直链失效）**不重试**（fail-fast，
// 交由上层 invalidate + 重签流程）。
//
// 变异：把 newRangeHTTPClient 的 Retry 去掉（或换成 PolicyNone）⇒ 本用例红。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRangeHTTPClient_RetriesTransient5xx(t *testing.T) {
	t.Parallel()
	const content = "0123456789abcdef"
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		http.ServeContent(w, r, "f.bin", zeroTime, strings.NewReader(content))
	}))
	t.Cleanup(srv.Close)

	cl := newRangeHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Range", "bytes=0-3")

	resp, derr := cl.Do(t.Context(), req)
	if resp == nil {
		t.Fatalf("瞬时 5xx 后应重试成功，实际无响应: %v（请求次数=%d）", derr, hits.Load())
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("状态码 = %d，期望 206", resp.StatusCode)
	}
	body, rerr := io.ReadAll(resp.Body)
	if rerr != nil {
		t.Fatalf("读 body: %v", rerr)
	}
	if string(body) != content[:4] {
		t.Fatalf("区间内容 = %q，期望 %q", body, content[:4])
	}
	if got := hits.Load(); got < 2 {
		t.Fatalf("期望至少 2 次请求（首次 502 后重试），实际 %d 次 —— 未接入重试编排", got)
	}
}

func TestRangeHTTPClient_4xxNotRetried(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "sign error", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	cl := newRangeHTTPClient()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("构造请求: %v", err)
	}
	req.Header.Set("Range", "bytes=0-3")

	resp, _ := cl.Do(t.Context(), req)
	if resp == nil {
		t.Fatalf("4xx 仍应返回响应（供上层判直链失效）")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("状态码 = %d，期望 403", resp.StatusCode)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("4xx 不应重试（直链失效需上层重签），实际 %d 次", got)
	}
}
