// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testReporter 记录上报（用于断言参数与次数）。
type testReporter struct {
	rels   []string
	copies []string
}

func (r *testReporter) ReportBlobMD5Pending(rel, localCopy string) {
	r.rels = append(r.rels, rel)
	r.copies = append(r.copies, localCopy)
}

// newFinishStagedStorage 造一个仅用于 finishStaged 的 Storage（不触网）。
func newFinishStagedStorage(t *testing.T, rep ReconcileReporter) *Storage {
	t.Helper()
	return &Storage{
		temp:      t.TempDir(),
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		reconcile: rep,
	}
}

// mkStage 造一个 stage-* 目录 + 副本文件，返回 stagedUpload。
func mkStage(t *testing.T, s *Storage, key string, content string) (*stagedUpload, string) {
	t.Helper()
	dir, err := os.MkdirTemp(s.temp, "stage-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	p := filepath.Join(dir, filepath.Base(key))
	if werr := os.WriteFile(p, []byte(content), 0o600); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	return &stagedUpload{tmpPath: p, md5: strings.Repeat("a", 32), blockMD5s: []string{strings.Repeat("a", 32)}}, dir
}

// TestFinishStaged_未置位时删除（零回归） 锁定既有行为：未标记保留 ⇒ 临时目录必须被删除。
func TestFinishStaged_未置位时删除(t *testing.T) {
	t.Parallel()
	rep := &testReporter{}
	s := newFinishStagedStorage(t, rep)
	st, dir := mkStage(t, s, "a/b.bin", "hello")
	s.finishStaged("a/b.bin", st)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("未置位时临时目录应被删除，仍存在: %v", err)
	}
	if len(rep.rels) != 0 {
		t.Fatalf("未置位时不得上报，actual=%v", rep.rels)
	}
}

// TestFinishStaged_置位时保留副本并上报 锁定 ④ 的核心：内容已在云端但 md5 未权威 ⇒
// **保留副本**（重传源）+ **上报事项**（rel/localCopy 必须逐参正确——错位会导致重传打错目标）。
func TestFinishStaged_置位时保留副本并上报(t *testing.T) {
	t.Parallel()
	rep := &testReporter{}
	s := newFinishStagedStorage(t, rep)
	st, dir := mkStage(t, s, "jav/a/b.bin", "payload")
	st.retainForReconcile = true
	s.finishStaged("jav/a/b.bin", st)

	// ① 上报参数正确
	if len(rep.rels) != 1 || rep.rels[0] != "jav/a/b.bin" {
		t.Fatalf("上报 rel 错误: %v", rep.rels)
	}
	copyPath := rep.copies[0]
	// ② 副本在 pending 根下且内容保留
	if !strings.HasPrefix(copyPath, s.pendingRoot()) {
		t.Fatalf("副本应在 pending 根下，actual=%s root=%s", copyPath, s.pendingRoot())
	}
	raw, err := os.ReadFile(copyPath)
	if err != nil || string(raw) != "payload" {
		t.Fatalf("副本内容应保留: err=%v raw=%q", err, raw)
	}
	// ③ 原 stage-* 目录已清理（不重复占盘）
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("原临时目录应被清理，仍存在: %v", err)
	}
	// ④ 启动兜底扫描能复原 key
	pend := s.PendingReconcile()
	if len(pend) != 1 || pend[0].Key != "jav/a/b.bin" || pend[0].LocalCopy != copyPath {
		t.Fatalf("PendingReconcile 复原错误: %+v", pend)
	}
	// ⑤ 终态删除可用，且**只删 pending 根下的目录**
	if rerr := s.RemovePendingReconcile(copyPath); rerr != nil {
		t.Fatalf("RemovePendingReconcile: %v", rerr)
	}
	if len(s.PendingReconcile()) != 0 {
		t.Fatalf("删除后不应再有待复查项")
	}
}

