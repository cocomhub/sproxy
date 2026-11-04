// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// cd_scope_test.go 锁「漫游当前目录按 context 隔离」。
//
// 真实缺陷（2026-11-04 实测）：cd 状态曾持久化在**全局**文件
// `<XDG cache>/sproxy/current_dir`，不按 context 隔离 ⇒ 在 A 环境 `cd subdir` 后切到
// B 环境，后续 upload/download 会静默落到 B 的 `subdir/`（用户以为在 B 的根目录）。
// 本机缓存曾被污染成 `subdir/C:/Program Files/Git/subdir` 这种形态。
//
// 语义：
//   1. 不同 context 的目录互不可见；
//   2. 切回原 context 恢复它自己的目录；
//   3. 升级兼容：首次在新 context 下运行，从历史全局文件**迁移一次**（读完即删，
//      避免其它 context 继续继承旧值）。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrg/xdg"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
)

// withScope 设置当前解析出的 context 名，并在用例结束时恢复（包级 globals 隔离）。
func withScope(t *testing.T, name string) {
	t.Helper()
	old := resolvedContext
	if name == "" {
		resolvedContext = nil
	} else {
		resolvedContext = &contextcfg.Resolved{ContextName: name}
	}
	t.Cleanup(func() { resolvedContext = old })
}

func TestCdState_IsolatedPerContext(t *testing.T) {
	_ = setCacheDirForTest(t)

	// A 环境：cd 到 subdir。
	withScope(t, "ctx-a")
	saveCurrentDirValue("subdir")
	if got := loadCurrentDir(); got != "subdir" {
		t.Fatalf("ctx-a 读回 = %q, want subdir", got)
	}

	// B 环境：必须是干净的根目录（不得继承 A 的 subdir）。
	withScope(t, "ctx-b")
	if got := loadCurrentDir(); got != "" {
		t.Fatalf("ctx-b 继承了 ctx-a 的目录：%q（切环境后上传会落到错误目录）", got)
	}
	saveCurrentDirValue("other")
	if got := loadCurrentDir(); got != "other" {
		t.Fatalf("ctx-b 读回 = %q, want other", got)
	}

	// 切回 A：恢复它自己的目录。
	withScope(t, "ctx-a")
	if got := loadCurrentDir(); got != "subdir" {
		t.Fatalf("切回 ctx-a 后 = %q, want subdir", got)
	}
}

func TestCdState_LegacyGlobalFileMigratesOnce(t *testing.T) {
	_ = setCacheDirForTest(t)

	// 模拟升级前留下的全局文件。
	legacy, err := xdg.CacheFile(filepath.Join("sproxy", "current_dir"))
	if err != nil || legacy == "" {
		t.Skip("xdg.CacheFile 不可用")
	}
	if merr := os.MkdirAll(filepath.Dir(legacy), 0o755); merr != nil {
		t.Fatal(merr)
	}
	if werr := os.WriteFile(legacy, []byte("legacy/dir"), 0o644); werr != nil {
		t.Fatal(werr)
	}

	// 第一个启用的 context 继承旧值，并且旧文件被删除（其它 context 不再继承）。
	withScope(t, "ctx-a")
	if got := loadCurrentDir(); got != "legacy/dir" {
		t.Fatalf("首次迁移 = %q, want legacy/dir", got)
	}
	if _, statErr := os.Stat(legacy); !os.IsNotExist(statErr) {
		t.Fatalf("迁移后旧全局文件应被删除（err=%v）", statErr)
	}

	// 另一个 context 不得继承。
	withScope(t, "ctx-b")
	if got := loadCurrentDir(); got != "" {
		t.Fatalf("ctx-b 不应继承已迁移的旧值：%q", got)
	}
}

func TestSanitizeScope_RejectsPathSeparators(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"../../etc", "a/b", `a\b`, "a b"} {
		out := sanitizeScope(in)
		if out == "" {
			t.Fatalf("sanitizeScope(%q) 不应为空", in)
		}
		if filepath.Base(out) != out || len(out) != len(in) {
			t.Fatalf("sanitizeScope(%q) = %q（含路径分隔符或长度异常）", in, out)
		}
	}
}
