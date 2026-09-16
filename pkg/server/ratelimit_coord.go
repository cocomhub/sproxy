// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// ratelimit_coord.go 是限流「协调后端」抽象：在单实例 RateLimiter（ratelimit.go，
// 内存滑动窗口 + per-IP 令牌桶）之上，提供可选的跨实例共享计数能力。
//
// 背景：sproxy 多实例（mesh 节点）部署时，每实例各自的 RateLimiter 限额独立，
// 攻击者可把请求分散到多实例绕过单实例限额。Coordinator 抽象让限流计数可
// 落到共享后端，多实例共享同一配额。
//
// 默认 local 实现（内存计数）保持既有单实例语义，零回归；coordinated 配置
// 默认关闭，行为与未装配 Coordinator 完全一致。
package server

import (
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// Coordinator 是限流协调后端抽象：Allow 决定 key 是否放行，并消耗 count 配额。
// 实现必须并发安全（Middleware 每请求调用）。
type Coordinator interface {
	// Allow 检查 key 是否放行；放行时消耗 count（默认 1）配额。
	Allow(key string, count int64) bool
}

// localCoordinator 是内存协调后端：每 key 一个滑动窗口计数，语义与
// RateLimiter.Allow 的全局窗口一致，但按 key 隔离。多实例部署下各实例
// 独立计数（无跨进程协调），是 coordinated=false 时的默认后端。
type localCoordinator struct {
	mu     sync.Mutex
	limit  int64
	window time.Duration
	perKey map[string][]time.Time
}

// newLocalCoordinator 创建 local 协调后端（limit<=0 或 window<=0 时按
// RateLimiter 相同默认逻辑归正）。
func newLocalCoordinator(limit int64, window time.Duration) *localCoordinator {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	return &localCoordinator{
		limit:  limit,
		window: window,
		perKey: make(map[string][]time.Time),
	}
}

// newCoordinator 按后端名装配 Coordinator（config 接线入口）。
// backend "local"（默认）→ localCoordinator；"file" → fileCoordinator（需 dir 非空）；
// 其它值 → 错误（由调用方决定回退 local 或报错）。
func newCoordinator(backend string, limit int64, window time.Duration, dir string, logger *slog.Logger) (Coordinator, error) {
	log := slogutil.Default(logger)
	switch backend {
	case "", "local":
		return newLocalCoordinator(limit, window), nil
	case "file":
		if dir == "" {
			return nil, fmt.Errorf("file coordinator requires non-empty storage dir")
		}
		return newFileCoordinator(limit, window, dir, log), nil
	default:
		return nil, fmt.Errorf("unknown rate limit backend %q", backend)
	}
}

// Allow 实现 Coordinator.Allow（内存滑动窗口，按 key 隔离）。
func (c *localCoordinator) Allow(key string, count int64) bool {
	if count <= 0 {
		count = 1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-c.window)
	// 裁剪过期时间戳（保持切片低水位，防止长期运行内存膨胀）。
	entries := c.perKey[key]
	keep := entries[:0]
	for _, ts := range entries {
		if ts.After(cutoff) {
			keep = append(keep, ts)
		}
	}
	c.perKey[key] = keep
	if int64(len(keep)) >= c.limit {
		return false
	}
	for range count {
		c.perKey[key] = append(c.perKey[key], now)
	}
	return true
}
