// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package credrotate 提供常驻 sclient 进程的「运行中凭据自动轮换」工具：
// 定时调 FileClient.RenewAccessKey（新 SK 立即热替换到签名器——RenewAccessKey
// 已支持同进程热更新，常驻进程无需重启），供 http-proxy / socks / mesh node /
// p2p / relay / mesh up 等所有常驻场景复用，避免各 main 包重复实现定时逻辑。
package credrotate

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// Options 是自动轮换配置。
type Options struct {
	// Interval 是轮换间隔（0 = 关闭）。
	Interval time.Duration
	// Logger 是会话日志（nil 用 slog.Default()）。
	Logger *slog.Logger
}

// Start 启动凭据自动轮换 goroutine：
//   - svc 为 nil 或 interval <= 0 → 不启动，返回 (nil, false)；
//   - 否则每 interval 调一次 svc.RenewAccessKey()，新 SK 热替换到签名器
//     （服务端多 SK 共存，旧 SK 宽限期内仍可用——幂等安全），失败记 Warn 下次重试。
//
// 返回 stop 函数（关闭 ticker 与 goroutine；ctx 取消同样停止）与 started 标记。
func Start(ctx context.Context, svc *client.FileClient, opts Options) (stop func(), started bool) {
	if svc == nil || opts.Interval <= 0 {
		return nil, false
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	stopCh := make(chan struct{})
	var stopOnce sync.Once

	go func() {
		ticker := time.NewTicker(opts.Interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-ticker.C:
				if _, rerr := svc.RenewAccessKey(ctx); rerr != nil {
					logger.Warn("凭据自动轮换失败（下次重试）", "error", rerr)
				} else {
					logger.Info("凭据已自动轮换（新 SK 热替换生效）")
				}
			}
		}
	}()
	return func() { stopOnce.Do(func() { close(stopCh) }) }, true
}
