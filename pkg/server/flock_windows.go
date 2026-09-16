// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

// flock.go（Windows）提供跨进程文件锁的降级实现：Windows 无 syscall.Flock
// 等价（LockFileEx 需 CGO），本文件把 lockFile/unlockFile 退化为空操作——
// 跨进程互斥在 Windows 上依赖「包级进程内互斥 + 文件系统原子性」尽力而为，
// 计数文件仍共享，并发写由 O_APPEND/Truncate 序列的原子性兜底（不保证精确）。
// 语义降级已在本文件与 ratelimit_coord.go 注释中说明。

package server

import (
	"os"
)

// lockFile 在 Windows 上为空操作（无跨进程文件锁；进程内互斥由包级锁承担）。
func lockFile(f *os.File) error { return nil }

// unlockFile 在 Windows 上为空操作。
func unlockFile(f *os.File) error { return nil }
