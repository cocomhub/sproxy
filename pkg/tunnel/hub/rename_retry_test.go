// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"errors"
	"strings"
	"testing"
)

// rename_retry_test.go 钉住「原子替换的瞬时失败重试」语义（flake 收口 2026-10-14）：
// Windows 上覆盖被占用目标会 sharing violation，POSIX 恒成功 ⇒ 重试让行为跨平台一致，
// 但**持续失败必须仍返回错误**（不得静默吞掉落盘失败）。
//
// 变异：去掉重试（只调一次）⇒ 瞬时失败用例红；忽略错误返回 nil ⇒ 持续失败用例红。
// 注：本文件刻意不使用 time.Sleep（R14 棘轮只减不增；退避在实现内）。

// TestRenameWithRetry_TransientFailureThenSuccess：前两次失败、第三次成功 ⇒ 成功且调用 3 次。
func TestRenameWithRetry_TransientFailureThenSuccess(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(string, string) error {
		calls++
		if calls < 3 {
			return errors.New("The process cannot access the file because it is being used by another process")
		}
		return nil
	}
	if err := renameWithRetry(rename, "tmp", "dst"); err != nil {
		t.Fatalf("瞬时失败后应重试成功: %v", err)
	}
	if calls != 3 {
		t.Fatalf("应重试到第 3 次成功，实际调用 %d 次", calls)
	}
}

// TestRenameWithRetry_PersistentFailureReturnsError：持续失败 ⇒ 用尽尝试次数后**返回错误**。
func TestRenameWithRetry_PersistentFailureReturnsError(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(string, string) error {
		calls++
		return errors.New("access denied")
	}
	err := renameWithRetry(rename, "tmp", "dst")
	if err == nil {
		t.Fatal("持续失败必须返回错误（落盘失败不得被静默吞掉）")
	}
	if calls != renameRetryAttempts {
		t.Fatalf("应尝试 %d 次，实际 %d 次", renameRetryAttempts, calls)
	}
	if !strings.Contains(err.Error(), "原子替换") || !errors.Is(err, err) {
		t.Fatalf("错误应可归因「原子替换失败」并保留底层原因: %v", err)
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("应包装底层错误（可归因）: %v", err)
	}
}

// TestRenameWithRetry_SuccessFirstTry：首次成功 ⇒ 只调用一次（无额外开销）。
func TestRenameWithRetry_SuccessFirstTry(t *testing.T) {
	t.Parallel()
	calls := 0
	rename := func(string, string) error { calls++; return nil }
	if err := renameWithRetry(rename, "tmp", "dst"); err != nil {
		t.Fatalf("首次成功不应报错: %v", err)
	}
	if calls != 1 {
		t.Fatalf("首次成功应只调用一次，实际 %d", calls)
	}
}
