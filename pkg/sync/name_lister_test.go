// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"testing"
)

// nameOnlyFS 只实现 NameLister（验证能力探测与装饰器转发）。
type nameOnlyFS struct {
	FS
	called int
}

func (n *nameOnlyFS) ListDirNames(ctx context.Context, relPath string) ([]Entry, error) {
	n.called++
	return []Entry{{Name: "a.bin", Path: "a.bin"}}, nil
}

// TestAssertNameLister 能力探测：实现 ⇒ ok；未实现 ⇒ false（调用方回落 ListDir）。
// 变异：AssertNameLister 恒返回 nil,false ⇒ 本测红。
func TestAssertNameLister(t *testing.T) {
	t.Parallel()
	if _, ok := AssertNameLister(NewLocalFS(t.TempDir(), nil)); ok {
		t.Fatal("LocalFS 未实现 NameLister，应为 false")
	}
	nl, ok := AssertNameLister(&nameOnlyFS{})
	if !ok || nl == nil {
		t.Fatal("nameOnlyFS 实现了 NameLister，应返回 ok")
	}
}

// TestPrefixFS_ListDirNamesForward 装饰器必须转发轻量列举能力，否则装配期会退回
// 「逐条补精确 size」的高成本路径（外部卷限流根源）。
// 变异：PrefixFS 不实现 ListDirNames ⇒ AssertNameLister(prefix) 返回 false ⇒ 本测红。
func TestPrefixFS_ListDirNamesForward(t *testing.T) {
	t.Parallel()
	inner := &nameOnlyFS{}
	p := NewPrefixFS(inner, "b/s")
	nl, ok := AssertNameLister(p)
	if !ok {
		t.Fatal("PrefixFS 应转发 NameLister 能力")
	}
	es, err := nl.ListDirNames(context.Background(), "x")
	if err != nil {
		t.Fatalf("ListDirNames: %v", err)
	}
	if inner.called != 1 {
		t.Fatalf("应调用底层 ListDirNames 1 次，实际 %d", inner.called)
	}
	if len(es) != 1 {
		t.Fatalf("应返回 1 条，实际 %d", len(es))
	}
}
