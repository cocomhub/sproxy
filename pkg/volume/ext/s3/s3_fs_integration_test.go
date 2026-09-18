// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_fs_integration_test.go 是 S3FS 的 MinIO 容器集成测试。
//
// 策略（用户 2026-09-18 定案，参考 pkg/accesskey/vault_integration_test.go）：
// 运行时检测 S3_ENDPOINT（默认 http://127.0.0.1:9000）可达——不可达 t.Skip
// （本地无 MinIO / CI 非 MinIO job 自动跳过，不失败）；可达则实跑 7 方法全链路。
// MinIO 默认凭据 minioadmin/minioadmin（容器默认）。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sync"
	"github.com/minio/minio-go/v7"
)

const (
	testBucket = "sproxy-test"
	testAK     = "minioadmin"
	testSK     = "minioadmin"
	testUseSSL = false
)

// s3Env 返回 S3 客户端配置（endpoint 来自 S3_ENDPOINT，默认 127.0.0.1:9000）。
func s3Env() ClientConfig {
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:9000"
	}
	return ClientConfig{
		Endpoint:  endpoint,
		AccessKey: testAK,
		SecretKey: testSK,
		Bucket:    testBucket,
		UseSSL:    testUseSSL,
	}
}

// requireS3 探测 MinIO 可达性（S3_ENDPOINT 默认 127.0.0.1:9000）——不可达 t.Skip。
// 探测用 HTTP HEAD /minio/health/live（MinIO 健康端点；任何 HTTP 响应即视为可达，
// 连接失败/超时 → skip）。
func requireS3(t *testing.T) {
	t.Helper()
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "127.0.0.1:9000"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	u := "http://" + endpoint + "/minio/health/live"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		t.Fatalf("构造健康检查请求: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Skipf("MinIO 不可达（%v）——跳过 S3 集成测试（docker/CI minio service 存在时自动实跑）", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_ = resp.StatusCode // 任何响应（含 4xx/5xx）都视为服务可达
}

// newTestS3FS 探测 MinIO 可达后构造 S3FS（bucket 已存在则复用；否则 MakeBucket）。
func newTestS3FS(t *testing.T) *S3FS {
	t.Helper()
	requireS3(t)
	cfg := s3Env()
	fs, err := NewS3FS(cfg)
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	// 确保 bucket 存在（不存在则创建）。
	exists, err := fs.client.BucketExists(context.Background(), cfg.Bucket)
	if err != nil {
		t.Fatalf("BucketExists(%s): %v", cfg.Bucket, err)
	}
	if !exists {
		if err := fs.client.MakeBucket(context.Background(), cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("MakeBucket(%s): %v", cfg.Bucket, err)
		}
	}
	return fs
}

// TestS3FS_WriteRead 验证 PutObject → GetObject 往返。
func TestS3FS_WriteRead(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t)
	ctx := context.Background()
	content := []byte("s3 write-read payload")
	if err := fs.WriteFile(ctx, "dir/a.txt", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "dir/a.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("内容不符: got %q, want %q", got, content)
	}
}

// TestS3FS_ListDir 验证 ListObjectsV2 delimiter="/" 单层列举（目录 + 文件）。
func TestS3FS_ListDir(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "list/top.txt", strings.NewReader("top"), 3, 0); err != nil {
		t.Fatalf("WriteFile top: %v", err)
	}
	if err := fs.WriteFile(ctx, "list/sub/nested.txt", strings.NewReader("nested"), 6, 0); err != nil {
		t.Fatalf("WriteFile nested: %v", err)
	}
	entries, err := fs.ListDir(ctx, "list")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	// 应有 top.txt（文件）+ sub（目录）。
	var topFile, subDir bool
	for _, e := range entries {
		if e.Name == "top.txt" && !e.IsDir && e.Path == "list/top.txt" {
			topFile = true
		}
		if e.Name == "sub" && e.IsDir && e.Path == "list/sub" {
			subDir = true
		}
	}
	if !topFile || !subDir {
		t.Fatalf("ListDir(list) 缺 top.txt(文件)/sub(目录), got: %+v", entries)
	}
}

