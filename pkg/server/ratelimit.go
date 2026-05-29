// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"sort"
	"sync"
	"time"
)

// RateLimiter implements a sliding-window rate limiter using only the stdlib.
// Thread-safe via sync.Mutex.
type RateLimiter struct {
	mu         sync.Mutex
	limit      int
	window     time.Duration
	timestamps []time.Time
}

// NewRateLimiter creates a RateLimiter allowing up to `limit` requests
// per sliding `window` duration.
func NewRateLimiter(limit int, window time.Duration) *RateLimiter {
	return &RateLimiter{
		limit:  limit,
		window: window,
	}
}

// Allow reports whether the current request is within the rate limit.
// It cleans expired entries, checks the count, and records the new timestamp.
func (rl *RateLimiter) Allow() bool {
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
// When the limit is exceeded, it responds with 429 Too Many Requests.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !rl.Allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
