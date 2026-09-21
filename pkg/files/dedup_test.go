// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// dedup_test.go 是「内容寻址去重」的域级测试（roadmap §2 P1）：
//   - DedupStore 台账：checksum → 引用列表（vol + rel），原子落盘可重载；
//   - Service 上传：dedup.enabled 时同 owner 同卷同内容 → 硬链接 + 引用计数，配额只计首份；
//   - Service 删除：引用计数 >1 只摘引用（文件保留 + 配额不减），归零才真删 + 释放配额；
//   - 覆盖写 / 重命名 / owner 隔离 / 跨卷不合并 / 默认关闭零回归。

import (
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDedupStore_PersistAndLoad 验证 DedupStore 台账：Add 引用 + 查重 + 落盘重载后一致。
func TestDedupStore_PersistAndLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "dedup.json")
	ds := newDedupStore(path, testLogger())

	if !ds.Add("user/a.txt", "vol0", "abc123") {
		t.Fatal("首个引用 Add 应返回 true（新 checksum）")
	}
	if ds.Add("user/b.txt", "vol0", "abc123") {
		t.Fatal("同 checksum 第二引用 Add 应返回 false（已存在首份）")
	}
	rel, ok := ds.FirstRel("vol0", "abc123")
	if !ok || rel != "user/a.txt" {
		t.Fatalf("FirstRel(vol0, abc123)=%q,%v want user/a.txt,true", rel, ok)
	}
	if ds.RefCount("abc123") != 2 {
		t.Fatalf("RefCount(abc123)=%d want 2", ds.RefCount("abc123"))
	}

	// 落盘重载。
	ds2 := newDedupStore(path, testLogger())
	if ds2.RefCount("abc123") != 2 {
		t.Fatalf("重载后 RefCount(abc123)=%d want 2", ds2.RefCount("abc123"))
	}
	if _, ok := ds2.FirstRel("vol0", "abc123"); !ok {
		t.Fatal("重载后 FirstRel 应命中")
	}

	// 摘引用。
	ds2.RemoveRef("user/a.txt", "vol0", "abc123")
	if ds2.RefCount("abc123") != 1 {
		t.Fatalf("摘引用后 RefCount(abc123)=%d want 1", ds2.RefCount("abc123"))
	}
	ds2.RemoveRef("user/b.txt", "vol0", "abc123")
	if ds2.RefCount("abc123") != 0 {
		t.Fatalf("全部摘除后 RefCount(abc123)=%d want 0", ds2.RefCount("abc123"))
	}
	if _, ok := ds2.FirstRel("vol0", "abc123"); ok {
		t.Fatal("全部摘除后 FirstRel 不应命中")
	}
}

// TestDedupStore_Rename 验证重命名后台账引用路径更新。
func TestDedupStore_Rename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ds := newDedupStore(filepath.Join(dir, "dedup.json"), testLogger())
	ds.Add("user/a.txt", "vol0", "abc123")
	ds.Add("user/b.txt", "vol0", "abc123")
	ds.Rename("user/a.txt", "user/c.txt")
	if _, ok := ds.RelExists("user/c.txt", "vol0", "abc123"); !ok {
		t.Fatal("Rename 后 user/c.txt 应在引用列表")
	}
	if _, ok := ds.RelExists("user/a.txt", "vol0", "abc123"); ok {
		t.Fatal("Rename 后 user/a.txt 不应在引用列表")
	}
	if ds.RefCount("abc123") != 2 {
		t.Fatalf("Rename 后 RefCount=%d want 2", ds.RefCount("abc123"))
	}
}

