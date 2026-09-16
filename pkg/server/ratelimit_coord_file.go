// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// ratelimit_coord_file.go 是基于 storage 目录下原子计数文件的跨进程协调后端。
// 每 key 对应 <dir>/ratelimit/<sanitized-key> 计数文件：
//   - 加文件锁（flock 独占）后 read-modify-write 计数，保证跨进程互斥；
//   - 窗口过期按文件头记录的窗口起点判断（首行 UnixNano），避免仅凭 mtime 的
//     时钟偏差问题；
//   - 文件内容：首行窗口起点，后续每请求一行时间戳（可读、可调试）。
//
// Windows 注意：syscall.Flock 无等价（见 flock_windows.go），跨进程互斥在
// Windows 上降级为进程内互斥 + 文件系统原子性（尽力而为，不保证精确）；
// 跨进程协调语义在 Linux 上验证（测试 TestFileCoordinator_CrossProcess）。
package server

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// fileCoordinator 是文件锁共享计数协调后端。
type fileCoordinator struct {
	mu     sync.Mutex // 进程内互斥（Windows 降级路径 + 本进程内串行化）
	limit  int64
	window time.Duration
	dir    string
	logger *slog.Logger
}

// newFileCoordinator 创建 file 协调后端（limit<=0 或 window<=0 时按
// RateLimiter 相同默认逻辑归正）。dir 为计数文件根目录（storage 下的
// ratelimit/ 子目录），必须非空。
func newFileCoordinator(limit int64, window time.Duration, dir string, logger *slog.Logger) *fileCoordinator {
	if limit <= 0 {
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	return &fileCoordinator{
		limit:  limit,
		window: window,
		dir:    dir,
		logger: logger,
	}
}

// Allow 实现 Coordinator.Allow（文件锁 + read-modify-write 原子计数）。
func (c *fileCoordinator) Allow(key string, count int64) bool {
	if count <= 0 {
		count = 1
	}
	c.mu.Lock() // 进程内串行化（Windows 降级路径依赖它保证本进程内原子）
	defer c.mu.Unlock()

	sub := filepath.Join(c.dir, "ratelimit")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		c.logger.Warn("rate limit dir create failed, allowing", "key", key, "error", err)
		return true
	}
	path := filepath.Join(sub, sanitizeKey(key))
	now := time.Now()
	start := now.UnixNano()

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		c.logger.Warn("rate limit file open failed, allowing", "key", key, "error", err)
		return true
	}
	defer f.Close()

	if err := lockFile(f); err != nil {
		c.logger.Warn("rate limit file lock failed, allowing", "key", key, "error", err)
		return true
	}
	defer unlockFile(f)

	// 读窗口起点与计数。
	var windowStart int64
	var countInFile int64
	data := make([]byte, 64*1024)
	n, _ := f.Read(data)
	if n > 0 {
		lineEnd := 0
		for i, b := range data[:n] {
			if b == '\n' {
				lineEnd = i
				break
			}
		}
		if lineEnd > 0 {
			_, _ = fmt.Sscanf(string(data[:lineEnd]), "%d", &windowStart)
		}
		// 计数 = 窗口起点行之后的换行数（每请求一行）。
		for i := lineEnd + 1; i < n; i++ {
			if data[i] == '\n' {
				countInFile++
			}
		}
	}

	// 窗口过期：距窗口起点超过 window → 新窗口（计数清零）。
	if windowStart > 0 && now.Sub(time.Unix(0, windowStart)) >= c.window {
		windowStart = 0
		countInFile = 0
	}

	if countInFile >= c.limit {
		return false
	}

	// 回写：窗口起点行 + 保留历史计数行 + 追加本次 count 行。
	// 注意：不能只写本次 count 行（会把历史行 Truncate 掉导致计数丢失）。
	if err := f.Truncate(0); err != nil {
		c.logger.Warn("rate limit file truncate failed, allowing", "key", key, "error", err)
		return true
	}
	if _, err := f.Seek(0, 0); err != nil {
		c.logger.Warn("rate limit file seek failed, allowing", "key", key, "error", err)
		return true
	}
	if _, err := f.WriteString(fmt.Sprintf("%d\n", start)); err != nil {
		c.logger.Warn("rate limit file write failed, allowing", "key", key, "error", err)
		return true
	}
	// 先补齐历史行（之前窗口内已放行的请求），再追加本次。
	for i := int64(0); i < countInFile; i++ {
		if _, err := f.WriteString(fmt.Sprintf("%d\n", start)); err != nil {
			c.logger.Warn("rate limit file history write failed, allowing", "key", key, "error", err)
			return true
		}
	}
	for range count {
		if _, err := f.WriteString(fmt.Sprintf("%d\n", now.UnixNano())); err != nil {
			c.logger.Warn("rate limit file append failed, allowing", "key", key, "error", err)
			return true
		}
	}
	if err := f.Sync(); err != nil {
		c.logger.Warn("rate limit file sync failed, allowing", "key", key, "error", err)
		return true
	}
	return true
}

// sanitizeKey 把限流 key 归一到安全文件名字段：只保留字母数字与 -_.，
// 其余替换为下划线，防路径穿越（key 可能来自 RemoteAddr/IP/路径）。
func sanitizeKey(key string) string {
	out := make([]byte, 0, len(key))
	for i, b := range []byte(key) {
		// 前导点（'.'/'..' 起始）在部分文件系统有隐藏/穿越语义，替换为下划线。
		if i == 0 && b == '.' {
			out = append(out, '_')
			continue
		}
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9',
			b == '-', b == '_', b == '.':
			out = append(out, b)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}
