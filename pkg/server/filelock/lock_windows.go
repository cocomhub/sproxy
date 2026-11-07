// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

// lock_windows.go（Windows）提供跨进程文件锁的降级实现：Windows 无 syscall.Flock
// 等价（LockFileEx 需 CGO），本文件把 Lock/Unlock 退化为空操作——
// 跨进程互斥在 Windows 上依赖调用方的进程内互斥 + 文件系统原子性尽力而为，
// 被协调的文件仍共享，并发写由 O_APPEND/Truncate 序列的原子性兜底（不保证精确）。
// 语义降级已在 package 注释与各调用方注释中说明。

package filelock

import (
	"os"
)

// Lock 在 Windows 上为空操作（无跨进程文件锁；进程内互斥由调用方承担）。
func Lock(f *os.File) error { return nil }

// Unlock 在 Windows 上为空操作。
func Unlock(f *os.File) error { return nil }
