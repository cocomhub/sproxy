// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav

// webdav_e2e_test.go 钉住 WebDAV 后端与 pkg/sync 引擎的端到端往返（V5）：
// httptest 模拟 WebDAV 服务端（复用 fakeWebDAVServer）→ WebDAVFS（sync.FS）→
// sync 引擎 push（本地 → WebDAV）→ pull（WebDAV → 本地）→ 内容/统计/子目录递归断言。
//
// 最小验证面（控制者裁决）：WebDAVFS 直接作为 sync.FS（不经 assembleVolumes/kind=volume
// 全链路——FS + 引擎往返即验证核心；全链路装配由 baidupcs e2e 模式覆盖）。
// 真实服务（Nextcloud 等）兼容性留后续（httptest 语义等价）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// e2eWriteLocal 在本地根下写文件（自动建父目录）。
func e2eWriteLocal(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// e2eReadLocal 读本地文件内容。
func e2eReadLocal(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取本地 %s: %v", rel, err)
	}
	return string(data)
}

// TestWebDAVE2E_SyncEngine_PushPull 验证 WebDAV 后端 + sync 引擎端到端往返：
//   - push：本地（a.txt + sub/b.txt）→ WebDAV 服务端（文件出现 + 子目录递归）
//   - pull：WebDAV → 本地（内容往返一致）
//   - 统计：FilesTotal/Done 2/2 + 内容断言
func TestWebDAVE2E_SyncEngine_PushPull(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)
	ctx := context.Background()

	// ---- push：本地 → WebDAV ----
	srcRoot := t.TempDir()
	e2eWriteLocal(t, srcRoot, "a.txt", "hello webdav")
	e2eWriteLocal(t, srcRoot, "sub/b.txt", "nested world")
	job := &syncpkg.Job{Direction: syncpkg.DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: syncpkg.ConflictSkip}
	engine := &syncpkg.Engine{Concurrency: 2}
	if err := engine.Sync(ctx, syncpkg.NewLocalFS(srcRoot, nil), fs, job); err != nil {
		t.Fatalf("push Sync error: %v", err)
	}
	if job.Status != syncpkg.StatusCompleted {
		t.Fatalf("push Status = %q, want completed", job.Status)
	}
	if job.Stats.FilesTotal != 2 || job.Stats.FilesDone != 2 {
		t.Fatalf("push 统计不符: total=%d done=%d, want 2/2", job.Stats.FilesTotal, job.Stats.FilesDone)
	}
	// WebDAV 服务端出现文件（含子目录）。
	srv.mu.Lock()
	gotA := srv.files["/a.txt"]
	gotB := srv.files["/sub/b.txt"]
	srv.mu.Unlock()
	if gotA != "hello webdav" {
		t.Fatalf("WebDAV a.txt = %q, want %q", gotA, "hello webdav")
	}
	if gotB != "nested world" {
		t.Fatalf("WebDAV sub/b.txt = %q, want %q", gotB, "nested world")
	}

	// ---- pull：WebDAV → 本地 ----
	dstRoot := t.TempDir()
	job2 := &syncpkg.Job{Direction: syncpkg.DirectionPull, Src: "", Dst: "", Recursive: true, ConflictPolicy: syncpkg.ConflictSkip}
	if err := engine.Sync(ctx, fs, syncpkg.NewLocalFS(dstRoot, nil), job2); err != nil {
		t.Fatalf("pull Sync error: %v", err)
	}
	if job2.Status != syncpkg.StatusCompleted {
		t.Fatalf("pull Status = %q, want completed", job2.Status)
	}
	if job2.Stats.FilesTotal != 2 || job2.Stats.FilesDone != 2 {
		t.Fatalf("pull 统计不符: total=%d done=%d, want 2/2", job2.Stats.FilesTotal, job2.Stats.FilesDone)
	}
	// 本地落盘内容往返一致。
	if got := e2eReadLocal(t, dstRoot, "a.txt"); got != "hello webdav" {
		t.Fatalf("本地 a.txt = %q, want %q", got, "hello webdav")
	}
	if got := e2eReadLocal(t, dstRoot, "sub/b.txt"); got != "nested world" {
		t.Fatalf("本地 sub/b.txt = %q, want %q", got, "nested world")
	}
}

// TestWebDAVE2E_SyncEngine_DirCleanup 验证 WebDAV 后端删除语义：
// TestWebDAVE2E_SyncEngine_WithRootPrefix 验证带根路径前缀的 WebDAV（Nextcloud 风格
// /remote.php/webdav）同步往返——urlFor 路径拼接正确。
func TestWebDAVE2E_SyncEngine_WithRootPrefix(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.files["/"] = ""
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL+"/remote.php/webdav")
	ctx := context.Background()

	srcRoot := t.TempDir()
	e2eWriteLocal(t, srcRoot, "a.txt", "root prefix")
	job := &syncpkg.Job{Direction: syncpkg.DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: syncpkg.ConflictSkip}
	engine := &syncpkg.Engine{Concurrency: 2}
	if err := engine.Sync(ctx, syncpkg.NewLocalFS(srcRoot, nil), fs, job); err != nil {
		t.Fatalf("push: %v", err)
	}
	srv.mu.Lock()
	got, ok := srv.files["/remote.php/webdav/a.txt"]
	srv.mu.Unlock()
	if !ok || got != "root prefix" {
		t.Fatalf("根前缀下 a.txt = %q (exists=%v), want %q", got, ok, "root prefix")
	}
}

// TestWebDAVE2E_SyncEngine_Auth 验证带 Basic 认证的 WebDAV 同步（认证头贯穿全链路）。
func TestWebDAVE2E_SyncEngine_Auth(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.authOK = "alice:secret"
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL, WithBasicAuth("alice", "secret"))
	ctx := context.Background()

	srcRoot := t.TempDir()
	e2eWriteLocal(t, srcRoot, "a.txt", strings.Repeat("auth", 10))
	job := &syncpkg.Job{Direction: syncpkg.DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: syncpkg.ConflictSkip}
	engine := &syncpkg.Engine{Concurrency: 2}
	if err := engine.Sync(ctx, syncpkg.NewLocalFS(srcRoot, nil), fs, job); err != nil {
		t.Fatalf("push(auth): %v", err)
	}
	srv.mu.Lock()
	got, ok := srv.files["/a.txt"]
	srv.mu.Unlock()
	if !ok || got != strings.Repeat("auth", 10) {
		t.Fatalf("认证 push 后 a.txt = %q (exists=%v)", got, ok)
	}
}