// TestService_Upload_DedupCreatesHardlink 上传相同内容两个文件名 → 第二个硬链接（同 inode）+ 引用计数 + 配额只计一份。
func TestService_Upload_DedupCreatesHardlink(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()

	const body = "dedup-content"
	cs := sha256Hex([]byte(body))
	rr := env.upload(t, "alice", "a.txt", []byte(body), cs, 0)
	if rr.Code != 200 {
		t.Fatalf("首个上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = env.upload(t, "alice", "b.txt", []byte(body), cs, 0)
	if rr.Code != 200 {
		t.Fatalf("去重上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !sameInode(t, env, "alice", "user/a.txt", "alice", "user/b.txt") {
		t.Fatal("去重后两文件应为同一 inode（硬链接）")
	}
	if got := env.dedupStoreFor("alice").RefCount(cs); got != 2 {
		t.Fatalf("引用计数=%d want 2", got)
	}
	// 配额只计一份物理占用。
	size := int64(len(body))
	if got := env.quotaBucketRoot("alice", "user").Usage(); got != size {
		t.Fatalf("去重上传后 user 桶配额=%d want %d（只计首份）", got, size)
	}
}

// TestService_Delete_DedupRefcount 删除一个引用 → 文件仍存在 + 配额不减；删最后一个 → 真删 + 释放配额。
func TestService_Delete_DedupRefcount(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()

	const body = "dedup-del"
	cs := sha256Hex([]byte(body))
	env.upload(t, "alice", "a.txt", []byte(body), cs, 0)
	env.upload(t, "alice", "b.txt", []byte(body), cs, 0)
	size := int64(len(body))

	// 删第一个引用：文件应仍在（inode 有另一引用）+ 配额不减。
	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "a.txt", cs))
	if rr.Code != 200 {
		t.Fatalf("删除引用应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "b.txt")); err != nil {
		t.Fatalf("删一个引用后 b.txt 应仍在: %v", err)
	}
	if got := env.quotaBucketRoot("alice", "user").Usage(); got != size {
		t.Fatalf("删一个引用后配额=%d want %d（不减）", got, size)
	}
	if got := env.dedupStoreFor("alice").RefCount(cs); got != 1 {
		t.Fatalf("删一个引用后 RefCount=%d want 1", got)
	}

	// 删最后一个：真删 + 配额释放。
	rr = httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "b.txt", cs))
	if rr.Code != 200 {
		t.Fatalf("删最后引用应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("删最后引用后 b.txt 应消失: %v", err)
	}
	if got := env.quotaBucketRoot("alice", "user").Usage(); got != 0 {
		t.Fatalf("删最后引用后配额=%d want 0", got)
	}
}

// TestService_Upload_DedupDisabled_NoHardlink 默认关闭 → 两份独立文件 + 配额双计（零回归）。
func TestService_Upload_DedupDisabled_NoHardlink(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	const body = "dedup-off"
	cs := sha256Hex([]byte(body))
	env.upload(t, "alice", "a.txt", []byte(body), cs, 0)
	env.upload(t, "alice", "b.txt", []byte(body), cs, 0)
	if sameInode(t, env, "alice", "user/a.txt", "alice", "user/b.txt") {
		t.Fatal("dedup 关闭时两文件应为独立 inode（零回归）")
	}
	size := int64(len(body))
	if got := env.quotaBucketRoot("alice", "user").Usage(); got != 2*size {
		t.Fatalf("dedup 关闭时配额=%d want %d（双计）", got, 2*size)
	}
}

// TestService_Upload_DedupOverwrite 覆盖写同 rel → 旧引用移除，新内容重新计。
func TestService_Upload_DedupOverwrite(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()
	env.versioningEnabled = true // 覆盖写需 versioning 开启（与无 dedup 时同语义：同 rel checksum 不同 = 有意覆盖）

	body1 := []byte("content-one")
	body2 := []byte("content-two")
	cs1 := sha256Hex(body1)
	cs2 := sha256Hex(body2)
	env.upload(t, "alice", "a.txt", body1, cs1, 0)
	env.upload(t, "alice", "b.txt", body1, cs1, 0) // b 与 a 同内容
	// 覆盖 a.txt 为新内容。
	env.upload(t, "alice", "a.txt", body2, cs2, 0)
	if got := env.dedupStoreFor("alice").RefCount(cs1); got != 1 {
		t.Fatalf("覆盖后 cs1 引用计数=%d want 1（只剩 b.txt）", got)
	}
	if got := env.dedupStoreFor("alice").RefCount(cs2); got != 1 {
		t.Fatalf("覆盖后 cs2 引用计数=%d want 1（a.txt）", got)
	}
}

// TestService_DedupOwnerIsolation 不同 owner 不共享（alice/bob 同内容不同 inode）。
func TestService_DedupOwnerIsolation(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()

	const body = "owner-isolated"
	cs := sha256Hex([]byte(body))
	env.upload(t, "alice", "a.txt", []byte(body), cs, 0)
	env.upload(t, "bob", "a.txt", []byte(body), cs, 0)
	if sameInode(t, env, "alice", "user/a.txt", "bob", "user/a.txt") {
		t.Fatal("不同 owner 同内容不应共享 inode（隔离）")
	}
}

// TestService_Upload_DedupCrossVolume_NoMerge 跨卷同内容不合并（不同 inode）。
func TestService_Upload_DedupCrossVolume_NoMerge(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()
	env.enableVolumes(t, "main", "disk2")

	const body = "cross-vol"
	cs := sha256Hex([]byte(body))
	env.upload(t, "alice", "a.txt", []byte(body), cs, 0)
	env.upload(t, "alice", "b.txt", []byte(body), cs, 0)
	// 显式 volume=disk2 上传到第二卷。
	up := uploadReq(t, "alice", "b.txt", []byte(body), cs, 0)
	up.Form = url.Values{"volume": {"disk2"}}
	up.PostForm = url.Values{"volume": {"disk2"}}
	rr := httptest.NewRecorder()
	env.svc.Upload(rr, up)
	if rr.Code != 200 {
		t.Fatalf("disk2 上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if sameInode(t, env, "alice", "user/a.txt", "alice", "user/b.txt") {
		t.Fatal("跨卷同内容不应合并 inode（卷独立物理）")
	}
}

// sameInode 断言两个（owner, rel）是否同一 inode（硬链接判定）。rel 为相对用户桶路径（user/...）。
func sameInode(t *testing.T, env *dirsEnv, ownerA, relA, ownerB, relB string) bool {
	t.Helper()
	absA := filepath.Join(env.root, ownerA, filepath.FromSlash(strings.TrimPrefix(relA, ownerA+"/")))
	absB := filepath.Join(env.root, ownerB, filepath.FromSlash(strings.TrimPrefix(relB, ownerB+"/")))
	ia, err := os.Stat(absA)
	if err != nil {
		t.Fatalf("stat %s: %v", absA, err)
	}
	ib, err := os.Stat(absB)
	if err != nil {
		t.Fatalf("stat %s: %v", absB, err)
	}
	return os.SameFile(ia, ib)
}
