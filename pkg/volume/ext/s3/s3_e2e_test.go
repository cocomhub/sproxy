// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/minio/minio-go/v7"
)

// s3_e2e_test.go 是 S3 后端端到端验证：MinIO 容器（S3_ENDPOINT 默认 127.0.0.1:9000）
// 可达时实跑 sync 引擎 push/pull 往返；不可达 t.Skip（本地无 MinIO / CI 非 MinIO job
// 自动跳过，不失败——仿 vault_integration_test.go 模式）。
//
// 与 webdav_e2e_test.go 同构：S3FS 直接作为 sync.FS，验证 FS 语义 + 引擎编排 +
// 子目录递归 + 内容往返 + 统计。

// e2eWriteLocal 在本地根下写测试文件（自动建父目录）。
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

// e2eReadLocal 读取本地文件内容（断言往返一致用）。
func e2eReadLocal(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读 %s: %v", rel, err)
	}
	return string(data)
}

// e2eNewBucket 建立唯一测试桶（e2e 隔离：每次测试独立桶名，避免并发冲突）。
func e2eNewBucket(t *testing.T, fs *S3FS) {
	t.Helper()
	exists, err := fs.client.BucketExists(context.Background(), fs.bucket)
	if err != nil {
		t.Fatalf("BucketExists(%s): %v", fs.bucket, err)
	}
	if exists {
		return // 桶已存在（复用）
	}
	if err := fs.client.MakeBucket(context.Background(), fs.bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket(%s): %v", fs.bucket, err)
	}
}

// TestS3E2E_SyncEngine_PushPull 全链路：本地 → S3 push → S3 → 本地 pull（子目录递归 + 内容往返 + 统计）。
func TestS3E2E_SyncEngine_PushPull(t *testing.T) {
	t.Parallel()
	requireS3(t)
	ctx := context.Background()

	cfg := s3Env()
	cfg.Bucket = "s3e2e-" + randSuffix()
	fs, err := NewS3FS(cfg)
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	e2eNewBucket(t, fs)

	// ---- push：本地 → S3 ----
	srcRoot := t.TempDir()
	e2eWriteLocal(t, srcRoot, "a.txt", "hello s3")
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

	// ---- pull：S3 → 本地 ----
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

	// 内容往返一致（含子目录）。
	if got := e2eReadLocal(t, dstRoot, "a.txt"); got != "hello s3" {
		t.Fatalf("本地 a.txt = %q, want %q", got, "hello s3")
	}
	if got := e2eReadLocal(t, dstRoot, "sub/b.txt"); got != "nested world" {
		t.Fatalf("本地 sub/b.txt = %q, want %q", got, "nested world")
	}
}

// TestS3E2E_SyncEngine_MakeDirRoundtrip 验证 MakeDir（占位对象）+ Stat 目录探测往返。
func TestS3E2E_SyncEngine_MakeDirRoundtrip(t *testing.T) {
	t.Parallel()
	requireS3(t)
	ctx := context.Background()

	cfg := s3Env()
	cfg.Bucket = "s3e2e-" + randSuffix()
	fs, err := NewS3FS(cfg)
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	e2eNewBucket(t, fs)

	if mkErr := fs.MakeDir(ctx, "emptydir"); mkErr != nil {
		t.Fatalf("MakeDir: %v", mkErr)
	}
	e, err := fs.Stat(ctx, "emptydir")
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if e == nil || !e.IsDir {
		t.Fatalf("Stat(emptydir) = %+v, want dir entry", e)
	}
	// 占位对象存在（key = emptydir/）。
	obj, err := fs.client.StatObject(ctx, fs.bucket, "emptydir/", minio.StatObjectOptions{})
	if err != nil {
		t.Fatalf("占位对象 StatObject: %v", err)
	}
	if obj.Size != 0 {
		t.Fatalf("占位对象 size = %d, want 0", obj.Size)
	}
}
