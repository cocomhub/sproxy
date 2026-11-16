// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestEnsureOwnedTempDir_CreatesMissingParentChain 锁定回归修复（2026-11-16 线上事故）：
// 进程临时根缺失时（进程重启/根被回收），**必须补建父链并成功创建叶子**，
// 而不是 ENOENT 后走「本地 staging 不可用 ⇒ 内存交付」降级链（实测 50 MiB 写入被拖到 19 分钟）。
func TestEnsureOwnedTempDir_CreatesMissingParentChain(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	// 父链整体缺失：<base>/root/<pid>-<rand>/
	dir := filepath.Join(base, "root", "1234-99", "sd-ch-1234-abcdef")
	if err := ensureOwnedTempDir(dir); err != nil {
		t.Fatalf("父链缺失时应补建并成功，got err=%v", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("目标应为目录: err=%v fi=%v", err, fi)
	}
	if runtime.GOOS != "windows" {
		// Windows 不保留 Unix 权限位 ⇒ 仅在类 Unix 平台断言 0700。
		if perm := fi.Mode().Perm(); perm != 0o700 {
			t.Fatalf("权限应为 0700，got %o", perm)
		}
	}
}

// TestEnsureOwnedTempDir_RejectsSymlinkLeaf 锁定既有安全语义不被本次修复放宽：
// 叶子为 symlink ⇒ 拒绝（不跟随），不得建在其指向的目标上。
func TestEnsureOwnedTempDir_RejectsSymlinkLeaf(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "root"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "victim")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(base, "root", "sd-ch-1-x")
	if err := os.Symlink(target, leaf); err != nil {
		t.Skipf("平台不支持 symlink: %v", err)
	}
	if err := ensureOwnedTempDir(leaf); err == nil {
		t.Fatal("叶子为 symlink 时必须拒绝（不跟随）")
	}
	// 目标目录不得被清空/改动。
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("symlink 目标不应被触碰: %v", err)
	}
}

// TestWriteFile_MissingTempRootIsRecreatedNotDegraded 锁定回归修复（2026-11-16 线上事故）：
// **临时根缺失但可创建**时，写入必须**补建父链后走本地分块路径成功**，
// 而不是 ENOENT ⇒ 「本地 staging 不可用 ⇒ 内存交付」降级链（实测 50 MiB 写入被拖到 19 分钟）。
//
// 断言分块经 staged 本地路径交付（stagedWrites > 0）⇒ 直接钉住「未降级」。
// 变异验证：去掉 ensureOwnedTempDir 的父链补建（Mkdir 失败即返回）⇒ 本用例必红。
func TestWriteFile_MissingTempRootIsRecreatedNotDegraded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("missing-temp-root-"), 20*1024) // 340KiB ⇒ 11 个 32KiB 块
	sfs, probe, tempDir := newInterleaveFS(t, 1)
	// 模拟「进程重启后旧临时根已清 / 被 tempclean 回收」：NewFS 时已创建，此后不在。
	if err := os.RemoveAll(tempDir); err != nil {
		t.Fatalf("移除临时根: %v", err)
	}
	// 一次性源（不可重读）：若仍被降级为内存交付，要么失败、要么 stagedWrites==0。
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("临时根缺失但可创建 ⇒ 应补建后成功（不得降级）: %v", err)
	}
	_, _, stagedWrites, memoryWrites, _, _, stageReadErr := probe.snapshot()
	if stageReadErr != nil {
		t.Fatalf("内层读 stagedPath 失败: %v", stageReadErr)
	}
	if stagedWrites == 0 {
		t.Fatalf("分块未经本地路径交付（staged=0 / memory=%d）⇒ 仍被降级为内存交付 ✗", memoryWrites)
	}
	if !mustReadEqual(t, sfs, ctx, "d/f.bin", payload) {
		t.Fatal("内容不可读回/不一致 ✗")
	}
	// 临时根被重建：目录存在。
	if fi, err := os.Stat(tempDir); err != nil || !fi.IsDir() {
		t.Fatalf("临时根应被补建为目录: err=%v fi=%v", err, fi)
	}
}

// TestEnsureOwnedTempDir_MissingLeafUnderExistingRoot 既有语义：根在、叶子不在 ⇒ 正常创建。
func TestEnsureOwnedTempDir_MissingLeafUnderExistingRoot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	leaf := filepath.Join(root, "sd-dd-1-x")
	if err := ensureOwnedTempDir(leaf); err != nil {
		t.Fatalf("根在时应正常创建叶子，got err=%v", err)
	}
}
