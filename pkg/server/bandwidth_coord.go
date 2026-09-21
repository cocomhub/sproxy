// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// bandwidth_coord.go 是带宽限速的跨实例协调后端（rate_limit.bandwidth.coord_backend=file）。
//
// 背景：单实例带宽限速用进程内 token 桶（pkg/files.TokenBucket，per-owner）。多实例
// （mesh 节点）部署时各实例独立桶，per_owner_bps 限额被 N 实例摊薄（每实例各限各的，
// 总量 N×bps）。coord_backend=file 让字节配额落到共享后端：多实例共享同一 per_owner
// 字节预算。
//
// 语义（裁决：等待不拒绝）：byteFileCoordinator 按 owner 记「固定窗口字节预算」——
// 窗口（默认 1s）内累计字节 ≤ per_owner_bps 则放行（Consume 成功累加），超限返回
// false（领域层 TokenBucket.waitCoord 轮询等待窗口刷新，有界 5s 超时按未限速继续）。
// 与单实例 token 桶行为一致（慢速传输），只是配额跨实例共享。
//
// 实现：每 key 一个原子计数文件 <dir>/bandwidth/<sanitized-owner>，首行窗口起点
// UnixNano，次行窗口内累计字节。文件锁（flock）read-modify-write 保证跨进程互斥；
// Windows 降级为进程内互斥 + 文件系统原子性（尽力而为，语义在 Linux 验证）。

import (
	"bufio"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// byteFileCoordinator 是按 owner 字节预算的跨进程协调后端。
// 实现 files.QuotaCoordinator（Consume 语义）。
type byteFileCoordinator struct {
	mu     sync.Mutex // 进程内互斥（Windows 降级路径 + 本进程内串行化）
	quota  int64      // 窗口内字节上限（= per_owner_bps）
	window time.Duration
	dir    string
	logger *slog.Logger
}

// newByteFileCoordinator 创建带宽字节预算协调后端。quota<=0 → 不限额（Consume 恒放行，
// 行为退化为纯内存 token 桶）；dir 为计数文件根目录（storage 根，bandwidth/ 子目录），
// 必须非空。
func newByteFileCoordinator(quota int64, window time.Duration, dir string, logger *slog.Logger) *byteFileCoordinator {
	if quota <= 0 {
		quota = 1 << 60 // 近似不限额（1 EiB）
	}
	if window <= 0 {
		window = time.Second
	}
	return &byteFileCoordinator{
		quota:  quota,
		window: window,
		dir:    dir,
		logger: slogutil.Default(logger),
	}
}

// Consume 尝试在协调后端消耗 n 字节配额：窗口内累计 + n ≤ quota 则累加并返回 true；
// 超限返回 false（调用方等待重试）。key 为 owner（经 sanitizeKey 归一为安全文件名）。
func (c *byteFileCoordinator) Consume(key string, n int64) bool {
	if n <= 0 {
		return true
	}
	c.mu.Lock() // 进程内串行化（Windows 降级路径 + 本进程内原子）
	defer c.mu.Unlock()

	sub := filepath.Join(c.dir, "bandwidth")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		c.logger.Warn("bandwidth coord dir create failed, allowing", "key", key, "error", err)
		return true
	}
	path := filepath.Join(sub, sanitizeKey(key))
	now := time.Now()
	start := now.UnixNano()

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		c.logger.Warn("bandwidth coord file open failed, allowing", "key", key, "error", err)
		return true
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		c.logger.Warn("bandwidth coord file lock failed, allowing", "key", key, "error", err)
		return true
	}
	defer func() { _ = unlockFile(f) }()

	// 读窗口起点与累计字节（文件两行：首行起点，次行累计）。
	var windowStart int64
	var used int64
	sc := bufio.NewScanner(f)
	if sc.Scan() {
		windowStart, _ = strconv.ParseInt(strings.TrimSpace(sc.Text()), 10, 64)
	}
	if sc.Scan() {
		used, _ = strconv.ParseInt(strings.TrimSpace(sc.Text()), 10, 64)
	}

	// 窗口过期：距起点超 window → 新窗口（累计清零，windowStart 由回写 start 覆盖）。
	if windowStart > 0 && now.Sub(time.Unix(0, windowStart)) >= c.window {
		used = 0
	}

	if used+n > c.quota {
		return false
	}

	// 回写：窗口起点行 + 新累计字节。
	if err := f.Truncate(0); err != nil {
		c.logger.Warn("bandwidth coord file truncate failed, allowing", "key", key, "error", err)
		return true
	}
	if _, err := f.Seek(0, 0); err != nil {
		c.logger.Warn("bandwidth coord file seek failed, allowing", "key", key, "error", err)
		return true
	}
	if _, err := fmt.Fprintf(f, "%d\n%d\n", start, used+n); err != nil {
		c.logger.Warn("bandwidth coord file write failed, allowing", "key", key, "error", err)
		return true
	}
	if err := f.Sync(); err != nil {
		c.logger.Warn("bandwidth coord file sync failed, allowing", "key", key, "error", err)
		return true
	}
	return true
}
