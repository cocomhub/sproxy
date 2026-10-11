// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// tlsTimeoutErr 模拟 "net/http: TLS handshake timeout"（net.Error，可重试）——
// 对应用户实测故障：DownloadLink 的 TLS 握手超时被当致命错误。
type tlsTimeoutErr struct{}

func (tlsTimeoutErr) Error() string   { return "net/http: TLS handshake timeout" }
func (tlsTimeoutErr) Timeout() bool   { return true }
func (tlsTimeoutErr) Temporary() bool { return true }

// wrappedNetErr 是 url.Error 形态的包装（Transport 返回的传输层错误）。
type wrappedNetErr struct{ err error }

func (e wrappedNetErr) Error() string { return "Get \"https://x\": " + e.err.Error() }
func (e wrappedNetErr) Unwrap() error { return e.err }

func TestRetryPolicy_Attempts(t *testing.T) {
	t.Parallel()
	if got := (retryPolicy{}).attempts(); got != defaultRetryCount+1 {
		t.Errorf("默认 attempts = %d, want %d", got, defaultRetryCount+1)
	}
	if got := (retryPolicy{MaxRetries: 5}).attempts(); got != 6 {
		t.Errorf("MaxRetries=5 attempts = %d, want 6", got)
	}
	if got := (retryPolicy{MaxRetries: -1}).attempts(); got != defaultRetryCount+1 {
		t.Errorf("负值按默认: %d", got)
	}
}

func TestRetryPolicy_DelayBackoffAndCap(t *testing.T) {
	t.Parallel()
	p := retryPolicy{MaxRetries: 10, Backoff: 100 * time.Millisecond}
	d1, d2 := p.delay(1), p.delay(2)
	// 抖动上限 1/4：d1 ∈ [100ms,125ms]，d2 ∈ [200ms,250ms]
	if d1 < 100*time.Millisecond || d1 > 125*time.Millisecond {
		t.Errorf("delay(1) = %v, want [100ms,125ms]", d1)
	}
	if d2 < 200*time.Millisecond || d2 > 250*time.Millisecond {
		t.Errorf("delay(2) = %v, want [200ms,250ms]", d2)
	}
	if got := p.delay(20); got > maxRetryBackoff+maxRetryBackoff/retryJitterDiv {
		t.Errorf("delay(20) = %v 未封顶在 %v", got, maxRetryBackoff)
	}
}

func TestRetryableRequestErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"tls 握手超时", tlsTimeoutErr{}, true},
		{"url.Error 包装", wrappedNetErr{err: tlsTimeoutErr{}}, true},
		{"ctx 取消", context.Canceled, false},
		{"ctx 超时", context.DeadlineExceeded, false},
		{"业务错误", errors.New("pikpak api GET /x: HTTP 403: forbidden"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := retryableRequestErr(c.err); got != c.want {
				t.Errorf("retryableRequestErr(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestRetryableStatus(t *testing.T) {
	t.Parallel()
	cases := map[int]bool{
		http.StatusOK:                  false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusTooManyRequests:     true,
		http.StatusInternalServerError: true,
		http.StatusBadGateway:          true,
		http.StatusServiceUnavailable:  true,
	}
	for code, want := range cases {
		if got := retryableStatus(code); got != want {
			t.Errorf("retryableStatus(%d) = %v, want %v", code, got, want)
		}
	}
}

// TestDoWithRetry_TransientThenSucceed 瞬断重试到成功，返回 nil 且尝试次数准确。
func TestDoWithRetry_TransientThenSucceed(t *testing.T) {
	t.Parallel()
	p := retryPolicy{MaxRetries: 3, Backoff: time.Millisecond}
	calls := 0
	err := doWithRetry(context.Background(), p, nil, "GET /x", func() (bool, error) {
		calls++
		if calls <= 2 {
			return true, tlsTimeoutErr{}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// TestDoWithRetry_NonRetryableStopsImmediately 不可重试错误只尝试一次。
func TestDoWithRetry_NonRetryableStopsImmediately(t *testing.T) {
	t.Parallel()
	p := retryPolicy{MaxRetries: 3, Backoff: time.Millisecond}
	calls := 0
	sentinel := errors.New("403 forbidden")
	err := doWithRetry(context.Background(), p, nil, "GET /x", func() (bool, error) {
		calls++
		return false, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// TestDoWithRetry_Exhausted 重试耗尽后返回最后一次错误，尝试次数 = 1+MaxRetries。
func TestDoWithRetry_Exhausted(t *testing.T) {
	t.Parallel()
	p := retryPolicy{MaxRetries: 2, Backoff: time.Millisecond}
	calls := 0
	err := doWithRetry(context.Background(), p, nil, "GET /x", func() (bool, error) {
		calls++
		return true, fmt.Errorf("attempt %d: %w", calls, tlsTimeoutErr{})
	})
	if err == nil {
		t.Fatal("err = nil, want error")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

// TestDoWithRetry_CtxCancelDuringBackoff ctx 在退避期结束 → 立即返回（不再尝试）。
func TestDoWithRetry_CtxCancelDuringBackoff(t *testing.T) {
	t.Parallel()
	p := retryPolicy{MaxRetries: 3, Backoff: 2 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := doWithRetry(ctx, p, nil, "GET /x", func() (bool, error) {
		calls++
		cancel() // 首次失败后取消
		return true, tlsTimeoutErr{}
	})
	if err == nil {
		t.Fatal("err = nil, want ctx error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1（退避期 ctx 取消不再尝试）", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want 包装 context.Canceled", err)
	}
}
