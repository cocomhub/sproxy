// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestRateLimiter_AllowEndpoint_NoRulePasses 无端点规则（默认零配置）→ 任意路径透传 true。
func TestRateLimiter_AllowEndpoint_NoRulePasses(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Second, nil)
	for _, p := range []string{"/", "/download", "/api/files", "/upload/x"} {
		if !rl.AllowEndpoint(p) {
			t.Fatalf("path %s: 无规则应透传 true", p)
		}
	}
}

// TestRateLimiter_AllowEndpoint_ExactPrefixBoundary 覆盖匹配矩阵：
// 精确优先 + "/" 前缀最长匹配 + 非子路径边界（/downloadx 不命中 /download）+ 兜底规则。
func TestRateLimiter_AllowEndpoint_ExactPrefixBoundary(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Second, nil)
	rl.UpdateDimensions(map[string]EndpointLimit{
		"/download":     {Limit: 2, Window: time.Hour},
		"/download/sub": {Limit: 5, Window: time.Hour},
	}, EndpointLimit{Limit: 1, Window: time.Hour}, 0)

	// 精确匹配 /download（limit=2）
	for i := range 2 {
		if !rl.AllowEndpoint("/download") {
			t.Fatalf("/download 第 %d 次: want true", i+1)
		}
	}
	if rl.AllowEndpoint("/download") {
		t.Fatal("/download 第 3 次: want false（精确规则 limit=2）")
	}

	// 精确匹配 /download/sub（独立配额 limit=5，不被 /download 干扰）
	for i := range 5 {
		if !rl.AllowEndpoint("/download/sub") {
			t.Fatalf("/download/sub 第 %d 次: want true", i+1)
		}
	}
	if rl.AllowEndpoint("/download/sub") {
		t.Fatal("/download/sub 第 6 次: want false（精确优先独立配额）")
	}

	// 前缀最长匹配：/download/sub/deep → /download/sub（limit=5 已耗尽）
	if rl.AllowEndpoint("/download/sub/deep") {
		t.Fatal("/download/sub/deep: want false（前缀最长命中 /download/sub 已耗尽）")
	}

	// 前缀匹配：/download/deep → /download（limit=2 已耗尽）
	if rl.AllowEndpoint("/download/deep") {
		t.Fatal("/download/deep: want false（前缀命中 /download 已耗尽）")
	}

	// 边界：/downloadx 不是 /download 的子路径 → 不命中 → 兜底（limit=1）
	if !rl.AllowEndpoint("/downloadx") {
		t.Fatal("/downloadx: want true（兜底第 1 次）")
	}
	if rl.AllowEndpoint("/downloadx") {
		t.Fatal("/downloadx: want false（兜底 limit=1 已耗尽）")
	}

	// 兜底：/other（兜底已耗尽）
	if rl.AllowEndpoint("/other") {
		t.Fatal("/other: want false（兜底已耗尽）")
	}
}

// TestRateLimiter_ConcurrentLimit_Basic AcquireConcurrent/ReleaseConcurrent 配对语义：
// max_concurrent=N 时第 N+1 个获取失败（非阻塞）；释放后恢复。
func TestRateLimiter_ConcurrentLimit_Basic(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Second, nil)
	rl.UpdateDimensions(nil, EndpointLimit{}, 3)
	for i := range 3 {
		if !rl.AcquireConcurrent() {
			t.Fatalf("acquire %d: want true", i)
		}
	}
	if rl.AcquireConcurrent() {
		t.Fatal("第 4 个: want false（max_concurrent=3）")
	}
	rl.ReleaseConcurrent()
	if !rl.AcquireConcurrent() {
		t.Fatal("释放后: want true")
	}
	rl.ReleaseConcurrent()
	rl.ReleaseConcurrent()
	rl.ReleaseConcurrent()
}

// TestRateLimiter_Middleware_ConcurrentLimit429 并发闸走 Middleware：
// max_concurrent=2 时第 3 个并发请求 429，释放后恢复。
// （t.Errorf 断言在子 goroutine 内，测试体不可超时——用 channel 回传结果。）
func TestRateLimiter_Middleware_ConcurrentLimit429(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Second, nil)
	rl.UpdateDimensions(nil, EndpointLimit{}, 2)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	codes := make(chan int, 2)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			codes <- sendAllowReq(t, h, "192.0.2.1:1000", "/download")
		})
	}
	// 等待两个请求都进入 handler（占满 sem=2）
	for range 2 {
		<-entered
	}
	// 第 3 个请求：sem 满 → 429（并发闸在放行链最前，不消耗其它配额）
	if code := sendAllowReq(t, h, "192.0.2.2:2000", "/download"); code != http.StatusTooManyRequests {
		t.Fatalf("第 3 个并发请求: want 429 (max_concurrent=2), got %d", code)
	}
	// 释放占位 → 后续请求恢复
	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("并发槽内请求: want 200, got %d", code)
		}
	}
	if code := sendAllowReq(t, h, "192.0.2.3:3000", "/download"); code != http.StatusOK {
		t.Fatalf("释放后: want 200, got %d", code)
	}
}