// TestFinishStaged_未注入上报口也要保留副本 锁定降级行为：reporter 为 nil ⇒
// **仍必须保留副本**（不丢重传源）+ 不 panic（仅少一次自动重试）。
func TestFinishStaged_未注入上报口也要保留副本(t *testing.T) {
	t.Parallel()
	s := newFinishStagedStorage(t, nil)
	st, _ := mkStage(t, s, "k.bin", "x")
	st.retainForReconcile = true
	s.finishStaged("k.bin", st)
	if len(s.PendingReconcile()) != 1 {
		t.Fatalf("未注入上报口时也必须保留副本（重传源不得丢失）")
	}
}

// TestRemovePendingReconcile_拒绝越界删除 锁定安全边界：只允许删 pending 根下的目录，
// 防路径穿越/误传导致删除任意目录。
func TestRemovePendingReconcile_拒绝越界删除(t *testing.T) {
	t.Parallel()
	s := newFinishStagedStorage(t, nil)
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := s.RemovePendingReconcile(filepath.Join(outside, "f.bin")); err == nil {
		t.Fatal("越界路径必须被拒绝")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("越界目录不得被删除: %v", err)
	}
}

// TestRetryBlobMD5Reconcile_收敛后清理副本 锁定重试动作的核心契约：
//   - 从**保留的本地副本**重传后复查；远端 md5 已权威（fake adapter 的 ETag=内容 md5）⇒ 返回 nil
//     且**清理保留副本**（终态，防副本堆积）；
//   - 参数缺失/副本不存在 ⇒ **返回错误**（转人工），不得静默成功；
//   - 必须走 `PutFromStagedPath`（只读副本）——若误用 `Put`，会在"未收敛"时**再保留一份副本**
//     ⇒ 副本无限增殖（本用例以"重试后 pending 数量不增加"间接锁定）。
func TestRetryBlobMD5Reconcile_收敛后清理副本(t *testing.T) {
	t.Parallel()
	ad := newFakeStorageAdapter()
	s := newTestStorage(t, ad)

	// 造一份"保留中"的副本（模拟上传后判定 md5 未权威时的保留动作）
	rep := &testReporter{}
	s.reconcile = rep
	st, _ := mkStage(t, s, "jav/x/y.bin", "retry-payload")
	st.retainForReconcile = true
	s.finishStaged("jav/x/y.bin", st)
	if len(s.PendingReconcile()) != 1 {
		t.Fatalf("前置：应有 1 个保留副本")
	}
	copyPath := rep.copies[0]

	// 重试：fake adapter 的 Meta 返回 ETag=内容 md5（权威）⇒ 收敛
	if err := s.RetryBlobMD5Reconcile(t.Context(), "jav/x/y.bin", copyPath); err != nil {
		t.Fatalf("收敛场景应返回 nil，得到 %v", err)
	}
	if n := len(s.PendingReconcile()); n != 0 {
		t.Fatalf("收敛后应清理保留副本，仍有 %d 个", n)
	}
}

// TestRetryBlobMD5Reconcile_缺参或副本丢失必须报错 锁定 fail-closed（绝静默成功）。
func TestRetryBlobMD5Reconcile_缺参或副本丢失必须报错(t *testing.T) {
	t.Parallel()
	s := newTestStorage(t, newFakeStorageAdapter())
	if err := s.RetryBlobMD5Reconcile(t.Context(), "", "/tmp/x"); err == nil {
		t.Error("缺 key 必须报错")
	}
	if err := s.RetryBlobMD5Reconcile(t.Context(), "k", ""); err == nil {
		t.Error("缺副本路径必须报错")
	}
	missing := filepath.Join(t.TempDir(), "not-exist.bin")
	if err := s.RetryBlobMD5Reconcile(t.Context(), "k", missing); err == nil {
		t.Error("副本不存在必须报错（无源可传 ⇒ 转人工）")
	}
}
