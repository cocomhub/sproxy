// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 2026-11-17 实测缺口（数据一致性）：卷索引/meta 显示写入成功，但远端 blob **实际不存在**
// （百度 31066）⇒ 读取时才 500 ✗。`ReverifyEntry` 是该场景的**自动重试**动作：
// 仍不可见/不一致 ⇒ 返回错误 ⇒ 事项退避重试、达上限**转人工**（绝不静默）✗。

// TestReverifyEntry_InvisibleReturnsError 锁定：块不可见 ⇒ 必须报错（交给 issues 重试/转人工）。
//
// 变异：把它改成「不可见也返回 nil」⇒ 本用例红（真丢块将一直被当成成功 ✗）。
func TestReverifyEntry_InvisibleReturnsError(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	rec.hideAll = true // 所有块/ meta 都不可见（模拟远端 blob 缺失）
	if err := writeOneFile(t, rec, "reverify-invisible.bin", 128<<10); err != nil {
		t.Fatalf("写入期不可见属待复核（应成功）：%v", err)
	}
	fs := rec.fs(t)
	if err := fs.ReverifyEntry(context.Background(), "reverify-invisible.bin"); err == nil {
		t.Fatal("块仍不可见 ⇒ ReverifyEntry 必须报错（否则真丢块会被静默当成成功 ✗）")
	}
}

// TestReverifyEntry_VisibleSucceeds 锁定：全部可见 ⇒ 返回 nil（事项 resolve）。
func TestReverifyEntry_VisibleSucceeds(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	if err := writeOneFile(t, rec, "reverify-ok.bin", 64<<10); err != nil {
		t.Fatalf("写入：%v", err)
	}
	fs := rec.fs(t)
	if err := fs.ReverifyEntry(context.Background(), "reverify-ok.bin"); err != nil {
		t.Fatalf("全部可见应成功，got %v", err)
	}
}

// TestReverifyEntry_UnknownRelFails 锁定：rel 不在索引 ⇒ 报错（转人工，不静默成功）。
func TestReverifyEntry_UnknownRelFails(t *testing.T) {
	t.Parallel()
	rec := newHintVerifyRecorder(syncpkg.NewLocalFS(t.TempDir(), nil))
	fs, ferr := NewFS(rec, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if ferr != nil {
		t.Fatalf("NewFS: %v", ferr)
	}
	if err := fs.ReverifyEntry(context.Background(), "no-such.bin"); err == nil {
		t.Fatal("不存在的 rel 必须报错（需人工处理）")
	}
}
