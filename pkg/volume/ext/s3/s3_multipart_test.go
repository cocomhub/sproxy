// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_multipart_test.go 是分片上传配置的纯逻辑单测（不依赖 MinIO 容器）：
// shouldMultipart 阈值判定 + normalizePartSize 分片大小归一。
// 协议级 multipart 集成见 s3_fs_integration_test.go（MinIO 容器可达才实跑）。

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/minio/minio-go/v7"
)

// TestShouldMultipart 验证大文件阈值判定（>= 阈值走 multipart；小文件单 PutObject）。
func TestShouldMultipart(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		size  int64
		thres int64
		want  bool
	}{
		{"零大小", 0, defaultMultipartThreshold, false},
		{"小文件低于阈值", 1024, defaultMultipartThreshold, false},
		{"等于阈值走分片", defaultMultipartThreshold, defaultMultipartThreshold, true},
		{"大文件超阈值", 2 * defaultMultipartThreshold, defaultMultipartThreshold, true},
		{"自定义阈值", 100, 50, true},
		{"零阈值禁用分片", 1 << 30, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldMultipart(tc.size, tc.thres); got != tc.want {
				t.Errorf("shouldMultipart(size=%d, thres=%d) = %v, want %v", tc.size, tc.thres, got, tc.want)
			}
		})
	}
}

// TestNormalizePartSize 验证分片大小归一（<=0 用默认；小于 minio 下限 5MiB 钳制）。
func TestNormalizePartSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   int64
		want int64
	}{
		{"零值默认", 0, defaultMultipartPartSize},
		{"负值默认", -1, defaultMultipartPartSize},
		{"正常值保留", 32 << 20, 32 << 20},
		{"小于 5MiB 下限钳制", 1 << 20, minMultipartPartSize},
		{"等于 5MiB 下限保留", minMultipartPartSize, minMultipartPartSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizePartSize(tc.in); got != tc.want {
				t.Errorf("normalizePartSize(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestRetryCount 验证重试次数归一（<=0 默认）。
func TestRetryCount(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"零值默认", 0, defaultUploadRetries},
		{"负值默认", -2, defaultUploadRetries},
		{"正常值保留", 5, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeRetries(tc.in); got != tc.want {
				t.Errorf("normalizeRetries(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestWriteFile_MultipartRouting 验证 WriteFile 按阈值路由：大文件走 multipart
// （PutObjectOptions.PartSize 透传），小文件单 PutObject（PartSize=0 零回归）。
// 用 mock putter 断言（不依赖 MinIO 容器）。
func TestWriteFile_MultipartRouting(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		size      int64
		threshold int64
		wantPart  uint64 // >0 = 应透传 PartSize（multipart）；0 = 单 PutObject
	}{
		{"大文件走 multipart", 128 << 20, 64 << 20, uint64(16 << 20)},
		{"等于阈值走 multipart", 64 << 20, 64 << 20, uint64(16 << 20)},
		{"小文件单 PutObject", 1024, 64 << 20, 0},
		{"零阈值禁用分片", 128 << 20, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingPutter{}
			fs := &S3FS{
				putter: rec,
				bucket: "b",
				prefix: "",
				cfg: ClientConfig{
					MultipartThreshold: tc.threshold,
					MultipartPartSize:  defaultMultipartPartSize,
					UploadRetries:      1,
				},
			}
			content := bytes.Repeat([]byte("z"), int(tc.size))
			if tc.size > 8<<20 { // 超大内容裁剪避免内存浪费（仅验证路由）
				content = content[:8<<20]
			}
			if err := fs.WriteFile(t.Context(), "x.bin", bytes.NewReader(content), tc.size, 0); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if len(rec.calls) != 1 {
				t.Fatalf("应恰好 1 次 PutObject，实际 %d", len(rec.calls))
			}
			if rec.calls[0].PartSize != tc.wantPart {
				t.Fatalf("PartSize = %d, want %d（%s）", rec.calls[0].PartSize, tc.wantPart, tc.name)
			}
		})
	}
}

// recordingPutter 记录每次 PutObject 的选项（断言路由用）。
type recordingPutter struct {
	calls []minio.PutObjectOptions
}

func (r *recordingPutter) PutObject(ctx context.Context, bucket, object string, reader io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	r.calls = append(r.calls, opts)
	return minio.UploadInfo{}, nil
}
