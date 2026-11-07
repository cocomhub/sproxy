// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// prefixfs_root_test.go 钉住「视图根必须互逆」不变式（独立审核 P1，2026-10-18）：
//
//	strip(Key(k)) == k  对**所有** k 成立，尤其 k == ""（视图根）。
//
// 缺陷：`Key("")` 返回 `trimSuffixSlash(Prefix)`（如 `"user"`，**无尾斜杠**），而 `strip` 只会
// 去掉 `Prefix`（`"user/"`）前缀 ⇒ `strip("user") == "user"`（**不互逆**）⇒ 根条目的 `Path`
// 泄漏内层桶键。下游把它当**用户路径**用：
//   - 备份空子树：`WalkEntries(…, "")` 的空子树分支原样返回根条目 ⇒ `dst.MakeDir(e.Path)`
//     在目标 `PrefixFS(…, "user/")` 上得到 `Key("user") == "user/user"` ⇒ **幽灵目录**；
//   - 同步/删除传播按错路径 `Stat`/`Delete`。
//
// 修法：`strip` 显式把「去尾斜杠的前缀」映射回 `""`（视图根）。

import (
	"context"
	"testing"
)

// TestPrefixFS_RootRoundTrip 纯函数层：strip(Key("")) 必须是视图根 ""。
func TestPrefixFS_RootRoundTrip(t *testing.T) {
	t.Parallel()
	p := NewPrefixFS(nil, "user/")
	if got := p.Key(""); got != "user" {
		t.Fatalf("前置：Key(\"\")=%q, want %q（去尾斜杠的前缀）", got, "user")
	}
	if got := p.strip(p.Key("")); got != "" {
		t.Fatalf("strip(Key(\"\"))=%q, want \"\"（未修必红：视图根不互逆 ⇒ 根条目 Path 泄漏内层桶键）", got)
	}
	// 非根仍须正确（回归保护）。
	if got := p.strip(p.Key("a/b")); got != "a/b" {
		t.Fatalf("strip(Key(\"a/b\"))=%q, want \"a/b\"", got)
	}
}

// TestPrefixFS_EmptySubtreeRootPathIsViewRoot 行为层：空子树的根条目 Path 必须是 ""，
// 否则备份/同步会在目标造出幽灵目录 `user/user`。
func TestPrefixFS_EmptySubtreeRootPathIsViewRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := NewLocalFS(t.TempDir(), nil)
	if err := inner.MakeDir(ctx, "user"); err != nil {
		t.Fatalf("MakeDir(user): %v", err)
	}
	pfs := NewPrefixFS(inner, "user/")

	e, err := pfs.Stat(ctx, "")
	if err != nil {
		t.Fatalf("Stat(\"\"): %v", err)
	}
	if e == nil {
		t.Fatal("视图根必须存在")
	}
	if e.Path != "" {
		t.Fatalf("根条目 Path=%q, want \"\"（未修必红：strip 不互逆）", e.Path)
	}

	entries, werr := WalkEntries(ctx, pfs, "", true, false, nil)
	if werr != nil {
		t.Fatalf("WalkEntries: %v", werr)
	}
	if len(entries) != 1 {
		t.Fatalf("空子树应返回 1 个根条目，得 %d", len(entries))
	}
	if entries[0].Path != "" {
		t.Fatalf("空子树根条目 Path=%q, want \"\"（未修必红：下游 dst.MakeDir(e.Path) 会造出幽灵目录 user/user）", entries[0].Path)
	}
}
