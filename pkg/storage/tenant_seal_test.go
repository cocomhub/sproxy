// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// TestOpenTenant_PropagatesSeals 封印必须传播到租户子根：租户根是该卷的一个子根，
// 「直连租户根」等价于「直连源存储」——若只在卷根设防，租户树内的同名路径仍可被直连
// （评审 P0 的机制级根因同理）。
// 变异：去掉 openTenant 中的封印传播 → 租户根写入不再被拒 → 红。
func TestOpenTenant_PropagatesSeals(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parent, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	if serr := parent.SetSeals([]SealRule{{Path: "private/vault", Wrapper: "vault"}}); serr != nil {
		t.Fatalf("SetSeals: %v", serr)
	}
	tn, err := OpenTenant(parent, "alice")
	if err != nil {
		t.Fatalf("OpenTenant: %v", err)
	}
	rt := tn.Root()
	// Windows：租户子根持目录句柄，须显式关闭（否则 TempDir 严格清理报占用）。
	t.Cleanup(func() { _ = rt.Close() })

	// 写：恒拒（含租户树内同名封印路径）。
	w, werr := rt.OpenFile("private/vault/x.bin", os.O_CREATE|os.O_WRONLY, 0o644)
	if werr == nil {
		_ = w.Close()
		t.Fatal("租户根内封印路径写入必须被拒绝")
	}
	if !errors.Is(werr, ErrSealed) {
		t.Fatalf("写拒绝应可归因 ErrSealed, got %v", werr)
	}
	// 读：默认按不存在。
	if _, rerr := rt.Stat("private/vault"); !errors.Is(rerr, fs.ErrNotExist) {
		t.Fatalf("租户根内读取封印路径应按不存在, got %v", rerr)
	}
	// 未封印路径正常。
	if err := rt.MkdirAll("private/ok", 0o755); err != nil {
		t.Fatalf("未封印路径应可用: %v", err)
	}
}
