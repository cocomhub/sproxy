// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package leader

import (
	"errors"
	"os"
	"syscall"
)

// lockFile 对 f 加**非阻塞**排他锁（flock LOCK_EX|LOCK_NB）。
// 已被其它进程持有 → errWouldBlock（TryAcquire 映射为 (false, nil)）。
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errWouldBlock
	}
	return err
}

// unlockFile 释放 f 上的排他锁（幂等；未持锁调用无害）。
func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
