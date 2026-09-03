// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRateLimiter_AllowsWithinLimit(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(3, time.Second, nil)
	for i := range 3 {
		if !rl.Allow() {
			t.Fatalf("call %d should be allowed", i)
		}
	}
}

func TestRateLimiter_RejectsBeyondLimit(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(2, time.Second, nil)
	_ = rl.Allow()
	_ = rl.Allow()
	if rl.Allow() {
		t.Fatal("3rd call should be rejected")
	}
}

func TestRateLimiter_RecoversAfterWindow(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1, 50*time.Millisecond, nil)
	if !rl.Allow() {
		t.Fatal("first call must pass")
	}
	if rl.Allow() {
		t.Fatal("second call must be rejected (still within window)")
	}
	// 轮询等待窗口重置，最多 2s
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rl.Allow() {
			return // 重置成功
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("call after window slide should be allowed")
}

func TestRateLimiter_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1000, time.Second, nil)
	var wg sync.WaitGroup
	var allowed int64
	for range 200 {
		wg.Go(func() {
			if rl.Allow() {
				atomic.AddInt64(&allowed, 1)
			}
		})
	}
	wg.Wait()
	if allowed != 200 {
		t.Fatalf("expected all 200 allowed under high limit, got %d", allowed)
	}
}

func TestRateLimiter_Middleware_Returns429(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1, time.Second, nil)
	called := 0
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.WriteHeader(http.StatusOK)
	}))

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	w1 := httptest.NewRecorder()
	h.ServeHTTP(w1, r1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first req: want 200, got %d", w1.Code)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second req: want 429, got %d", w2.Code)
	}
	if called != 1 {
		t.Fatalf("downstream handler should be called exactly once, got %d", called)
	}
}

// ---- UpdateConfig 热更新 ----

// sendAllowReq 构造一个带远端地址的请求并交给 handler 处理。
func sendAllowReq(t *testing.T, h http.Handler, remote, path string) int {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

// TestRateLimiter_UpdateConfig_TightenLimit 收紧 limit 后，已有窗口内的时间戳立即生效：
// 灌满 limit=10 → 收紧到 2 → 旧时间戳仍在窗口内，下一个请求立即 429；放宽后恢复。
func TestRateLimiter_UpdateConfig_TightenLimit(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, time.Second, nil)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// 灌满 10 个请求
	for i := range 10 {
		if code := sendAllowReq(t, h, "192.0.2.1", "/"); code != http.StatusOK {
			t.Fatalf("prefill req %d: want 200, got %d", i, code)
		}
	}
	rl.UpdateConfig(true, 2, time.Second)
	if code := sendAllowReq(t, h, "192.0.2.1", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("tighten to 2: want 429 (old timestamps still in window), got %d", code)
	}
	rl.UpdateConfig(true, 100, time.Second)
	if code := sendAllowReq(t, h, "192.0.2.2", "/"); code != http.StatusOK {
		t.Fatalf("relax to 100: want 200, got %d", code)
	}
}

// TestRateLimiter_UpdateConfig_DisabledShortCircuits enabled=false 时 Middleware
// 短路放行，不再限流；重新 enabled=true 后恢复限流。
func TestRateLimiter_UpdateConfig_DisabledShortCircuits(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1, time.Second, nil)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rl.UpdateConfig(false, 1, time.Second)
	// 即使 limit=1 且已消费，禁用后仍全部放行
	_ = sendAllowReq(t, h, "192.0.2.1", "/")
	_ = sendAllowReq(t, h, "192.0.2.1", "/")
	_ = sendAllowReq(t, h, "192.0.2.1", "/")
	if code := sendAllowReq(t, h, "192.0.2.1", "/"); code != http.StatusOK {
		t.Fatalf("disabled: want 200, got %d", code)
	}
	rl.UpdateConfig(true, 1, time.Second)
	_ = sendAllowReq(t, h, "192.0.2.2", "/")
	if code := sendAllowReq(t, h, "192.0.2.2", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("re-enabled: want 429, got %d", code)
	}
}

// TestRateLimiter_UpdateConfig_TimestampsKeepEnabled 切换 enabled 不重建 handler、不清空
// 时间戳：禁用期间旧窗口按新 window 自然过期，重新置 true 后继续基于同一实例限流。
func TestRateLimiter_UpdateConfig_TimestampsKeepEnabled(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(1, 40*time.Millisecond, nil)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if code := sendAllowReq(t, h, "192.0.2.1", "/"); code != http.StatusOK {
		t.Fatalf("first: want 200, got %d", code)
	}
	if code := sendAllowReq(t, h, "192.0.2.1", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("second: want 429 (limit=1 同 IP 已超限), got %d", code)
	}
	// 禁用 + 短窗口：enabled=false 短路放行（旧时间戳仍留在窗口内，不因禁用被清空）。
	rl.UpdateConfig(false, 1, 40*time.Millisecond)
	_ = sendAllowReq(t, h, "192.0.2.9", "/")
	_ = sendAllowReq(t, h, "192.0.2.9", "/")
	if code := sendAllowReq(t, h, "192.0.2.9", "/"); code != http.StatusOK {
		t.Fatalf("disabled: want 200, got %d", code)
	}
	// 重新启用，保持短窗口 → 窗口继续滑动，最终放行（timestamp 未被 UpdateConfig 清空）。
	rl.UpdateConfig(true, 1, 40*time.Millisecond)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if code := sendAllowReq(t, h, "192.0.2.3", "/"); code == http.StatusOK {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("re-enabled after window slide should allow requests")
}

// TestRateLimiter_UpdateConfig_WindowChange 修改 window 后按新窗口判断：
// 长窗口 → 短窗口，旧窗口内的时间戳立即超限。
func TestRateLimiter_UpdateConfig_WindowChange(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(10, time.Second, nil)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// 灌满 10 个
	for i := range 10 {
		if code := sendAllowReq(t, h, "192.0.2.1", "/"); code != http.StatusOK {
			t.Fatalf("prefill req %d: want 200, got %d", i, code)
		}
	}
	// 缩短窗口：旧时间戳远超新窗口 → 全部过期，放行
	rl.UpdateConfig(true, 10, 10*time.Millisecond)
	if code := sendAllowReq(t, h, "192.0.2.2", "/"); code != http.StatusOK {
		t.Fatalf("shrink window: want 200 (old timestamps expired), got %d", code)
	}
	// 立即再收紧窗口会因两个新时间戳断超限
	rl.UpdateConfig(true, 2, time.Second)
	_ = sendAllowReq(t, h, "192.0.2.2", "/")
	_ = sendAllowReq(t, h, "192.0.2.2", "/")
	if code := sendAllowReq(t, h, "192.0.2.2", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("tighten after shrink: want 429, got %d", code)
	}
}

// TestRateLimiter_UpdateConfig_ConcurrentSafe 并发更新与放行交错，验证 -race 下无数据竞争。
func TestRateLimiter_UpdateConfig_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter(5, time.Second, nil)
	h := rl.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 50 {
				rl.UpdateConfig(i%2 == 0, 1+i%4, time.Duration(100+i)*time.Millisecond)
				// 并发读路径：禁用时走 AllowIP 短路（不再触发 MIDDLEWARE 429），
				// Allow 与 UpdateConfig 交错（覆盖无锁读 limit 的竞态）。
				_ = rl.Allow()
				_ = sendAllowReq(t, h, "192.0.2.9", "/")
			}
		})
	}
	wg.Wait()
}
