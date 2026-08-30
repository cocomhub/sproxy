// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"context"
	"io"
	"log/slog"
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
	return syncmgr.RemoteConfig{Name: "r1", URL: srvURL, AccessKey: "test-ak", AccessKeySecret: strings.Repeat("a", 64)}
}

func TestExecutor_Push(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	dir := t.TempDir()
	writeLocalFile(t, dir, "a.txt", "hello push")
	exec := NewExecutor(dir, discardLogger())

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
	dir := t.TempDir()
	exec := NewExecutor(dir, discardLogger())

	task := &syncmgr.SyncTask{ID: "t1", Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true, ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed，got %q", res.Status)
	}
	if got := readLocalFile(t, dir, "local/r.txt"); got != "remote content" {
		t.Fatalf("本地内容不符: %q", got)
	}
}

func TestExecutor_Push_SameChecksum_Skipped(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	dir := t.TempDir()
	writeLocalFile(t, dir, "same.txt", "identical")
	remote.SeedFile("same.txt", "identical")
	exec := NewExecutor(dir, discardLogger())

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
	dir := t.TempDir()
	exec := NewExecutor(dir, discardLogger())

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
	dir := t.TempDir()
	exec := NewExecutor(dir, discardLogger())
	task := &syncmgr.SyncTask{ID: "t1", Direction: "push", Remote: "r1", Src: "", Dst: "", ConflictPolicy: "skip"}
	rc := syncmgr.RemoteConfig{Name: "r1", URL: "not-a-url", AccessKey: "ak", AccessKeySecret: strings.Repeat("a", 64)}
	if _, err := exec.Run(context.Background(), task, rc); err == nil {
		t.Fatal("非法 remote URL 应报错")
	}
}
