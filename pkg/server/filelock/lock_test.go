// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package filelock

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestLock_Unlock_Relock 验证同一文件的加锁 → 解锁 → 再加锁全链路成功
// （Unix flock 语义；Windows 上 Lock/Unlock 是空操作，同样恒成功）。
func TestLock_Unlock_Relock(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "filelock-relock-")
	path := filepath.Join(dir, "count")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("打开计数文件: %v", err)
	}
	defer f.Close()

	if err := Lock(f); err != nil {
		t.Fatalf("首次加锁: %v", err)
	}
	if err := Unlock(f); err != nil {
		t.Fatalf("解锁: %v", err)
	}
	if err := Lock(f); err != nil {
		t.Fatalf("解锁后再次加锁: %v", err)
	}
	if err := Unlock(f); err != nil {
		t.Fatalf("再次解锁: %v", err)
	}
}

// TestLock_ClosedFileErrorPath 验证失败路径：Unix 上对已关闭文件加锁返回错误
// （fd 无效 ⇒ flock EBADF）；Windows 的降级实现是空操作，不触任何系统调用，
// 故恒返回 nil——平台差异在此显式断言，避免误以为 Windows 也具备跨进程互斥。
func TestLock_ClosedFileErrorPath(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "filelock-closed-")
	path := filepath.Join(dir, "count")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatalf("打开计数文件: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭计数文件: %v", err)
	}

	lockErr := Lock(f)
	if runtime.GOOS == "windows" {
		if lockErr != nil {
			t.Fatalf("Windows 降级实现应为空操作返回 nil, got %v", lockErr)
		}
		return
	}
	if lockErr == nil {
		t.Fatal("Unix 上对已关闭文件加锁必须返回错误（fd 无效）")
	}
}
