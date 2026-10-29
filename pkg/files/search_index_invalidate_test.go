// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_index_invalidate_test.go 钉住索引失效的**代际语义**（设计审计 2026-10-29）：
// 失效不能依赖「删除磁盘快照成功」——删除失败（权限/占用/崩溃窗口）时若仍载入旧快照，
// 用户会看到已删文件或搜不到新文件。实现改为「快照携带 (Gen, Epoch)，载入时与内存代际
// 比对」⇒ 失效后即使旧快照被复活，也必然重建。

import (
	"os"
	"path/filepath"
	"testing"
)

// indexSnapshotAbs 返回 owner 的索引快照绝对路径（测试直写以模拟「删除失败/快照复活」）。
func indexSnapshotAbs(t *testing.T, env *dirsEnv, owner string) string {
	t.Helper()
	abs, ok := env.tenantFor(owner).Root().Abs(indexSnapshotRel(owner))
	if !ok {
		t.Fatalf("Abs(%s) 失败", indexSnapshotRel(owner))
	}
	return abs
}

// TestSearchIndex_InvalidateRejectsResurrectedSnapshot：`InvalidateIndex` 后旧快照被复活
// （模拟删除失败），下次搜索必须**重建**而不是载入它。
//
// 变异：invalidate 不递增 per-owner 代际 ⇒ 载入复活快照 ⇒ b.txt 搜不到 ⇒ 红。
func TestSearchIndex_InvalidateRejectsResurrectedSnapshot(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir user: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	env.rebuild()
	svc := env.newService()
	if res, err := svc.Search(SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("构建索引后应命中 a.txt: n=%d err=%v", len(res.Files), err)
	}
	if n := svc.SaveIndexSnapshots(); n != 1 {
		t.Fatalf("保存快照数 = %d, want 1", n)
	}
	snapPath := indexSnapshotAbs(t, env, "alice")
	stale, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}

	// 进程外新增 b.txt（不经服务写路径 ⇒ 无事件、无增量）。
	if werr := os.WriteFile(filepath.Join(userAbs, "b.txt"), []byte("b"), 0o644); werr != nil {
		t.Fatalf("write b: %v", werr)
	}

	svc.InvalidateIndex("alice")
	// 模拟删除失败：失效后把旧快照写回。
	if rerr := os.WriteFile(snapPath, stale, 0o644); rerr != nil {
		t.Fatalf("resurrect snapshot: %v", rerr)
	}

	res, err := svc.Search(SearchQuery{Owner: "alice", Query: "b.txt"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "b.txt" {
		t.Fatalf("失效后必须重建（看到新文件），got %+v（旧快照被载入 = 失效未生效）", res.Files)
	}
}

// TestSearchIndex_InvalidateAllDeletesAllSnapshots：全局失效（`POST /api/volumes/refresh`
// 无参）必须**删除磁盘快照**（含本进程未知的 owner）——否则重启后首次搜索会载入陈旧快照。
//
// 变异：invalidateAll 不调 deleteAllSnapshots ⇒ 旧快照残留 ⇒ 新 Service 载入它 ⇒
// 进程外新增的 b.txt 搜不到 ⇒ 红。
func TestSearchIndex_InvalidateAllDeletesAllSnapshots(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir user: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}
	env.rebuild()
	svc := env.newService()
	if res, err := svc.Search(SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("构建索引后应命中 a.txt: n=%d err=%v", len(res.Files), err)
	}
	if n := svc.SaveIndexSnapshots(); n != 1 {
		t.Fatalf("保存快照数 = %d, want 1", n)
	}
	snapPath := indexSnapshotAbs(t, env, "alice")
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("前置失败：快照应存在: %v", err)
	}
	// 进程外新增 b.txt。
	if err := os.WriteFile(filepath.Join(userAbs, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	// 全局失效：清内存 + 删除该 owner 的磁盘快照（生产装配下 deleteAllSnapshots 还会枚举
	// 磁盘上本进程未知的 owner，见 pkg/server 的 refresh 集成用例）。
	svc.InvalidateAllIndexes()
	if _, err := os.Stat(snapPath); !os.IsNotExist(err) {
		t.Fatalf("全局失效必须删除磁盘快照（含本进程未知的 owner），stat err=%v", err)
	}

	// 新 Service（模拟重启：内存索引为空）。
	svc2 := env.newService()
	res, err := svc2.Search(SearchQuery{Owner: "alice", Query: "b.txt"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "b.txt" {
		t.Fatalf("全局失效后必须重建，got %+v（陈旧快照被载入）", res.Files)
	}
}
