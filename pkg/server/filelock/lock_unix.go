// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

// lock_unix.go（Unix）提供跨进程文件锁：syscall.Flock 独占锁。
// Windows 无 Flock 等价（见 lock_windows.go），本文件仅在非 Windows 构建。

package filelock

import (
	"os"
	"syscall"
)

// Lock 对 f 加独占文件锁（阻塞直到获得）。
// 同一进程内对同一文件的 Flock 可重入（不互斥）——进程内互斥由
// 调用方自己的锁承担，本锁只解决跨进程互斥。
func Lock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// Unlock 释放 f 上的独占文件锁。
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
