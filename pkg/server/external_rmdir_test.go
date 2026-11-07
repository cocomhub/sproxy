// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// external_rmdir_test.go 钉住外部卷目录的**递归删除**原语（加密卷 rmdir 的底座）：
// 先子后父、整棵树消失；层级超限拒绝（防异常后端/键空间环）。

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func TestDeleteExternalTree_RemovesNestedTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, testLogger())
	ctx := context.Background()
	// 树：dir/top.bin、dir/sub/inner.bin、dir/sub/deep/deeper.bin
	for _, p := range []string{"dir/sub/deep", "dir/other"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"dir/top.bin", "dir/sub/inner.bin", "dir/sub/deep/deeper.bin", "dir/other/x.bin"} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("payload-"+p), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h := &Handlers{logger: testLogger()}
	if err := h.deleteExternalTree(ctx, fsys, "dir", 0); err != nil {
		t.Fatalf("deleteExternalTree: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "dir")); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("整棵目录树应已删除, got err=%v", serr)
	}
}

func TestDeleteExternalTree_DepthLimitRejects(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, testLogger())
	deep := filepath.Join(dir, strings.Repeat("d/", maxExternalTreeDepth+3))
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "leaf.bin"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{logger: testLogger()}
	err := h.deleteExternalTree(context.Background(), fsys, "d", 0)
	if err == nil {
		t.Fatal("超过深度上限必须拒绝（防异常后端/键空间环）")
	}
	if !strings.Contains(err.Error(), "层级过深") {
		t.Fatalf("错误文案应指明层级过深, got: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "d", "d")); serr != nil {
		t.Fatalf("拒绝时不应破坏浅层目录, got err=%v", serr)
	}
}
