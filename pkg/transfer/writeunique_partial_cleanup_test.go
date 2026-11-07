// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

// writeunique_partial_cleanup_test.go 钉住「写失败必须清理残骸 ⇒ 重试可自愈」
// （独立审核 P2，2026-10-18）。
//
// 缺陷：`writeUnique`（Overwrite=false）在 `WriteIfAbsent` 报**真错误**时直接 return，未清理
// 非原子后端留下的**半截对象** ⇒ 后续每次重试都因「目标已存在」被判 `ErrExists` ⇒ **永久阻断**
// （resume 走同一路径 ⇒ 用户只能人工删对象），且半截对象成为孤儿（字节已计入 owner 配额）。
//
// 本用例用「先写半截再报错、且只失败一次」的假后端**确定性**复现该场景，核心断言是
// **重试成功**（不只是「错误被返回」）。

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// partialFailFS 模拟**非原子**后端：首次 WriteIfAbsent 先写半截再报错（残骸真实落到 LocalFS）。
type partialFailFS struct {
	*syncpkg.LocalFS
	fail atomic.Bool
}

func (f *partialFailFS) WriteIfAbsent(ctx context.Context, p string, r io.Reader, size, mtime int64) (bool, error) {
	if !f.fail.Load() {
		return f.LocalFS.WriteIfAbsent(ctx, p, r, size, mtime)
	}
	if werr := f.WriteFile(ctx, syncpkg.LogicalKey{}, p, strings.NewReader("partial"), 7, mtime); werr != nil {
		return false, werr
	}
	f.fail.Store(false) // 只失败一次 ⇒ 重试应能成功
	return false, errors.New("injected: mid-write failure")
}

// TestWriteUnique_CleansPartialOnFailureAndRetrySucceeds 写失败后残骸必须被清理，且重试成功。
func TestWriteUnique_CleansPartialOnFailureAndRetrySucceeds(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs := &partialFailFS{LocalFS: inner}
	fs.fail.Store(true)
	ctx := context.Background()
	body := "hello-transfer"
	rel := "a.bin"

	// 第一次：写失败（非原子后端已留下半截）。
	if err := writeUnique(ctx, fs, rel, strings.NewReader(body), int64(len(body)), 0, false); err == nil {
		t.Fatal("首次写必须失败（注入的中途失败）")
	}
	// 残骸必须被清理（未修必红：仍在 ⇒ 重试会撞 ErrExists 永久阻断）。
	if e, serr := inner.Stat(ctx, rel); serr == nil && e != nil {
		t.Fatalf("写失败后残骸未被清理（未修必红）：%+v", e)
	}
	// **核心断言**：重试必须成功（自愈）。未修必红：ErrExists 永久阻断。
	if rerr := writeUnique(ctx, fs, rel, strings.NewReader(body), int64(len(body)), 0, false); rerr != nil {
		t.Fatalf("重试必须成功（未修必红：半截残骸 ⇒ 目标已存在 ⇒ ErrExists 永久阻断，用户无法自愈）：%v", rerr)
	}
	rc, oerr := inner.OpenRead(ctx, rel)
	if oerr != nil {
		t.Fatalf("重试后目标不可读: %v", oerr)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Fatalf("重试后内容=%q, want %q", got, body)
	}
}