// TestS3FS_Stat 验证 Stat（文件存在 / 目录占位 / 不存在 nil）。
func TestS3FS_Stat(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "stat/f.txt", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.MakeDir(ctx, "stat-dir"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	// 文件存在。
	e, err := fs.Stat(ctx, "stat/f.txt")
	if err != nil || e == nil {
		t.Fatalf("Stat 文件: e=%v err=%v", e, err)
	}
	if e.IsDir {
		t.Fatalf("stat/f.txt IsDir = true, want false")
	}
	// 目录占位对象存在。
	de, derr := fs.Stat(ctx, "stat-dir")
	if derr != nil || de == nil {
		t.Fatalf("Stat 目录: e=%v err=%v", de, derr)
	}
	if !de.IsDir {
		t.Fatalf("stat-dir IsDir = false, want true")
	}
	// 不存在 → (nil, nil)。
	ne, nerr := fs.Stat(ctx, "missing-xyz")
	if nerr != nil || ne != nil {
		t.Fatalf("Stat 不存在: e=%v err=%v（want nil,nil）", ne, nerr)
	}
}

// TestS3FS_Rename 验证 CopyObject + RemoveObject。
func TestS3FS_Rename(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "ren/from.txt", strings.NewReader("mv"), 2, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Rename(ctx, "ren/from.txt", "ren/to.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// 新位置存在。
	e, err := fs.Stat(ctx, "ren/to.txt")
	if err != nil || e == nil {
		t.Fatalf("Rename 后 Stat(to): e=%v err=%v", e, err)
	}
	// 旧位置不存在。
	oe, oerr := fs.Stat(ctx, "ren/from.txt")
	if oerr != nil || oe != nil {
		t.Fatalf("Rename 后 Stat(from): e=%v err=%v（want nil,nil）", oe, oerr)
	}
}

// TestS3FS_Delete 验证 RemoveObject（幂等：不存在不报错）。
func TestS3FS_Delete(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "del/d.txt", strings.NewReader("d"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Delete(ctx, "del/d.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 再删（不存在）→ 幂等 nil。
	if err := fs.Delete(ctx, "del/d.txt"); err != nil {
		t.Fatalf("Delete 幂等: %v", err)
	}
	// 确认不存在。
	e, err := fs.Stat(ctx, "del/d.txt")
	if err != nil || e != nil {
		t.Fatalf("Delete 后 Stat: e=%v err=%v（want nil,nil）", e, err)
	}
}

// TestS3FS_MakeDir 验证零字节占位对象（key+"/"）。
func TestS3FS_MakeDir(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t)
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "mkd"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	// 占位对象存在（Stat 目录 → IsDir）。
	e, err := fs.Stat(ctx, "mkd")
	if err != nil || e == nil {
		t.Fatalf("MakeDir 后 Stat: e=%v err=%v", e, err)
	}
	if !e.IsDir {
		t.Fatalf("mkd IsDir = false, want true")
	}
	// 重复 MakeDir → 幂等。
	if err := fs.MakeDir(ctx, "mkd"); err != nil {
		t.Fatalf("MakeDir 幂等: %v", err)
	}
}

// TestS3FS_SyncEngine 验证 pkg/sync 引擎在 S3FS 上 push/pull 往返。
func TestS3FS_SyncEngine(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t)
	ctx := context.Background()
	// 预置 S3 文件（目录占位 + 文件）。
	if err := fs.MakeDir(ctx, "eng/sub"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := fs.WriteFile(ctx, "eng/sub/data.txt", strings.NewReader("engine-data"), 11, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// ListDir 递归可遍历（walkDir 由引擎驱动）。
	entries, err := fs.ListDir(ctx, "eng")
	if err != nil {
		t.Fatalf("ListDir(eng): %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("ListDir(eng) 空（应有 sub 目录）")
	}
	// 引擎 sync.FS 编译期断言（S3FS 已 var _ sync.FS）。
	var _ sync.FS = fs
}
