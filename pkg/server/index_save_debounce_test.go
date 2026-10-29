// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestIndexSaveLoop_DebouncePersistsDirty 钉住「写后 1s 内落盘」（审计 2026-10-29）：
// 周期保存默认 5m ⇒ 崩溃窗口过大；debounce 只落本次置脏的 owner。
//
// 变异：去掉 debounce 分支（只保留 periodic）⇒ 快照不更新 ⇒ 红。
func TestIndexSaveLoop_DebouncePersistsDirty(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	userDir := filepath.Join(root, "alice", "user")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}

	cfg := Default()
	cfg.StorageRoot = root
	cfg.Placement = "prefer-default"
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: root}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	svc := h.fileService()
	if svc == nil {
		t.Fatal("files.Service 未装配")
	}
	// 先构建索引（触发首次快照落盘）。
	if res, err := svc.Search(files.SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("构建索引: n=%d err=%v", len(res.Files), err)
	}
	snapPath := filepath.Join(root, "alice", "meta", "index", "alice.json")
	before, err := os.Stat(snapPath)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}

	h.indexSaveStop = make(chan struct{})
	go h.indexSaveLoopWithDebounce(time.Hour, 20*time.Millisecond)
	// 停止由 h.Close()（t.Cleanup 已注册）关闭 indexSaveStop —— 不要重复 close（会 panic）。

	// 写路径增量（内容写事件 → 索引 upsert → 置脏）。
	if err := os.WriteFile(filepath.Join(userDir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}
	svc.ApplyRootMutation(storage.Mutation{
		Op: storage.MutationWrite, Tag: "main", Rel: "alice/user/b.txt", Root: h.globalRoot,
	})

	ok := testutil.WaitForBool(5*time.Second, func() bool {
		after, serr := os.Stat(snapPath)
		return serr == nil && after.ModTime().After(before.ModTime())
	})
	if !ok {
		t.Fatal("写后 debounce 未落盘快照（崩溃窗口仍为周期 interval）")
	}
}
