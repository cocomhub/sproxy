// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAtomicRename_MissingSourceKeepsDestination 钉住「慢路径不得破坏目标」。
//
// 旧实现（慢路径）第一步是 `_ = rt.Remove(dstRel)`，即**无条件删除目标**再重试 rename：
// 只要快路径 rename 失败（Windows 句柄抖动、源不存在、权限拒绝……），目标就被物理删除，
// 重试再失败时**新旧两不存**⇒ 静默数据丢失。这里用「源不存在」构造确定性的慢路径：
// 旧实现会把 session 数据删掉，新实现必须原样保留目标并只回报错误。
func TestAtomicRename_MissingSourceKeepsDestination(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	dstAbs := filepath.Join(dir, "dst.txt")
	if werr := os.WriteFile(dstAbs, []byte("precious"), 0o644); werr != nil {
		t.Fatalf("准备目标文件: %v", werr)
	}

	if rerr := rt.AtomicRename("missing.txt", "dst.txt"); rerr == nil {
		t.Fatal("源不存在时 AtomicRename 必须报错")
	}

	got, rerr := os.ReadFile(dstAbs)
	if rerr != nil {
		t.Fatalf("目标文件被慢路径删除（数据丢失）: %v", rerr)
	}
	if string(got) != "precious" {
		t.Fatalf("目标内容被改写: %q", string(got))
	}
}

// TestAtomicRename_SuccessStillReplaces 守住「替换语义」不回退：上传覆盖同名文件依赖它
// （快路径 rename 直接替换），修复慢路径破坏性**不得**影响这条既有契约。
func TestAtomicRename_SuccessStillReplaces(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })

	if werr := os.WriteFile(filepath.Join(dir, "dst.txt"), []byte("old"), 0o644); werr != nil {
		t.Fatalf("准备目标: %v", werr)
	}
	if werr := os.WriteFile(filepath.Join(dir, "src.txt"), []byte("new"), 0o644); werr != nil {
		t.Fatalf("准备源: %v", werr)
	}

	if rerr := rt.AtomicRename("src.txt", "dst.txt"); rerr != nil {
		t.Fatalf("AtomicRename: %v", rerr)
	}
	got, rerr := os.ReadFile(filepath.Join(dir, "dst.txt"))
	if rerr != nil || string(got) != "new" {
		t.Fatalf("覆盖未生效: content=%q err=%v", string(got), rerr)
	}
}
