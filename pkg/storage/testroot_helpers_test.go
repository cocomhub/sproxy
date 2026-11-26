// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// testroot_helpers_test.go 提供**打开 root 并自动登记关闭**的唯一测试助手。
//
// 为什么必须有它（2026-11-27 CI 实测 flake）：`os.Root` 持有一个**打开的目录句柄**，且
// **没有 finalizer** ⇒ 不 Close 就持有到进程退出。Windows 上这会让 `t.TempDir()` 的
// `RemoveAll` 直接失败：
//
//	TempDir RemoveAll cleanup: unlinkat ...: The process cannot access the file
//	because it is being used by another process.
//
// 实测命中：`TestEncReadCloserSeek_UsesBlockSkip`（#1306 的 CI）。全仓 `OpenRoot(` 的测试
// 调用点有 30+ 处，逐处手写 `t.Cleanup` 容易漏；统一走本助手，新增用例也不会再漏。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// openTestRoot 打开 dir 作为 root，并注册 t.Cleanup 关闭它。
// 打开失败直接 t.Fatalf（测试夹具不需要区分失败原因）。
func openTestRoot(t *testing.T, dir string) *Root {
	t.Helper()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// TestOpenRoot_CallSitesRegisterClose 是**机制性守卫**（源码级）：
// `pkg/storage` 的测试里每个 `OpenRoot(` 调用点都必须有明确的关闭归属——
// 要么走 `openTestRoot(t, dir)`（自动 `t.Cleanup(Close)`），要么紧跟显式 `Close()`
// （`defer X.Close()` / `X.Close()`）。
//
// 为什么需要门禁：`os.Root` 持目录句柄且**没有 finalizer**，漏关在 Windows 上会让
// `t.TempDir()` 的 RemoveAll 失败（实测 flake：TestEncReadCloserSeek_UsesBlockSkip）。
// 靠"记得写 t.Cleanup"是不可靠的，所以把判据做成可执行断言。
func TestOpenRoot_CallSitesRegisterClose(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	const maxWindow = 8 // OpenRoot 之后多少行内必须出现关闭归属
	sites := 0
	for _, f := range files {
		if f == "testroot_helpers_test.go" {
			continue // 助手自身：openTestRoot 内部已注册 Cleanup
		}
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("读 %s: %v", f, rerr)
		}
		lines := strings.Split(string(raw), "\n")
		for i, ln := range lines {
			if !strings.Contains(ln, "OpenRoot(") || strings.Contains(ln, "openTestRoot(") {
				continue
			}
			sites++
			end := min(len(lines), i+1+maxWindow)
			window := strings.Join(lines[i:end], "\n")
			// 关闭归属：t.Cleanup(Close) / defer X.Close() / X.Close()。
			if strings.Contains(window, "t.Cleanup(") && strings.Contains(window, "Close") {
				continue
			}
			if regexp.MustCompile(`\.Close\(\)`).MatchString(window) {
				continue
			}
			t.Errorf("%s:%d 的 OpenRoot 调用未登记关闭（os.Root 无 finalizer，Windows 上会阻塞 TempDir 清理）:\n%s",
				f, i+1, ln)
		}
	}
	if sites == 0 {
		t.Fatal("扫描到 0 个 OpenRoot 调用点（解析器可能失效，门禁会假绿）")
	}
}
