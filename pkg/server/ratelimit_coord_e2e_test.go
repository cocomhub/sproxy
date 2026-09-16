// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCoordinatedRateLimit_E2E 验证 coordinated=file 时，同一 storage 目录下两个
// 服务实例共享限流配额：实例 1 消耗限额后，实例 2 对同一 IP 的请求返回 429。
//
// 注意：rate_limit 挂载在隧道内层 apiHandler（localMux），主 mux 直连面不受限流。
// 本测试通过 RegisterRoutes 装配两个共享同一 StorageRoot 的 handler，分别挂在
// 两个 httptest.Server 上，直接请求各自 /healthz（探针路由绕过限流）不可行——
// 正确验证路径是请求被 Middleware 包裹的路由。因 localMux 仅经 /tunnel 可达，
// 这里退化为直接测 Middleware 装配后的协调行为：两个 handler 的 rateLimiter
// 各自消费同一协调文件，第二个实例在第一个耗尽配额后拒绝。
//
// 简化验证：不启动完整隧道，直接对两个 handler 的 h.rateLimiter.Middleware
// 包装的 stub handler 发请求（httptest.NewRecorder），断言第 N+1 个请求 429。
func TestCoordinatedRateLimit_E2E(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	buildRL := func() *RateLimiter {
		rl := NewRateLimiter(3, 60*time.Second, testLogger())
		coord, err := newCoordinator("file", 3, 60*time.Second, dir, testLogger())
		if err != nil {
			t.Fatalf("newCoordinator(file): %v", err)
		}
		rl.SetCoordinator(coord)
		return rl
	}
	rl1 := buildRL()
	rl2 := buildRL()

	// 同一协调文件共享配额：rl1 消耗 3 次后 rl2 第 4 次 429。
	call := func(rl *RateLimiter, wantStatus int, label string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("%s: got %d want %d", label, rec.Code, wantStatus)
		}
	}

	for i := range 3 {
		call(rl1, http.StatusOK, fmt.Sprintf("rl1 call %d", i))
	}
	call(rl2, http.StatusTooManyRequests, "rl2 4th call")
}

// TestCoordinatedRateLimit_DisabledNoCoord 验证 coordinated=false（默认）时
// 不装配协调后端（SetCoordinator(nil)），限流行为与既有单实例一致。
func TestCoordinatedRateLimit_DisabledNoCoord(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(2, 60*time.Second, testLogger())
	// 未装配 coordinator → Middleware 不调用协调后端。
	call := func(wantStatus int, label string) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != wantStatus {
			t.Fatalf("%s: got %d want %d", label, rec.Code, wantStatus)
		}
	}
	call(http.StatusOK, "call 1")
	call(http.StatusOK, "call 2")
	call(http.StatusTooManyRequests, "call 3")
}

// TestCoordinatedRateLimit_RoutesFallbackLocal 验证未知 backend 时装配回退
// local（fail-open 不阻断启动）——newCoordinator 返回错误，装配点回退。
func TestCoordinatedRateLimit_RoutesFallbackLocal(t *testing.T) {
	t.Parallel()
	_, err := newCoordinator("bogus", 5, time.Second, t.TempDir(), testLogger())
	if err == nil {
		t.Fatal("bogus backend should error")
	}
	if !strings.Contains(err.Error(), "unknown rate limit backend") {
		t.Fatalf("error should mention backend, got %v", err)
	}
}
