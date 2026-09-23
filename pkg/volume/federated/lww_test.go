// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package federated

// lww_test.go 验证联邦卷冲突语义（roadmap P2 残余：冲突语义 LWW）：
//  1. 同路径两次写（不同 mtime）→ 后写者胜（LWW 覆盖）。
//  2. 未注入 Writer → 恒 ErrReadOnly（零回归）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestFS_LWWLastWriteWins 同路径两次写 → 后写者胜。
func TestFS_LWWLastWriteWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := &memWriter{files: map[string]string{}}
	fs, err := New(w)
	if err != nil {
		t.Fatal(err)
	}
	fs = fs.WithWriter(w)

	// 第一次写（旧 mtime）。
	if err := fs.WriteFile(ctx, "a.txt", strings.NewReader("v1"), 2, time.Now().Add(-time.Hour).UnixNano()); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	// 第二次写（新 mtime）→ LWW 覆盖。
	if err := fs.WriteFile(ctx, "a.txt", strings.NewReader("v2"), 2, time.Now().UnixNano()); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	// 写面应反映 v2 内容（后写覆盖 = LWW）。
	if got := w.files["a.txt"]; got != "v2" {
		t.Fatalf("写面内容 = %q, want v2（LWW 后写覆盖）", got)
	}
}

// TestFS_LWWWithoutWriter_ErrReadOnly 未注入 Writer → 写拒绝（零回归）。
func TestFS_LWWWithoutWriter_ErrReadOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fs, err := New(&memWriter{files: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(ctx, "a.txt", strings.NewReader("x"), 1, time.Now().UnixNano()); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("未注入 Writer 应 ErrReadOnly, got %v", err)
	}
}
