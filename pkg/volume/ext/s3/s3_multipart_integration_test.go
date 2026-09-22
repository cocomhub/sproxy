// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_multipart_integration_test.go 是分片上传的 MinIO 容器集成测试
// （S3_ENDPOINT 默认 127.0.0.1:9000 可达才实跑；不可达 t.Skip——同 s3_fs_integration_test.go 模式）。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"
)

// newTestMultipartS3FS 构造走分片参数（小阈值便于测试大文件 multipart）的 S3FS。
func newTestMultipartS3FS(t *testing.T, threshold int64) *S3FS {
	t.Helper()
	requireS3(t) // MinIO 不可达 → t.Skip（本地无容器/CI 非 MinIO job 自动跳过）
	cfg := s3Env()
	cfg.MultipartThreshold = threshold
	cfg.MultipartPartSize = minMultipartPartSize // 最小分片（5MiB）减少测试数据量
	fs, err := NewS3FS(cfg)
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	exists, err := fs.client.BucketExists(context.Background(), cfg.Bucket)
	if err != nil {
		t.Fatalf("BucketExists: %v", err)
	}
	if !exists {
		if err := fs.client.MakeBucket(context.Background(), cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatalf("MakeBucket: %v", err)
		}
	}
	return fs
}

// TestS3FS_Multipart_LargeFile 验证大文件（>=阈值）走 multipart：上传完成 + 对象可读。
func TestS3FS_Multipart_LargeFile(t *testing.T) {
	t.Parallel()
	fs := newTestMultipartS3FS(t, 1<<20) // 阈值 1MiB：>1MiB 内容走分片
	ctx := context.Background()
	// 3MiB 内容（> 阈值；分片 5MiB 下限 → minio 仍自动 multipart）。
	content := bytes.Repeat([]byte("x"), 3<<20)
	if err := fs.WriteFile(ctx, "mp/big.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(大文件): %v", err)
	}
	rc, err := fs.OpenRead(ctx, "mp/big.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("大文件内容不符: got %d bytes, want %d", len(got), len(content))
	}
}

// TestS3FS_Multipart_SmallFileZeroRegression 验证小文件（<阈值）仍单 PutObject 零回归。
func TestS3FS_Multipart_SmallFileZeroRegression(t *testing.T) {
	t.Parallel()
	fs := newTestS3FS(t) // 默认阈值 64MiB：小文件单 PutObject
	ctx := context.Background()
	content := []byte("small file")
	if err := fs.WriteFile(ctx, "small/x.txt", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(小文件): %v", err)
	}
	rc, err := fs.OpenRead(ctx, "small/x.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("小文件内容不符: got %q, want %q", got, content)
	}
}

// TestS3FS_Multipart_Retry 验证 PutObject 失败重试：注入失败后重试成功。
func TestS3FS_Multipart_Retry(t *testing.T) {
	t.Parallel()
	fs := newTestMultipartS3FS(t, 1<<20)
	ctx := context.Background()
	content := bytes.Repeat([]byte("y"), 2<<20)

	// 注入失败：第一次 PutObject 报错，重试成功（退避重试语义）。
	orig := fs.putter
	attempt := 0
	fs.putter = &failingPutter{inner: orig, failTimes: 1, attempt: &attempt}
	defer func() { fs.putter = orig }()

	if err := fs.WriteFile(ctx, "mp/retry.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(重试): %v", err)
	}
	if attempt < 2 {
		t.Fatalf("应至少尝试 2 次（1 失败 + 1 重试成功），实际 %d", attempt)
	}
}

// failingPutter 包装 putter：前 failTimes 次 PutObject 报错（重试测试注入）。
type failingPutter struct {
	inner     putter
	failTimes int
	attempt   *int
}

func (c *failingPutter) PutObject(ctx context.Context, bucket, object string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	*c.attempt++
	if *c.attempt <= c.failTimes {
		return minio.UploadInfo{}, fmt.Errorf("注入失败: attempt %d", *c.attempt)
	}
	return c.inner.PutObject(ctx, bucket, object, reader, size, opts)
}