// TestRateLimiter_Middleware_Endpoint429AndPassThrough per-endpoint 规则走 Middleware：
// 配置端点超限 429；未配置路径透传。
func TestRateLimiter_Middleware_Endpoint429AndPassThrough(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Hour, nil)
	rl.UpdateDimensions(map[string]EndpointLimit{
		"/download": {Limit: 1, Window: time.Hour},
	}, EndpointLimit{}, 0)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if code := sendAllowReq(t, h, "192.0.2.1:1", "/download"); code != http.StatusOK {
		t.Fatalf("/download 第 1 次: want 200, got %d", code)
	}
	if code := sendAllowReq(t, h, "192.0.2.1:1", "/download"); code != http.StatusTooManyRequests {
		t.Fatalf("/download 第 2 次: want 429 (endpoint limit=1), got %d", code)
	}
	// 未配置路径透传（endpoint 维度不干预）
	if code := sendAllowReq(t, h, "192.0.2.1:1", "/upload"); code != http.StatusOK {
		t.Fatalf("/upload 无规则: want 200, got %d", code)
	}
}

// TestRateLimiter_Middleware_NoConfigZeroRegression 零回归 golden：
// 无端点规则 + 无并发上限时放行链与现状逐字一致（per-IP → 全局回退语义不变，
// 新维度不干预任何路径）。
func TestRateLimiter_Middleware_NoConfigZeroRegression(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1, time.Hour, nil)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if code := sendAllowReq(t, h, "203.0.113.1:1", "/"); code != http.StatusOK {
		t.Fatalf("第 1 次: want 200, got %d", code)
	}
	if code := sendAllowReq(t, h, "203.0.113.1:1", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("第 2 次（同 IP limit=1）: want 429, got %d", code)
	}
	// 高配额 + 各路径：全部放行（新维度默认关闭）
	rl2 := NewRateLimiter(1000, time.Second, nil)
	h2 := rl2.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, p := range []string{"/download", "/upload", "/api/files", "/"} {
		if code := sendAllowReq(t, h2, "203.0.113.2:2", p); code != http.StatusOK {
			t.Fatalf("path %s: want 200 (新维度默认关), got %d", p, code)
		}
	}
}

// TestRateLimiter_UpdateDimensions_HotReload 热更新生效：
// 初始无规则全放行 → UpdateDimensions 加规则后超限 429 → 清除规则后恢复放行。
func TestRateLimiter_UpdateDimensions_HotReload(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Second, nil)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := range 3 {
		if code := sendAllowReq(t, h, "192.0.2.1:1", "/download"); code != http.StatusOK {
			t.Fatalf("初始 req %d: want 200, got %d", i, code)
		}
	}
	rl.UpdateDimensions(map[string]EndpointLimit{
		"/download": {Limit: 1, Window: time.Hour},
	}, EndpointLimit{}, 0)
	if code := sendAllowReq(t, h, "192.0.2.1:1", "/download"); code != http.StatusOK {
		t.Fatalf("规则后第 1 次: want 200, got %d", code)
	}
	if code := sendAllowReq(t, h, "192.0.2.1:1", "/download"); code != http.StatusTooManyRequests {
		t.Fatalf("规则后第 2 次: want 429 (热更新规则生效), got %d", code)
	}
	// 清除规则 → 恢复放行
	rl.UpdateDimensions(nil, EndpointLimit{}, 0)
	if code := sendAllowReq(t, h, "192.0.2.1:1", "/download"); code != http.StatusOK {
		t.Fatalf("清除规则后: want 200, got %d", code)
	}
}

// TestRateLimiter_ConcurrentAcquireRelease_Race -race 并发 acquire/release 交错，
// 结束断言零泄漏（8 个槽全部可获取）——变异③：ReleaseConcurrent 缺失 → 泄漏红。
func TestRateLimiter_ConcurrentAcquireRelease_Race(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Second, nil)
	rl.UpdateDimensions(nil, EndpointLimit{}, 8)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			for range 200 {
				if rl.AcquireConcurrent() {
					rl.ReleaseConcurrent()
				}
			}
		})
	}
	wg.Wait()
	// 无泄漏：8 个槽应全部可获取
	for range 8 {
		if !rl.AcquireConcurrent() {
			t.Fatalf("槽获取失败（存在泄漏）")
		}
	}
	for range 8 {
		rl.ReleaseConcurrent()
	}
}
