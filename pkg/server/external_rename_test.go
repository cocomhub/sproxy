// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// external_rename_test.go 钉住「外部卷不支持原地改名时的等价路径」：底层返回
// `syncpkg.ErrUnsupported`（secretdata 文件 basename 锚定于加密 meta）⇒ 装配层做
// 「读源 → 写目标 → 删源」，且**写成功后才删源**（崩溃窗口只留双份，不丢数据）。
//
// 变异：去掉 fsys.Delete(srcKey) → 旧名仍在（本用例红）；改为「先删后写」→ 写失败时
// 内容已丢（本用例的写失败分支红）。

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func TestCopyThenDeleteExternal_MovesContentAndRemovesSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, testLogger())
	ctx := context.Background()
	content := []byte("external rename payload")
	if err := os.WriteFile(filepath.Join(dir, "src.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyThenDeleteExternal(ctx, fsys, "src.bin", "dst.bin"); err != nil {
		t.Fatalf("copyThenDeleteExternal: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "dst.bin"))
	if err != nil {
		t.Fatalf("目标应存在: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("目标内容不一致: %q", got)
	}
	if _, serr := os.Stat(filepath.Join(dir, "src.bin")); !errors.Is(serr, fs.ErrNotExist) {
		t.Fatalf("源应已被删除, got err=%v", serr)
	}
}

func TestCopyThenDeleteExternal_WriteFailureKeepsSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.Background()
	content := []byte("keep me on failure")
	if err := os.WriteFile(filepath.Join(dir, "src.bin"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	// 目标路径是**目录** → 写失败；源必须原样保留（写成功才删源）。
	if err := os.Mkdir(filepath.Join(dir, "dst.bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyThenDeleteExternal(ctx, syncpkg.NewLocalFS(dir, testLogger()), "src.bin", "dst.bin"); err == nil {
		t.Fatal("目标为目录时写应失败")
	}
	got, err := os.ReadFile(filepath.Join(dir, "src.bin"))
	if err != nil {
		t.Fatalf("写失败后源必须保留: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("源内容不得被破坏: %q", got)
	}
}
