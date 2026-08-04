// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"
)

// RateLimiter implements a sliding-window rate limiter using only the stdlib.
// Thread-safe via sync.Mutex.
//
// 当前实现为全局限流（全局单实例），不区分客户端 IP。
// 单个客户端可耗尽全部配额，影响其他客户端。
// 如需要扩展为 IP 级别限流，可为每个 RemoteAddr 创建独立的 RateLimiter
// 实例（需定期清理过期条目），或使用 per-IP 计数器 + 滑动窗口方案。
type RateLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	timestamps []time.Time
	logger     *slog.Logger
}

// NewRateLimiter creates a RateLimiter allowing up to `limit` requests
// per sliding `window` duration.
func NewRateLimiter(limit int, window time.Duration, logger *slog.Logger) *RateLimiter {
	if window <= 0 {
		window = time.Second
	}
	return &RateLimiter{
		limit:  limit,
		window: window,
		logger: defaultLogger(logger),
	}
}

// Allow reports whether the current request is within the rate limit.
// It cleans expired entries, checks the count, and records the new timestamp.
func (rl *RateLimiter) Allow() bool {
	if rl.limit <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Binary search for first non-expired entry
	idx := sort.Search(len(rl.timestamps), func(i int) bool {
		return rl.timestamps[i].After(cutoff)
	})
	rl.timestamps = rl.timestamps[idx:]

	if len(rl.timestamps) >= rl.limit {
		return false
	}

	rl.timestamps = append(rl.timestamps, now)
	return true
}

// Middleware wraps an http.Handler with rate limiting.
// When the limit is exceeded, it responds with 429 Too Many Requests (JSON).
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow() {
			rl.logger.Warn("rate limit exceeded", "remote_addr", r.RemoteAddr, "path", r.URL.Path)
			sendJSONResponse(w, map[string]string{"error": "rate limit exceeded"}, http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
