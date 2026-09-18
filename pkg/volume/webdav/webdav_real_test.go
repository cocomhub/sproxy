// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav

// webdav_real_test.go 钉住 WebDAV 后端与**真实 WebDAV 服务端**（hacdias/webdav 容器）的
// 协议兼容性（T2，2026-09-18）：
//   - CI 的 Test (ubuntu, +Vault) job 启动 hacdias/webdav 容器（匿名模式，端口 8080），
//     本测试检测 WEBDAV_ENDPOINT（默认 http://127.0.0.1:8080）可达 → 不可达 t.Skip
//     （本地无容器自动跳过，与 vault/MinIO 集成测试同模式）；
//   - 验证真实服务协议往返：PROPFIND/GET/PUT/MOVE/DELETE/MKCOL + sync 引擎 push/pull
//     + 子目录递归 + 404 幂等——补 httptest fake 之外的**真实协议实现**兼容面。
//
// 认证（Basic/Bearer）兼容性由 webdav_fs_test.go 的 httptest fake 覆盖（头发送正确性），
// 本测试用匿名模式（hacdias/webdav 容器无需 users 配置即可验证协议往返）。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// realWebDAVEnv 返回真实 WebDAV 服务端配置（WEBDAV_ENDPOINT 默认 127.0.0.1:8080）。
func realWebDAVEnv() ClientConfig {
	ep := os.Getenv("WEBDAV_ENDPOINT")
	if ep == "" {
		ep = "http://127.0.0.1:8080"
	}
	return ClientConfig{RootURL: ep}
}

