// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestStaleDirMetaCount_DetectsResidue 锁定：只读判据能识别残留（1 个非权威 meta）。
func TestStaleDirMetaCount_DetectsResidue(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	inner := syncpkg.NewLocalFS(root, nil)
	ctx := context.Background()
	seed := newIssueFS(t, inner, nil)
	if err := seed.MakeDir(ctx, "d1"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	// 造残留
	faulty := &faultDeleteFS{wrap: inner, fail: 1}
	fs := newIssueFS(t, faulty, nil)
	if err := fs.Rename(ctx, "d1", "d2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	container := containerOfDir(t, fs, "d2")
	n, err := fs.StaleDirMetaCount(ctx, container)
	if err != nil {
		t.Fatalf("StaleDirMetaCount: %v", err)
	}
	if n != 1 {
		t.Fatalf("残留数 = %d，期望 1（改名后旧 meta 未删）", n)
	}
}

// containerOfDir 通过内部索引取逻辑目录 → 容器名（测试辅助）。
func containerOfDir(t *testing.T, fs *SecretdataFS, rel string) string {
	t.Helper()
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	c, ok := fs.dirSegs[rel]
	if !ok {
		t.Fatalf("未找到目录 %q 的容器（现有条目数 %d）", rel, len(fs.dirSegs))
	}
	return c
}

// TestCleanupStaleDirMeta_RemovesOnlyNonAuthoritative 锁定核心：删掉非权威那份、
// **保留 Version 最大者**（即改名后的新 meta），且**幂等**（再跑一次为 0）。
func TestCleanupStaleDirMeta_RemovesOnlyNonAuthoritative(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	inner := syncpkg.NewLocalFS(root, nil)
	ctx := context.Background()
	seed := newIssueFS(t, inner, nil)
	if err := seed.MakeDir(ctx, "d1"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	faulty := &faultDeleteFS{wrap: inner, fail: 1}
	fs := newIssueFS(t, faulty, nil)
	if err := fs.Rename(ctx, "d1", "d2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	container := containerOfDir(t, fs, "d2")

	// 清理前确认有残留
	if n, err := fs.StaleDirMetaCount(ctx, container); err != nil || n != 1 {
		t.Fatalf("清理前残留 = (%d,%v)，期望 (1,nil)", n, err)
	}
	removed, err := fs.CleanupStaleDirMeta(ctx, container)
	if err != nil {
		t.Fatalf("CleanupStaleDirMeta: %v", err)
	}
	if removed != 1 {
		t.Fatalf("删除数 = %d，期望 1", removed)
	}
	// 清理后无残留
	if n, cerr := fs.StaleDirMetaCount(ctx, container); cerr != nil || n != 0 {
		t.Fatalf("清理后残留 = (%d,%v)，期望 (0,nil)", n, cerr)
	}
	// **幂等**：重复执行必须成功（这正是"重试直到成功"的前提）
	if r2, err2 := fs.CleanupStaleDirMeta(ctx, container); err2 != nil || r2 != 0 {
		t.Fatalf("重复清理应 (0,nil)，got (%d,%v)", r2, err2)
	}
	// 目录仍可用（保留的是权威 meta：逻辑名已是 d2）
	if _, serr := fs.Stat(ctx, "d2"); serr != nil {
		t.Fatalf("清理后目录应仍可访问: %v", serr)
	}
}

// TestCleanupStaleDirMeta_IdempotentWhenNoResidue 锁定：无残留时 (0,nil)（不误删唯一 meta）。
func TestCleanupStaleDirMeta_IdempotentWhenNoResidue(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(filepath.Join(t.TempDir(), "backing"), nil)
	ctx := context.Background()
	fs := newIssueFS(t, inner, nil)
	if err := fs.MakeDir(ctx, "solo"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	container := containerOfDir(t, fs, "solo")
	if n, err := fs.CleanupStaleDirMeta(ctx, container); err != nil || n != 0 {
		t.Fatalf("无残留应 (0,nil)，got (%d,%v)", n, err)
	}
	if _, err := fs.Stat(ctx, "solo"); err != nil {
		t.Fatalf("目录必须仍存在（不得误删唯一 meta）: %v", err)
	}
}

// TestCleanupStaleDirMeta_RejectsUnreadable 锁定 fail-closed：
// 有 meta 解不开（模拟密钥不符/损坏）⇒ **拒绝清理**（宁可不删，也绝不误删可能唯一可解的那份）。
func TestCleanupStaleDirMeta_RejectsUnreadable(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(filepath.Join(t.TempDir(), "backing"), nil)
	ctx := context.Background()
	seed := newIssueFS(t, inner, nil)
	if err := seed.MakeDir(ctx, "u1"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	faulty := &faultDeleteFS{wrap: inner, fail: 1}
	fs := newIssueFS(t, faulty, nil)
	if err := fs.Rename(ctx, "u1", "u2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	container := containerOfDir(t, fs, "u2")

	// 找到其中一个 dir-meta blob，**保持文件名不变**把内容改成无法解密的垃圾
	// （文件名仍被判为 dir-meta ⇒ 走解密分支 ⇒ 解不开 ⇒ 必须拒绝清理）。
	entries, lerr := inner.ListDir(ctx, container)
	if lerr != nil {
		t.Fatalf("ListDir: %v", lerr)
	}
	target := ""
	for _, e := range entries {
		if !e.IsDir && strings.HasPrefix(e.Name, "AA") && len(e.Name) == 18 {
			target = e.Name
			break
		}
	}
	if target == "" {
		t.Skip("未定位到 dir-meta blob（命名假设变化）")
	}
	if werr := inner.WriteFile(ctx, syncpkg.LogicalKey{}, filepath.Join(container, target),
		strings.NewReader(strings.Repeat("z", 512)), 512, 1); werr != nil {
		t.Fatalf("写入坏 meta: %v", werr)
	}
	if _, cerr := fs.CleanupStaleDirMeta(ctx, container); cerr == nil {
		t.Fatal("存在无法解密的 dir-meta 时必须拒绝清理（fail-closed）")
	}
	// 至少不应把可解的那份删掉（目录仍可访问）。
	if _, serr := fs.Stat(ctx, "u2"); serr != nil {
		t.Fatalf("拒绝清理后目录必须仍可访问: %v", serr)
	}
}

// TestCleanupStaleDirMeta_RejectsEmptyContainer 锁定参数校验（空容器 / nil 接收者）。
func TestCleanupStaleDirMeta_RejectsEmptyContainer(t *testing.T) {
	t.Parallel()
	fs := newIssueFS(t, syncpkg.NewLocalFS(filepath.Join(t.TempDir(), "b"), nil), nil)
	if _, err := fs.CleanupStaleDirMeta(context.Background(), ""); err == nil {
		t.Fatal("空容器路径应报错")
	}
	var nilFS *SecretdataFS
	if _, err := nilFS.CleanupStaleDirMeta(context.Background(), "c"); err == nil {
		t.Fatal("nil 接收者应报错")
	}
	if _, err := nilFS.StaleDirMetaCount(context.Background(), "c"); err == nil {
		t.Fatal("nil 接收者应报错")
	}
}
