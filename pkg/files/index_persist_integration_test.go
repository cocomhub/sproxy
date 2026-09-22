// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// index_persist_integration_test.go 验证快照载入集成（roadmap 2.3 P0）：
// Service A 搜索构建索引 + 保存快照 → 重建 Service B（同存储根）→ 搜索仍命中
// （载入快照免全量 WalkDir，且结果与磁盘一致）。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestService_IndexSnapshotReload 快照落盘 → 重建 Service → 搜索命中。
func TestService_IndexSnapshotReload(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)

	// 上传文件到 alice/user/dir/a.txt。
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	sub := filepath.Join(userAbs, "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Service A：搜索触发索引构建。
	env.rebuild()
	s1 := env.newService()
	if n, err := s1.Search(SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(n.Files) != 1 {
		t.Fatalf("搜索命中数 = %d err=%v, want 1（构建索引）", len(n.Files), err)
	}
	// 保存快照。
	if n := s1.SaveIndexSnapshots(); n != 1 {
		t.Fatalf("保存快照数 = %d, want 1", n)
	}

	// 快照文件存在。
	tnt := env.tenantFor("alice")
	p, _ := tnt.Root().Abs("meta/index/alice.json")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("快照文件应存在: %v", err)
	}

	// 记录快照文件 mtime（区分载入 vs 重建：载入不覆盖，重建会覆盖同内容改 mtime）。
	snapInfo, serr := os.Stat(p)
	if serr != nil {
		t.Fatalf("Stat 快照: %v", serr)
	}
	beforeMtime := snapInfo.ModTime()

	// Service B（新实例）：搜索命中（载入快照，无需全量 WalkDir）。
	s2 := env.newService()
	if n, err := s2.Search(SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(n.Files) != 1 {
		t.Fatalf("重建后搜索命中数 = %d err=%v, want 1（快照载入）", len(n.Files), err)
	}
	// 载入路径不重建 → 快照文件 mtime 不变（变异：不载入 → 重建覆盖 → mtime 变 → 红）。
	snapInfo2, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat 快照 #2: %v", err)
	}
	if !snapInfo2.ModTime().Equal(beforeMtime) {
		t.Fatalf("快照 mtime 变化 = 未载入（重建覆盖）, before=%v after=%v", beforeMtime, snapInfo2.ModTime())
	}
	if n := s2.SaveIndexSnapshots(); n != 1 {
		t.Fatalf("重建后保存快照数 = %d, want 1", n)
	}
}
