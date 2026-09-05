// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/testutil/syncmock"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestTenantRoot 构造测试用租户根解析器（与生产装配同路径语义）：
// <base>/<owner>/user 为 user 根（空 owner → anonymous）。
func newTestTenantRoot(base string) syncmgr.TenantRootResolver {
	return func(owner string) (string, string, bool) {
		if owner == "" {
			owner = "anonymous"
		}
		return filepath.Join(base, owner, "user"),
			filepath.Join(base, owner, "meta", "sync"), true
	}
}

// userRootFor 返回 owner 租户 user 根（空 owner → anonymous）。
func userRootFor(base, owner string) string {
	if owner == "" {
		owner = "anonymous"
	}
	return filepath.Join(base, owner, "user")
}

func writeLocalFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func readLocalFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取本地文件 %s 失败: %v", rel, err)
	}
	return string(data)
}

// remoteConfig 构造指向 mock 远程的 RemoteConfig（假凭据，mock 忽略 Authorization）。
func remoteConfig(srvURL string) syncmgr.RemoteConfig {
	return syncmgr.RemoteConfig{Name: "r1", URL: srvURL, AccessKey: "test-ak", AccessKeySecret: strings.Repeat("a", 64), AccessKeyID: "skey-test-remote"}
}

func TestExecutor_Push(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "hello push")

	task := &syncmgr.SyncTask{ID: "t1", Direction: "push", Remote: "r1", Src: "", Dst: "", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed，got %q", res.Status)
	}
	if res.FilesDone != 1 || res.FilesTotal != 1 {
		t.Fatalf("进度不符: %d/%d", res.FilesDone, res.FilesTotal)
	}
	if res.BytesDone != int64(len("hello push")) {
		t.Fatalf("BytesDone 应为 %d，got %d", len("hello push"), res.BytesDone)
	}
	files := remote.SnapshotFiles()
	f, ok := files["a.txt"]
	if !ok || string(f.Data) != "hello push" {
		t.Fatalf("远程应存在 a.txt 且内容正确: %+v", files)
	}
}

func TestExecutor_Pull(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	remote.SeedFile("sub/r.txt", "remote content")
	remote.SeedDir("sub")
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())

	task := &syncmgr.SyncTask{ID: "t1", Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true, ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed，got %q", res.Status)
	}
	if got := readLocalFile(t, userRootFor(base, ""), "local/r.txt"); got != "remote content" {
		t.Fatalf("本地内容不符: %q", got)
	}
}

// TestExecutor_OwnerIsolation 验证多租户隔离（审查 F1）：带 owner 的同步任务
// 本地文件根必须落在 <base>/<owner>/user 桶下，而非全局根。
func TestExecutor_OwnerIsolation(t *testing.T) {
	t.Run("Push_UsesOwnerUserRoot", func(t *testing.T) {
		srv, remote := syncmock.NewServer(t)
		base := t.TempDir()
		exec := NewExecutor(newTestTenantRoot(base), discardLogger())
		// 源文件放在 owner 租户 user 桶（<base>/ak-A/user/，与布局一致）
		writeLocalFile(t, userRootFor(base, "ak-A"), "a.txt", "hello owner push")

		task := &syncmgr.SyncTask{ID: "t1", Direction: "push", Remote: "r1", Src: "", Dst: "", Owner: "ak-A", ConflictPolicy: "skip"}
		res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "completed" {
			t.Fatalf("状态应为 completed，got %q", res.Status)
		}
		if res.FilesDone != 1 {
			t.Fatalf("应推送 1 个文件（owner user 根），got %d", res.FilesDone)
		}
		f, ok := remote.SnapshotFiles()["a.txt"]
		if !ok || string(f.Data) != "hello owner push" {
			t.Fatalf("远端应存在 a.txt 且内容正确: %+v", remote.SnapshotFiles())
		}
	})

	t.Run("Pull_WritesOwnerUserRoot", func(t *testing.T) {
		srv, remote := syncmock.NewServer(t)
		remote.SeedFile("sub/owner.txt", "owner content")
		remote.SeedDir("sub")
		base := t.TempDir()
		exec := NewExecutor(newTestTenantRoot(base), discardLogger())

		task := &syncmgr.SyncTask{ID: "t1", Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true, Owner: "ak-A", ConflictPolicy: "skip"}
		res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "completed" {
			t.Fatalf("状态应为 completed，got %q", res.Status)
		}
		// 文件必须落在 <base>/ak-A/user/local，而非 ak-A 根或全局根
		if got := readLocalFile(t, userRootFor(base, "ak-A"), "local/owner.txt"); got != "owner content" {
			t.Fatalf("owner user 桶下内容不符: %q", got)
		}
		if _, err := os.Stat(filepath.Join(base, "ak-A", "local", "owner.txt")); err == nil {
			t.Fatalf("文件不应落在 ak-A 根（非 user 桶）: 隔离失败")
		}
		if _, err := os.Stat(filepath.Join(base, "local", "owner.txt")); err == nil {
			t.Fatalf("文件不应落在全局根 local/owner.txt（隔离失败）")
		}
	})

	t.Run("EmptyOwner_UsesAnonymousTenant", func(t *testing.T) {
		srv, remote := syncmock.NewServer(t)
		remote.SeedFile("sub/g.txt", "global")
		remote.SeedDir("sub")
		base := t.TempDir()
		exec := NewExecutor(newTestTenantRoot(base), discardLogger())

		task := &syncmgr.SyncTask{ID: "t1", Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true, Owner: "", ConflictPolicy: "skip"}
		res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != "completed" {
			t.Fatalf("状态应为 completed，got %q", res.Status)
		}
		// 空 owner 落 anonymous 租户 user 桶（<base>/anonymous/user），不再回落全局根
		if got := readLocalFile(t, userRootFor(base, ""), "local/g.txt"); got != "global" {
			t.Fatalf("空 owner 应写 anonymous 租户 user 桶: %q", got)
		}
		if _, err := os.Stat(filepath.Join(base, "local", "g.txt")); err == nil {
			t.Fatalf("空 owner 不应写全局根 local/g.txt（隔离误判）")
		}
		if _, err := os.Stat(filepath.Join(base, "ak-A", "local", "g.txt")); err == nil {
			t.Fatalf("空 owner 不应产生 ak-A 子目录（隔离误判）")
		}
	})
}

