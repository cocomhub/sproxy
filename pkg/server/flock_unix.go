// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

// flock.go（Unix）提供跨进程文件锁：syscall.Flock 独占锁。
// Windows 无 Flock 等价（见 flock_windows.go），本文件仅在非 Windows 构建。

package server

import (
	"errors"
	"os"
	"syscall"
)

// lockFile 对 f 加独占文件锁（阻塞直到获得）。
// 同一进程内对同一文件的 Flock 可重入（不互斥）——进程内互斥由
// fileCoordinator 的包级锁承担，本锁只解决跨进程互斥。
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockFile 释放 f 上的独占文件锁。
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// errFileLockUnsupported 在 Unix 上永不返回（占位，供 Windows 变体对齐签名）。
var errFileLockUnsupported = errors.New("file lock unsupported on this platform")
