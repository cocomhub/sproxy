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
	time.Sleep(80 * time.Millisecond)
	if !rl.Allow() {
		t.Fatal("call after window slide should be allowed")
	}
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
