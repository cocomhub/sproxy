// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// store_close_gate_test.go 是**机制性守卫**：confirm `ChecksumStore` 带 debounce 落盘
// 协程（`savemerger`），测试里创建它之后必须登记关闭，否则协程会在测试结束后仍写一次，
// 与 `t.TempDir()` 的 `RemoveAll` 撞车。
//
// 实测（2026-11-27 CI Test(windows)）：
//
//	TempDir RemoveAll cleanup: unlinkat ...\alice\meta: The directory is not empty.
//
// 判据：本包内每个 `NewChecksumStore(` 调用点，其后 6 行内必须出现 `Close()`（或
// `t.Cleanup(...Close...)`）。含**下限护栏**（扫到 0 个即失败）防止解析器失效假绿。
//
// 其它包（`pkg/files` / `pkg/server` / `pkg/cloud` …）的调用点已按同一纪律补 `t.Cleanup`；
// 若要把门禁扩到全仓，把 moduleRoot 扫描面放宽即可（本包先守住自己）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNewChecksumStore_CallSitesRegisterClose(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("Glob: %v", err)
	}
	const maxWindow = 6
	closeRe := regexp.MustCompile(`\.Close\(\)`)
	sites := 0
	for _, f := range files {
		if f == "store_close_gate_test.go" {
			continue // 门禁自身的匹配字符串不是真实调用
		}
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("读 %s: %v", f, rerr)
		}
		lines := strings.Split(string(raw), "\n")
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if !strings.Contains(ln, "NewChecksumStore(") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			sites++
			end := min(len(lines), i+1+maxWindow)
			window := strings.Join(lines[i:end], "\n")
			if closeRe.MatchString(window) {
				continue
			}
			t.Errorf("%s:%d 的 NewChecksumStore 调用未登记关闭（debounce 协程会在测试后写盘，Windows 上阻塞 TempDir 清理）:\n%s",
				f, i+1, ln)
		}
	}
	if sites == 0 {
		t.Fatal("扫描到 0 个 NewChecksumStore 调用点（解析器可能失效，门禁会假绿）")
	}
}