// requireRealWebDAV 探测真实服务可达性——不可达 t.Skip（本地无容器自动跳过）。
func requireRealWebDAV(t *testing.T) {
	t.Helper()
	cfg := realWebDAVEnv()
	client, err := NewWebDAVFS(cfg)
	if err != nil {
		t.Skipf("WebDAVFS 构造失败（%v）——跳过真实服务集成测试", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = client.ListDir(ctx, "")
	if err != nil {
		t.Skipf("WebDAV 服务不可达（%v）——跳过真实服务集成测试（docker/CI webdav service 存在时自动实跑）", err)
	}
}

// TestWebDAVReal_SyncEngine_PushPull 真实服务端到端：push（本地 → 真实 WebDAV）→ pull 往返。
// 与 httptest fake 用例同构（TestWebDAVE2E_SyncEngine_PushPull），但打在真实协议实现上。
func TestWebDAVReal_SyncEngine_PushPull(t *testing.T) {
	// 真实服务集成测试不并发（共享容器服务端；不 t.Parallel）。
	// sproxy:serial: 真实容器单实例——并行用例共享服务端会互相干扰
	requireRealWebDAV(t)
	cfg := realWebDAVEnv()
	fs, err := NewWebDAVFS(cfg)
	if err != nil {
		t.Fatalf("NewWebDAVFS: %v", err)
	}
	defer fs.Close()

	localRoot := t.TempDir()
	// push：本地 a.txt + sub/b.txt → 真实 WebDAV。
	e2eWriteLocal(t, localRoot, "a.txt", "real webdav a")
	e2eWriteLocal(t, localRoot, "sub/b.txt", "real webdav b")
	job := &syncpkg.Job{Direction: syncpkg.DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: syncpkg.ConflictSkip}
	engine := &syncpkg.Engine{Concurrency: 2}
	if err := engine.Sync(context.Background(), syncpkg.NewLocalFS(localRoot, nil), fs, job); err != nil {
		t.Fatalf("push Sync: %v", err)
	}
	if job.Status != syncpkg.StatusCompleted || job.Stats.FilesDone != 2 {
		t.Fatalf("push 统计不符: status=%s total=%d done=%d, want completed 2/2", job.Status, job.Stats.FilesTotal, job.Stats.FilesDone)
	}

	// pull：真实 WebDAV → 本地（新目录），内容往返一致。
	pullRoot := t.TempDir()
	job2 := &syncpkg.Job{Direction: syncpkg.DirectionPull, Src: "", Dst: "", Recursive: true, ConflictPolicy: syncpkg.ConflictSkip}
	engine2 := &syncpkg.Engine{Concurrency: 2}
	if err := engine2.Sync(context.Background(), fs, syncpkg.NewLocalFS(pullRoot, nil), job2); err != nil {
		t.Fatalf("pull Sync: %v", err)
	}
	if job2.Status != syncpkg.StatusCompleted || job2.Stats.FilesDone != 2 {
		t.Fatalf("pull 统计不符: status=%s done=%d, want completed 2/2", job2.Status, job2.Stats.FilesDone)
	}
	if got := e2eReadLocal(t, pullRoot, "a.txt"); got != "real webdav a" {
		t.Fatalf("pull a.txt = %q, want 'real webdav a'", got)
	}
	if got := e2eReadLocal(t, pullRoot, "sub/b.txt"); got != "real webdav b" {
		t.Fatalf("pull sub/b.txt = %q, want 'real webdav b'", got)
	}

	// Delete 幂等：删 a.txt（真实 DELETE）+ 再删（404 幂等 nil）。
	if err := fs.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete a.txt: %v", err)
	}
	if err := fs.Delete(context.Background(), "a.txt"); err != nil {
		t.Fatalf("Delete a.txt 二次（404 幂等）: %v", err)
	}
}

// TestWebDAVReal_MakeDirRoundtrip 真实服务 MKCOL + Stat（目录占位）往返。
func TestWebDAVReal_MakeDirRoundtrip(t *testing.T) {
	requireRealWebDAV(t)
	cfg := realWebDAVEnv()
	fs, err := NewWebDAVFS(cfg)
	if err != nil {
		t.Fatalf("NewWebDAVFS: %v", err)
	}
	defer fs.Close()

	ctx := context.Background()
	if err := fs.MakeDir(ctx, "realdir"); err != nil {
		t.Fatalf("MakeDir realdir: %v", err)
	}
	e, err := fs.Stat(ctx, "realdir")
	if err != nil || e == nil || !e.IsDir {
		t.Fatalf("Stat realdir = %v (err %v), want dir", e, err)
	}
	// 已存在 MKCOL → 幂等 nil（hacdias/webdav 405 → WebDAVFS 幂等处理）。
	if err := fs.MakeDir(ctx, "realdir"); err != nil {
		t.Fatalf("MakeDir realdir 二次（幂等）: %v", err)
	}
	// Rename（MOVE）：realdir → realdir2。
	if err := fs.Rename(ctx, "realdir", "realdir2"); err != nil {
		t.Fatalf("Rename realdir: %v", err)
	}
	e2, err := fs.Stat(ctx, "realdir2")
	if err != nil || e2 == nil || !e2.IsDir {
		t.Fatalf("Stat realdir2 = %v (err %v), want dir", e2, err)
	}
}

// TestWebDAVReal_ListDir_CompletePath 真实服务 ListDir 返回**完整相对路径**（引擎递归依赖）。
func TestWebDAVReal_ListDir_CompletePath(t *testing.T) {
	requireRealWebDAV(t)
	cfg := realWebDAVEnv()
	fs, err := NewWebDAVFS(cfg)
	if err != nil {
		t.Fatalf("NewWebDAVFS: %v", err)
	}
	defer fs.Close()

	ctx := context.Background()
	// 预置嵌套：realroot/a.txt + realroot/sub/b.txt（真实 MKCOL + PUT）。
	if err := fs.MakeDir(ctx, "realroot"); err != nil {
		t.Fatalf("MakeDir realroot: %v", err)
	}
	if err := fs.MakeDir(ctx, "realroot/sub"); err != nil {
		t.Fatalf("MakeDir realroot/sub: %v", err)
	}
	if err := fs.WriteFile(ctx, "realroot/a.txt", strings.NewReader("A"), 1, 0); err != nil {
		t.Fatalf("WriteFile realroot/a.txt: %v", err)
	}
	if err := fs.WriteFile(ctx, "realroot/sub/b.txt", strings.NewReader("B"), 1, 0); err != nil {
		t.Fatalf("WriteFile realroot/sub/b.txt: %v", err)
	}
	entries, err := fs.ListDir(ctx, "realroot")
	if err != nil {
		t.Fatalf("ListDir realroot: %v", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		if !strings.Contains(e.Path, "/") {
			t.Errorf("ListDir 条目 %q 缺完整相对路径（应有 realroot/ 前缀）", e.Path)
		}
		found[e.Path] = true
	}
	if !found["realroot/a.txt"] || !found["realroot/sub"] {
		t.Fatalf("ListDir 缺条目: got %v", found)
	}
}