func TestExecutor_Push_SameChecksum_Skipped(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "same.txt", "identical")
	remote.SeedFile("same.txt", "identical")

	task := &syncmgr.SyncTask{ID: "t1", Direction: "push", Remote: "r1", Src: "", Dst: "", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed，got %q", res.Status)
	}
	if res.FilesDone != 0 {
		t.Fatalf("checksum 相同的文件应跳过，got FilesDone=%d", res.FilesDone)
	}
	if len(res.Results) != 1 || res.Results[0].Action != "skipped" || res.Results[0].Path != "same.txt" {
		t.Fatalf("应记录 skipped 结果: %+v", res.Results)
	}
}

func TestExecutor_Push_SourceMissing(t *testing.T) {
	srv, _ := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())

	task := &syncmgr.SyncTask{ID: "t1", Direction: "push", Remote: "r1", Src: "nonexistent", Dst: "", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err == nil {
		t.Fatal("源路径不存在应返回 error")
	}
	if res == nil || res.Status != "failed" {
		t.Fatalf("应返回 failed 状态，got %+v", res)
	}
	if res.Error == "" {
		t.Fatal("failed 结果应携带错误文本")
	}
}

func TestExecutor_RemoteURL_Invalid(t *testing.T) {
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	task := &syncmgr.SyncTask{ID: "t1", Direction: "push", Remote: "r1", Src: "", Dst: "", ConflictPolicy: "skip"}
	rc := syncmgr.RemoteConfig{Name: "r1", URL: "not-a-url", AccessKey: "ak", AccessKeySecret: strings.Repeat("a", 64)}
	if _, err := exec.Run(context.Background(), task, rc); err == nil {
		t.Fatal("非法 remote URL 应报错")
	}
}

// TestExecutor_RetryableNetworkError 验证瞬时网络错误（连接被拒绝）被判别为可重试：
// RunResult.Retryable=true、Status=failed（阶段 6 自动重试的判别依据）。
func TestExecutor_RetryableNetworkError(t *testing.T) {
	// 监听后立即关闭端口 → 连接被拒绝（瞬时网络错误）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	task := &syncmgr.SyncTask{ID: "t1", Direction: string(syncmgr.DirectionPull), Remote: "r1", Src: "", Dst: "", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig("http://"+addr))
	if err == nil {
		t.Fatal("连接被拒绝应返回错误")
	}
	if res == nil {
		t.Fatal("应返回 RunResult（引擎产出 failed 状态）")
	}
	if res.Status != "failed" {
		t.Fatalf("连接失败状态应为 failed，got %q", res.Status)
	}
	if !res.Retryable {
		t.Fatalf("网络错误应标记 Retryable=true（连接拒绝=瞬时），got Retryable=%v error=%q", res.Retryable, res.Error)
	}
}

// TestExecutor_BusinessErrorNotRetryable 验证业务失败（源路径不存在）不被判为可重试：
// Retryable=false（确定性错误，重试不会成功）。
func TestExecutor_BusinessErrorNotRetryable(t *testing.T) {
	srv, _ := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	task := &syncmgr.SyncTask{ID: "t1", Direction: string(syncmgr.DirectionPush), Remote: "r1", Src: "nonexistent", Dst: "", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err == nil {
		t.Fatal("源路径不存在应返回错误")
	}
	if res == nil || res.Status != "failed" {
		t.Fatalf("应返回 failed 状态，got %+v", res)
	}
	if res.Retryable {
		t.Fatal("业务失败（源路径不存在）不应标记为可重试")
	}
}
