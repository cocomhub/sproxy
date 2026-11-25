// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// mkTree 在租户 user 桶内造一棵小目录树（a.txt + sub/b.txt），返回 user 绝对目录。
func mkTree(t *testing.T, env *dirsEnv) string {
	t.Helper()
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("tenant 不可用")
	}
	userAbs, _ := tnt.Root().Abs("user")
	if err := os.MkdirAll(filepath.Join(userAbs, "d1", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "d1", "a.txt"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "d1", "sub", "b.txt"), []byte("bbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	return userAbs
}

// TestRenameDir_MovesSubtree 目录重命名必须搬整棵子树，且**不需要 checksum**。
func TestRenameDir_MovesSubtree(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	userAbs := mkTree(t, env)

	res, err := env.svc.RenameFile(context.Background(), RenameFileInput{
		Owner: "alice", From: "d1", To: "d2",
	})
	if err != nil {
		t.Fatalf("目录重命名失败: %v", err)
	}
	if res.From != "d1" || res.To != "d2" {
		t.Fatalf("结果 = %+v", res)
	}
	if _, err := os.Stat(filepath.Join(userAbs, "d1")); !os.IsNotExist(err) {
		t.Fatalf("源目录应消失: %v", err)
	}
	for _, rel := range []string{"d2/a.txt", "d2/sub/b.txt"} {
		if _, err := os.Stat(filepath.Join(userAbs, rel)); err != nil {
			t.Fatalf("目标文件缺失 %s: %v", rel, err)
		}
	}
}

// TestRenameDir_RejectsExistingTarget 目标目录已存在 → 409（不静默合并/覆盖）。
func TestRenameDir_RejectsExistingTarget(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	userAbs := mkTree(t, env)
	if err := os.MkdirAll(filepath.Join(userAbs, "d2"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := env.svc.RenameFile(context.Background(), RenameFileInput{
		Owner: "alice", From: "d1", To: "d2",
	})
	if err == nil {
		t.Fatal("目标已存在应报错")
	}
	he := asHTTPError(err)
	if he == nil || he.Status != 409 {
		t.Fatalf("期望 409，实际 %v", err)
	}
	// 源仍在
	if _, serr := os.Stat(filepath.Join(userAbs, "d1", "a.txt")); serr != nil {
		t.Fatalf("源目录不应被破坏: %v", serr)
	}
}

// TestRenameDir_MovesMetaSidecarSubtree 可信卷：目录重命名必须整棵搬 meta 子树，
// 否则改名后所有文件的 sidecar 成为孤儿（校验/元数据全部丢失）。
func TestRenameDir_MovesMetaSidecarSubtree(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.fileMeta = true
	env.enableWriteDefaults()
	ctx := context.Background()
	tnt := env.tenantFor("alice")
	userAbs, _ := tnt.Root().Abs("user")
	metaAbs, _ := tnt.Root().Abs("meta")

	// 用真实写路径建目录与文件（会建 sidecar）。
	if _, err := env.svc.MakeDir(ctx, "alice", "d1", ""); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if _, err := env.svc.WriteFile(ctx, WriteFileInput{
		Owner: "alice", RemotePath: "d1/a.txt", ExpectedChecksum: testutil.SHA256Hex([]byte("aaa")), ClientSize: 3,
	}, strings.NewReader("aaa")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(metaAbs, "d1", "a.txt.meta")); err != nil {
		t.Fatalf("前置：sidecar 应存在: %v", err)
	}

	if _, err := env.svc.RenameFile(ctx, RenameFileInput{Owner: "alice", From: "d1", To: "d2"}); err != nil {
		t.Fatalf("目录重命名: %v", err)
	}
	if _, err := os.Stat(filepath.Join(metaAbs, "d2", "a.txt.meta")); err != nil {
		t.Fatalf("meta 子树未随目录迁移: %v", err)
	}
	if _, err := os.Stat(filepath.Join(metaAbs, "d1")); !os.IsNotExist(err) {
		t.Fatalf("旧 meta 子树应消失: %v", err)
	}
	_ = userAbs
}

// TestRenameDir_RewritesChecksumLedgerPrefix 目录重命名后，子树内文件的 checksum 台账键
// 必须同步改名（否则列表 checksum 丢失 ⇒ 后续删除/分享全部失败）。
func TestRenameDir_RewritesChecksumLedgerPrefix(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.fileMeta = true
	env.enableWriteDefaults()
	ctx := context.Background()
	if _, err := env.svc.MakeDir(ctx, "alice", "d1", ""); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	sum := testutil.SHA256Hex([]byte("aaa"))
	if _, err := env.svc.WriteFile(ctx, WriteFileInput{
		Owner: "alice", RemotePath: "d1/a.txt", ExpectedChecksum: sum, ClientSize: 3,
	}, strings.NewReader("aaa")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := env.svc.RenameFile(ctx, RenameFileInput{Owner: "alice", From: "d1", To: "d2"}); err != nil {
		t.Fatalf("目录重命名: %v", err)
	}
	// 列表里 d2/a.txt 必须带 checksum（台账键已改名）。
	res, err := env.svc.List(ctx, ListQuery{Owner: "alice", Subdir: "d2"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, f := range res.Files {
		if f.Name == "a.txt" {
			if f.Checksum != sum {
				t.Fatalf("改名后 checksum = %q，期望 %q（台账键未跟随）", f.Checksum, sum)
			}
			return
		}
	}
	t.Fatal("d2 下未找到 a.txt")
}
