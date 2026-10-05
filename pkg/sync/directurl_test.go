// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"testing"
)

// TestAssertDirectURL_NilForLocalFS：LocalFS 未实现 DirectURLProvider → 断言 nil
// （调用方回落服务端转发）。
func TestAssertDirectURL_NilForLocalFS(t *testing.T) {
	t.Parallel()
	fs := NewLocalFS(t.TempDir(), nil)
	if AssertDirectURL(fs) != nil {
		t.Fatal("LocalFS 不应实现 DirectURLProvider")
	}
}

// directURLFS 是仅测试用的 DirectURL 实现（验证断言非 nil）。
type directURLFS struct {
	LocalFS
}

func (d *directURLFS) DirectURL(_ context.Context, relPath string) (string, bool, error) {
	return "https://d.example/" + relPath + "?sign=test", true, nil
}

// TestAssertDirectURL_Found：实现 DirectURLProvider 的 FS → 断言非 nil。
func TestAssertDirectURL_Found(t *testing.T) {
	t.Parallel()
	fs := &directURLFS{LocalFS: *NewLocalFS(t.TempDir(), nil)}
	d := AssertDirectURL(fs)
	if d == nil {
		t.Fatal("实现 DirectURLProvider 的 FS 应断言非 nil")
	}
	u, ok, err := d.DirectURL(context.Background(), "dir/f.txt")
	if err != nil || !ok || u == "" {
		t.Fatalf("DirectURL=(%q,%v,%v)，应成功", u, ok, err)
	}
}
