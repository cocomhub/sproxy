// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// seedRenameSource 在 alice 的 user 桶写入内容，返回其 SHA-256（rename 必填 checksum）。
func seedRenameSource(t *testing.T, env *dirsEnv, rel, content string) string {
	t.Helper()
	abs := filepath.Join(env.root, "alice", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("建父目录: %v", err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}
	return sha256Hex([]byte(content))
}

// renameErrStatus 取 *HTTPError 的状态码（rename 的对外契约一律用 *HTTPError 表达）。
func renameErrStatus(t *testing.T, err error) int {
	t.Helper()
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("期望 *HTTPError，实际 err=%v", err)
	}
	return he.Status
}

// assertRenameLocksFree 断言 (owner, rel) 的锁未被泄漏——泄漏会让该路径此后的
// 上传/删除/移动**永久 409**，是比竞态更难排查的故障。
func assertRenameLocksFree(t *testing.T, env *dirsEnv, rels ...string) {
	t.Helper()
	for _, rel := range rels {
		release, ok := env.svc.rt.fileLocks().Acquire("alice", rel)
		if !ok {
			t.Fatalf("锁 %s 泄漏（未释放）", rel)
		}
		release()
	}
}

// TestRenameFile_RejectsWhenTargetLocked 覆盖 rename 竞争窗口的收口。
//
// 背景：renameInHome 原先只做「Stat(目标) 不存在 → Rename」，两步之间是 TOCTOU 窗口；
// 并发写入者（上传 / 分块 complete / 另一条 rename）在窗口内创建目标后，`Rename` 会
// **静默覆盖**它（POSIX rename 是替换语义）⇒ 数据丢失，且「目标路径已存在」的 409 门禁
// 形同虚设。
//
// 修法：rename 与上传/删除/分块 complete 共用同一 rel 锁池，把「目标检查 + 改名」纳入
// 临界区。本条测试把目标 rel 的锁占住，断言 rename 直接 409 而**不触盘**。
func TestRenameFile_RejectsWhenTargetLocked(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	sum := seedRenameSource(t, env, "user/f.txt", "payload")

	release, ok := env.svc.rt.fileLocks().Acquire("alice", "user/g.txt")
	if !ok {
		t.Fatal("前置：目标 rel 锁应可获取")
	}
	defer release()

	_, err := env.svc.RenameFile(context.Background(), RenameFileInput{
		Owner: "alice", From: "f.txt", To: "g.txt", ExpectedChecksum: sum,
	})
	if got := renameErrStatus(t, err); got != http.StatusConflict {
		t.Fatalf("目标被持锁应 409, got %d (%v)", got, err)
	}
	if _, statErr := os.Stat(filepath.Join(env.root, "alice", "user", "f.txt")); statErr != nil {
		t.Fatalf("被拒分支不得触盘（源文件应原地保留）: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(env.root, "alice", "user", "g.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("被拒分支不得创建目标, stat err=%v", statErr)
	}
}

// TestRenameFile_RejectsWhenSourceLocked 覆盖相反方向：源 rel 被其它操作持锁（如正在上传
// 该路径）时，rename 必须 409——否则会在「写入尚未落盘」的源上做 checksum 校验并把它搬走。
func TestRenameFile_RejectsWhenSourceLocked(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	sum := seedRenameSource(t, env, "user/f.txt", "payload")

	release, ok := env.svc.rt.fileLocks().Acquire("alice", "user/f.txt")
	if !ok {
		t.Fatal("前置：源 rel 锁应可获取")
	}
	defer release()

	_, err := env.svc.RenameFile(context.Background(), RenameFileInput{
		Owner: "alice", From: "f.txt", To: "g.txt", ExpectedChecksum: sum,
	})
	if got := renameErrStatus(t, err); got != http.StatusConflict {
		t.Fatalf("源被持锁应 409, got %d (%v)", got, err)
	}
}

// TestRenameFile_ReleasesBothLocksOnSuccess 覆盖成功路径的锁释放（源 + 目标两把都要放）。
func TestRenameFile_ReleasesBothLocksOnSuccess(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	sum := seedRenameSource(t, env, "user/f.txt", "payload")

	if _, err := env.svc.RenameFile(context.Background(), RenameFileInput{
		Owner: "alice", From: "f.txt", To: "g.txt", ExpectedChecksum: sum,
	}); err != nil {
		t.Fatalf("重命名应成功: %v", err)
	}
	assertRenameLocksFree(t, env, "user/f.txt", "user/g.txt")
}

// TestRenameFile_ReleasesLocksOnRejection 覆盖失败路径的锁释放：目标已存在 → 409 之后
// 两把锁都必须归还（否则该路径彻底不可写）。
func TestRenameFile_ReleasesLocksOnRejection(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	sum := seedRenameSource(t, env, "user/f.txt", "source")
	seedRenameSource(t, env, "user/g.txt", "already-here")

	_, err := env.svc.RenameFile(context.Background(), RenameFileInput{
		Owner: "alice", From: "f.txt", To: "g.txt", ExpectedChecksum: sum,
	})
	if got := renameErrStatus(t, err); got != http.StatusConflict {
		t.Fatalf("目标已存在应 409, got %d (%v)", got, err)
	}
	assertRenameLocksFree(t, env, "user/f.txt", "user/g.txt")
	// 目标内容不得被覆盖。
	b, readErr := os.ReadFile(filepath.Join(env.root, "alice", "user", "g.txt"))
	if readErr != nil || string(b) != "already-here" {
		t.Fatalf("目标被改写: content=%q err=%v", string(b), readErr)
	}
}
