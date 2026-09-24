// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package leader

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile 对 f 加**非阻塞**排他锁（LockFileEx + LOCKFILE_EXCLUSIVE_LOCK |
// LOCKFILE_FAIL_IMMEDIATELY，等价 Unix flock LOCK_EX|LOCK_NB）。
// 已被其它进程持有 → ERROR_LOCK_VIOLATION → errWouldBlock（TryAcquire 映射为 (false, nil)）。
func lockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errWouldBlock
	}
	return err
}

// unlockFile 释放 f 上的排他锁（幂等；未持锁调用返回 ERROR_NOT_LOCKED 视为成功）。
func unlockFile(f *os.File) error {
	ol := new(windows.Overlapped)
	err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_NOT_LOCKED) {
		return nil
	}
	return err
}
