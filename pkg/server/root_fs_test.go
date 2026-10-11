// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestRootFS_EncryptedVolumeIsUserVisible Root 适配器必须让「用户可见明文」语义成立：
// 写=密文落盘、读=明文、Stat/ListDir 大小=明文长度。
// 变异：core 改回 raw os I/O → 密文断言红。
func TestRootFS_EncryptedVolumeIsUserVisible(t *testing.T) {
	t.Parallel()
	src := newEncRoot(t)
	top := syncpkg.NewPrefixFS(NewRootFS(src), "user/")
	plain := []byte("dav-plain-text")

	if err := top.WriteFile(context.Background(), "docs/a.bin", bytes.NewReader(plain), int64(len(plain)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	raw, rerr := os.ReadFile(src.AbsPath() + "/user/docs/a.bin")
	if rerr != nil {
		t.Fatalf("读原始文件: %v", rerr)
	}
	if bytes.Contains(raw, []byte("dav-plain-text")) {
		t.Fatal("加密卷写经适配器必须透明加密落盘（明文落盘 = at-rest 绕过）")
	}

	rc, oerr := top.OpenRead(context.Background(), "docs/a.bin")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, plain) {
		t.Fatalf("应读到明文, got %q", got)
	}

	e, serr := top.Stat(context.Background(), "docs/a.bin")
	if serr != nil || e == nil {
		t.Fatalf("Stat: e=%v err=%v", e, serr)
	}
	if e.Size != int64(len(plain)) {
		t.Fatalf("Stat 大小应为明文长度 %d, got %d", len(plain), e.Size)
	}
	entries, lerr := top.ListDir(context.Background(), "docs")
	if lerr != nil {
		t.Fatalf("ListDir: %v", lerr)
	}
	if len(entries) != 1 || entries[0].Size != int64(len(plain)) || entries[0].Path != "docs/a.bin" {
		t.Fatalf("ListDir 条目应为明文长度与完整相对路径, got %+v", entries)
	}
}

// TestRootFS_SealEnforced DAV 面此前用裸 LocalFS ⇒ 封印被绕过。经 Root 适配器后，
// 读写封印路径必须分别被拒（写 ErrSealed、读按不存在）。
// 变异：core 换成 os 直读 → 封印断言红。
func TestRootFS_SealEnforced(t *testing.T) {
	t.Parallel()
	rt, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.MkdirAll("user/private/vault", 0o755); err != nil {
		t.Fatal(err)
	}
	if serr := rt.SetSeals([]storage.SealRule{{Path: "user/private/vault", Wrapper: "vault"}}); serr != nil {
		t.Fatal(serr)
	}
	top := syncpkg.NewPrefixFS(NewRootFS(rt), "user/")

	werr := top.WriteFile(context.Background(), "private/vault/x.bin", bytes.NewReader([]byte("x")), 1, 0)
	if !errors.Is(werr, storage.ErrSealed) {
		t.Fatalf("DAV 写封印路径必须被拒(ErrSealed), got %v", werr)
	}
	if _, oerr := top.OpenRead(context.Background(), "private/vault/secret.bin"); !errors.Is(oerr, fs.ErrNotExist) {
		t.Fatalf("DAV 读封印路径应按不存在, got %v", oerr)
	}
	if e, serr := top.Stat(context.Background(), "private/vault"); serr != nil || e != nil {
		t.Fatalf("DAV Stat 封印路径应为 (nil,nil)（404 语义）, got e=%v err=%v", e, serr)
	}
	if derr := top.Delete(context.Background(), "private/vault/x.bin"); !errors.Is(derr, storage.ErrSealed) {
		t.Fatalf("DAV 删除封印路径必须被拒, got %v", derr)
	}
}

// TestRootFS_PlainZeroRegression 非加密卷：原样写读、Rename/Delete/MakeDir 正常。
func TestRootFS_PlainZeroRegression(t *testing.T) {
	t.Parallel()
	rt, err := storage.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	top := NewRootFS(rt)
	ctx := context.Background()
	data := []byte("plain")
	mtime := time.Now().UnixNano()
	if werr := top.WriteFile(ctx, "user/p.txt", bytes.NewReader(data), int64(len(data)), mtime); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if raw, _ := os.ReadFile(rt.AbsPath() + "/user/p.txt"); !bytes.Equal(raw, data) {
		t.Fatalf("非加密卷应原样落盘, got %q", raw)
	}
	if rerr := top.Rename(ctx, "user/p.txt", "user/q.txt"); rerr != nil {
		t.Fatalf("Rename: %v", rerr)
	}
	if e, serr := top.Stat(ctx, "user/p.txt"); serr != nil || e != nil {
		t.Fatalf("改名后旧路径应不存在, got e=%v err=%v", e, serr)
	}
	if derr := top.Delete(ctx, "user/q.txt"); derr != nil {
		t.Fatalf("Delete: %v", derr)
	}
	if merr := top.MakeDir(ctx, "user/newdir"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
}
