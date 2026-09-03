// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

// RateLimiter implements a sliding-window rate limiter using only the stdlib.
// Thread-safe via sync.Mutex.
//
// 当前实现为全局限流（全局单实例）+ 每 IP 令牌桶限流。
// 每个客户端 IP 获得 limit/10 的令牌桶配额，先检查 per-IP 令牌桶，
// 配额耗尽后回退到全局滑动窗口。
type RateLimiter struct {
	mu         sync.Mutex
	enabled    bool
	limit      int
	window     time.Duration
	timestamps []time.Time
	logger     *slog.Logger

	// Per-IP token bucket
	ipBuckets   sync.Map
	ipQuota     float64
	lastCleanup time.Time
}

// ipBucket 表示单个 IP 的令牌桶状态。
type ipBucket struct {
	tokens    float64
	lastCheck time.Time
}

// maxTimestampsCap 是时间戳队列的最大容量，防止内存泄漏。
const maxTimestampsCap = 100000

// NewRateLimiter creates a RateLimiter allowing up to `limit` requests
// per sliding `window` duration.
func NewRateLimiter(limit int, window time.Duration, logger *slog.Logger) *RateLimiter {
	log := defaultLogger(logger)
	if limit <= 0 {
		log.Warn("rate limiter created with limit <= 0, defaulting to 5")
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	ipQuota := float64(limit) / 10.0
	if ipQuota < 1 {
		ipQuota = 1
	}
	return &RateLimiter{
		enabled:     true,
		limit:       limit,
		window:      window,
		logger:      log,
		ipQuota:     ipQuota,
		lastCleanup: time.Now(),
	}
}

// UpdateConfig 热更新限流参数（PUT /api/config 接线）。
// 复用现有 mu 与实例，不重建 handler 链（xfer LocalHandler 已持有构造期引用），
// 不清空 timestamps（旧窗口按新 window 自然过期）。enabled=false 时 Middleware 短路放行。
// 与 NewRateLimiter 相同的默认逻辑：limit<=0 归 5、window<=0 归 1s。
func (rl *RateLimiter) UpdateConfig(enabled bool, limit int, window time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if limit <= 0 {
		rl.logger.Warn("rate limiter update: limit <= 0, defaulting to 5")
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	ipQuota := float64(limit) / 10.0
	if ipQuota < 1 {
		ipQuota = 1
	}
	rl.enabled = enabled
	rl.limit = limit
	rl.window = window
	rl.ipQuota = ipQuota
}

// Allow reports whether the current request is within the global rate limit.
// 不使用 per-IP 限流。
func (rl *RateLimiter) Allow() bool {
	if rl.limit <= 0 {
		return false
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.allowGlobalLocked()
}

// allowGlobalLocked 执行全局滑动窗口检查（调用者必须已持有 rl.mu）。
func (rl *RateLimiter) allowGlobalLocked() bool {
	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Binary search for first non-expired entry
	idx := sort.Search(len(rl.timestamps), func(i int) bool {
		return rl.timestamps[i].After(cutoff)
	})
	rl.timestamps = rl.timestamps[idx:]

	// 限制切片容量上限，防止异常流量导致内存泄漏
	if cap(rl.timestamps) > maxTimestampsCap {
		trimmed := make([]time.Time, len(rl.timestamps))
		copy(trimmed, rl.timestamps)
		rl.timestamps = trimmed
	}

	if len(rl.timestamps) >= rl.limit {
		return false
	}

	rl.timestamps = append(rl.timestamps, now)
	return true
}

// AllowIP 检查请求是否在限流范围内，优先使用 per-IP 令牌桶，
// 配额耗尽后回退到全局滑动窗口。
func (rl *RateLimiter) AllowIP(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.allowIPLocked(ip)
}

// cleanupIPBuckets 清理超过 2 个窗口未使用的 IP 桶条目。
func (rl *RateLimiter) cleanupIPBuckets() {
	cutoff := time.Now().Add(-rl.window * 2)
	rl.ipBuckets.Range(func(key, value any) bool {
		bucket := value.(*ipBucket) //nolint:errcheck // 类型断言安全：我们只存储 *ipBucket
		if bucket.lastCheck.Before(cutoff) {
			rl.ipBuckets.Delete(key)
		}
		return true
	})
}

// Middleware wraps an http.Handler with rate limiting.
// 使用 per-IP 令牌桶 + 全局限流。
// When the limit is exceeded, it responds with 429 Too Many Requests (JSON).
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 热更新 enabled=false 时短路放行（不重建 handler 链）。
		// 读 enabled 与 AllowIP 共用 mu（AllowIP 持锁后重读），避免数据竞争。
		rl.mu.Lock()
		enabled := rl.enabled
		ip := r.RemoteAddr
		allowed := enabled && rl.allowIPLocked(ip)
		rl.mu.Unlock()
		if !allowed {
			if enabled {
				rl.logger.Warn("rate limit exceeded", "remote_addr", ip, "path", r.URL.Path)
				sendJSONResponse(w, map[string]string{"error": "rate limit exceeded"}, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allowIPLocked 实现 AllowIP 的加锁体（调用者必须已持有 rl.mu）。
// 每请求首查 per-IP 令牌桶，配额不足回退全局滑动窗口。
func (rl *RateLimiter) allowIPLocked(ip string) bool {
	if rl.limit <= 0 {
		return false
	}

	// 定期清理过期 IP 桶条目
	if time.Since(rl.lastCleanup) > rl.window*2 {
		rl.cleanupIPBuckets()
		rl.lastCleanup = time.Now()
	}

	// Per-IP 令牌桶检查
	if ip != "" && rl.ipQuota > 0 {
		val, _ := rl.ipBuckets.LoadOrStore(ip, &ipBucket{
			tokens:    rl.ipQuota,
			lastCheck: time.Now(),
		})
		bucket := val.(*ipBucket) //nolint:errcheck // 类型断言安全：我们只存储 *ipBucket

		now := time.Now()
		elapsed := now.Sub(bucket.lastCheck).Seconds()
		rate := rl.ipQuota / rl.window.Seconds()
		bucket.tokens = math.Min(rl.ipQuota, bucket.tokens+elapsed*rate)
		bucket.lastCheck = now

		if bucket.tokens >= 1 {
			bucket.tokens--
			// 记录到全局窗口，确保总配额准确
			rl.timestamps = append(rl.timestamps, now)
			return true
		}
	}

	// 回退到全局滑动窗口
	return rl.allowGlobalLocked()
}
