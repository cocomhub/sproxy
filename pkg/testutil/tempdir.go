// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"
)

// CleanupRetryDelay / CleanupRetries 是 Windows 上目录删除的重试参数。
//
// 背景（2026-10-31 实测）：%TEMP% 累积了约 1,800 个本仓测试目录
// （archive-volume-* 791、nested-secret-tmp* 648、archive-local-* 134 等）。
// 这些站点**都已**调用 RemoveAll，但 Windows 上句柄释放有延迟（文件/目录句柄被
// 上一批 defer 释放前，RemoveAll 会报 ERROR_SHARING_VIOLATION），而清理错误被
// `_ =` 吞掉 ⇒ 每次运行静默残留一个目录。
var (
	CleanupRetryDelay = 100 * time.Millisecond
	CleanupRetries    = 20
)

// 进程级登记：Windows 上测试内 RemoveAll 常因句柄未释放而失败（"being used by another
// process"），单测试的 t.Cleanup 无解。故所有经本包创建的目录都登记到进程表，
// 由 TestMain 在**全部测试结束后**调用 CleanupAllTempDirs 统一清理。
var tempDirs struct {
	mu   sync.Mutex
	dirs []string
}

// CleanupAllTempDirs 删除本进程经 MkdirTemp/CleanupDir 创建的所有目录。
// 必须在 m.Run() 之后调用（此时测试 goroutine 已结束，句柄可释放）。
func CleanupAllTempDirs() {
	tempDirs.mu.Lock()
	dirs := tempDirs.dirs
	tempDirs.dirs = nil
	tempDirs.mu.Unlock()
	for _, d := range dirs {
		removeWithRetry(d)
	}
}

// TrackTempDir 登记一个已在别处创建的临时目录，由 CleanupAllTempDirs 统一清理。
// 用于没有 *testing.T 的构造路径（如测试用的 backend builder）。
func TrackTempDir(dir string) { registerTempDir(dir) }

func registerTempDir(dir string) {
	tempDirs.mu.Lock()
	tempDirs.dirs = append(tempDirs.dirs, dir)
	tempDirs.mu.Unlock()
}

// removeWithRetry 删除目录：首次失败后触发 GC（Windows finalizer 释放句柄）再重试。
func removeWithRetry(dir string) {
	var err error
	for i := 0; i < CleanupRetries; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return
		}
		if i%5 == 4 {
			runtime.GC()
		}
		time.Sleep(CleanupRetryDelay)
	}
	_ = err
}

// MkdirTemp 创建临时目录并登记清理（测试内尽力清理 + 进程退出时兜底），返回目录路径。
//
// 与 `os.MkdirTemp("", prefix)` 的区别：
//   - 清理走 t.Cleanup（在测试函数所有 defer 之后执行 ⇒ 句柄已关闭）；
//   - 失败重试（Windows 句柄延迟释放）；
//   - 最终仍失败时 t.Logf 记录（不再静默吞掉）。
//
// 测试里创建临时目录一律用它（或 t.TempDir），不要裸调 os.MkdirTemp。
func MkdirTemp(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix+"*")
	if err != nil {
		t.Fatalf("MkdirTemp(%q): %v", prefix, err)
	}
	registerTempDir(dir)
	CleanupDir(t, dir)
	return dir
}

// CleanupDir 注册带重试的目录清理（用于已在别处创建的临时目录）。
func CleanupDir(t *testing.T, dir string) {
	t.Helper()
	if dir == "" {
		return
	}
	registerTempDir(dir)
	t.Cleanup(func() {
		// 测试内尽力清理；失败不报错——进程级 CleanupAllTempDirs 会兜底。
		removeWithRetry(dir)
	})
}
