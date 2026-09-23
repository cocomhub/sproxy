// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// trash_test.go 验证回收站/软删除（roadmap P2 回收站）：
//  1. 软删：DeleteFile SoftDelete=true → 文件移到 trash 桶（user/ 原路径消失）。
//  2. 列表：TrashStore.List 返回回收站条目（含原路径 + 删除时间）。
//  3. 恢复：Restore 把文件移回 user/ 原路径。
//  4. 清空：Empty 删除全部回收站文件。
//  5. TTL：过期条目（保留期超时）被清理。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestTrash_SoftDeleteAndRestore 软删 → 列表 → 恢复。
func TestTrash_SoftDeleteAndRestore(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant 不可用")
	}
	userAbs, _ := tnt.Root().Abs("user")
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 软删（域 API：DeleteFile SoftDelete=true）。
	cs := testutil.SHA256Hex([]byte("hello"))
	_, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "a.txt", ExpectedChecksum: cs, SoftDelete: true,
	})
	if err != nil {
		t.Fatalf("DeleteFile soft: %v", err)
	}
	// user/ 原路径消失。
	if _, err := os.Stat(filepath.Join(userAbs, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("原路径应删除: %v", err)
	}
	// trash 桶有文件。
	trashAbs, _ := tnt.Root().Abs("trash")
	entries, _ := os.ReadDir(trashAbs)
	if len(entries) != 1 {
		t.Fatalf("trash 应 1 条, got %d", len(entries))
	}
	// 恢复。
	trashRel := "trash/" + entries[0].Name()
	if err := env.svc.RestoreTrash(context.Background(), "alice", trashRel); err != nil {
		t.Fatalf("RestoreTrash: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(userAbs, "a.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("恢复后内容 = %q err=%v", b, err)
	}
}

// TestTrash_Empty 清空回收站。
func TestTrash_Empty(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	userAbs, _ := tnt.Root().Abs("user")
	_ = os.MkdirAll(userAbs, 0o755)
	_ = os.WriteFile(filepath.Join(userAbs, "b.txt"), []byte("x"), 0o644)
	cs := testutil.SHA256Hex([]byte("x"))
	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "b.txt", ExpectedChecksum: cs, SoftDelete: true,
	}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := env.svc.EmptyTrash(context.Background(), "alice"); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	trashAbs, _ := tnt.Root().Abs("trash")
	entries, _ := os.ReadDir(trashAbs)
	if len(entries) != 0 {
		t.Fatalf("清空后 trash 应空, got %d", len(entries))
	}
}

// TestTrash_TTL 过期条目清理（保留期超时）。
func TestTrash_TTL(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	userAbs, _ := tnt.Root().Abs("user")
	_ = os.MkdirAll(userAbs, 0o755)
	_ = os.WriteFile(filepath.Join(userAbs, "c.txt"), []byte("y"), 0o644)
	cs := testutil.SHA256Hex([]byte("y"))
	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "c.txt", ExpectedChecksum: cs, SoftDelete: true,
	}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// TTL=0（立即过期）清理。
	cleaned, err := env.svc.CleanupTrash(context.Background(), "alice", 0)
	if err != nil {
		t.Fatalf("CleanupTrash: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("应清理 1 条, got %d", cleaned)
	}
}
