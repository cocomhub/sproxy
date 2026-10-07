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
	"strings"
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
	trashRel := trashPrefix + entries[0].Name()
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

// TestTrash_MetaFollowsFile M2 回归：软删时 meta sidecar 随主文件移入 trash 桶 →
// 恢复时一起回 meta 桶 → 清空/清理时随条目一起删（无孤儿，生命周期一致）。
func TestTrash_MetaFollowsFile(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.fileMeta = true // 注入 testMetaPolicy（可信卷 meta 能力）
	env.enableWriteDefaults()
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant 不可用")
	}
	root := tnt.Root()
	userAbs, _ := root.Abs("user")
	_ = os.MkdirAll(userAbs, 0o755)
	// 上传经 writeFileSettle → 自动建 meta（fileMeta 装配生效）。
	cs := testutil.SHA256Hex([]byte("hello"))
	if _, err := env.svc.WriteFile(context.Background(), WriteFileInput{
		Owner: "alice", RemotePath: "a.txt", ExpectedChecksum: cs, ClientSize: 5,
	}, strings.NewReader("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	metaPath := filepath.Join(userAbs, "..", "meta", "a.txt.meta") // meta/<rel>.meta
	if _, err := os.Stat(filepath.Clean(metaPath)); err != nil {
		t.Fatalf("上传应建 meta sidecar: %v", err)
	}
	// 软删 → meta 随迁 trash 桶。
	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "a.txt", ExpectedChecksum: cs, SoftDelete: true,
	}); err != nil {
		t.Fatalf("DeleteFile soft: %v", err)
	}
	if _, err := os.Stat(filepath.Clean(metaPath)); !os.IsNotExist(err) {
		t.Fatalf("软删后 meta 应随迁（原 meta 桶无 sidecar）: %v", err)
	}
	// trash 桶有 2 条（主文件 + meta sidecar）。
	trashAbs, _ := root.Abs("trash")
	entries, _ := os.ReadDir(trashAbs)
	if len(entries) != 2 {
		t.Fatalf("trash 应 2 条（主文件+meta）, got %d", len(entries))
	}
	// 恢复 → meta 回 meta 桶（用实际 trash 条目名——时间戳后缀由软删生成，扫描获得）。
	trashRel := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "user_a.txt"+trashDeletedSuffix) && !strings.Contains(e.Name(), trashMetaMarker) {
			trashRel = trashPrefix + e.Name()
			break
		}
	}
	if trashRel == "" {
		t.Fatal("trash 缺主文件条目")
	}
	if err := env.svc.RestoreTrash(context.Background(), "alice", trashRel); err != nil {
		t.Fatalf("RestoreTrash: %v", err)
	}
	if _, err := os.Stat(filepath.Clean(metaPath)); err != nil {
		t.Fatalf("恢复后 meta 应回 meta 桶: %v", err)
	}
	// 再软删 + 清空 → trash 条目（含 meta）全部删除，无孤儿。
	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "a.txt", ExpectedChecksum: cs, SoftDelete: true,
	}); err != nil {
		t.Fatalf("DeleteFile soft 2nd: %v", err)
	}
	if err := env.svc.EmptyTrash(context.Background(), "alice"); err != nil {
		t.Fatalf("EmptyTrash: %v", err)
	}
	entries2, _ := os.ReadDir(trashAbs)
	if len(entries2) != 0 {
		t.Fatalf("EmptyTrash 后 trash 应空, got %d", len(entries2))
	}
}

// TestFlattenRel_UnderscoreRoundtrip D-C1 回归：rel 文件名含 `_` 时 flatten→unflatten
// 必须互逆（`user/a_b/c.txt` 不得还原成 `user/a/b/c.txt`——原 ReplaceAll 双射破坏致
// 恢复路径错位/409 永久丢失）。
func TestFlattenRel_UnderscoreRoundtrip(t *testing.T) {
	t.Parallel()
	cases := []string{
		"user/a.txt",
		"user/a_b/c.txt",     // 文件名含 _
		"user/dir_a/x_y.bin", // 目录+文件都含 _
		"user/a__b/c.txt",    // 文件名本身含 __
		"user/单级/中文名.txt",    // 中文
		"user/x/y/z.bin",
	}
	for _, rel := range cases {
		if back := unflattenRel(flattenRel(rel)); back != rel {
			t.Errorf("flatten/unflatten(%q) → %q，应互逆", rel, back)
		}
	}
	// 不同 rel 不得映射到同一 flat（双射不破坏的额外断言）。
	seen := map[string]string{}
	for _, rel := range cases {
		flat := flattenRel(rel)
		if prev, ok := seen[flat]; ok && prev != rel {
			t.Errorf("flat 冲突: %q 与 %q 同映射 %q", prev, rel, flat)
		}
		seen[flat] = rel
	}
}

// TestTrash_SoftDeleteRestore_Underscore D-C1 端到端：软删含 _ 的文件 → 恢复回到原
// 路径（不落到错位目录）。
func TestTrash_SoftDeleteRestore_Underscore(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	root := tnt.Root()
	userAbs, _ := root.Abs("user")
	_ = os.MkdirAll(filepath.Join(userAbs, "a_b"), 0o755) // 目录名含 _
	if err := os.WriteFile(filepath.Join(userAbs, "a_b", "c.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := testutil.SHA256Hex([]byte("u"))
	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "a_b/c.txt", ExpectedChecksum: cs, SoftDelete: true,
	}); err != nil {
		t.Fatalf("软删: %v", err)
	}
	// 恢复（扫描 trash 主条目——flat 编码：rel=user/a_b/c.txt → user_a__b_c.txt）。
	trashAbs, _ := root.Abs("trash")
	entries, _ := os.ReadDir(trashAbs)
	trashRel := ""
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "user_a__b_c.txt"+trashDeletedSuffix) {
			trashRel = trashPrefix + e.Name()
			break
		}
	}
	if trashRel == "" {
		t.Fatal("trash 缺主条目")
	}
	if err := env.svc.RestoreTrash(context.Background(), "alice", trashRel); err != nil {
		t.Fatalf("恢复: %v", err)
	}
	// 恢复后必须在原路径 a_b/c.txt（不是 a/b/c.txt）。
	if _, err := os.Stat(filepath.Join(userAbs, "a_b", "c.txt")); err != nil {
		t.Fatalf("恢复应回原路径 a_b/c.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(userAbs, "a", "b", "c.txt")); err == nil {
		t.Fatal("不得落到错位目录 a/b/c.txt")
	}
}
