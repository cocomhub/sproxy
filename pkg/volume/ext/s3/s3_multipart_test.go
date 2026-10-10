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

// consumeFailPutter 每次调用先读尽 reader（记录字节数）再按脚本返回错误——模拟真实
// 网络在传输中段中断（区别于不消费 reader 的假失败）。
type consumeFailPutter struct {
	failFirst int
	attempt   int
	readBytes []int
}

func (p *consumeFailPutter) PutObject(_ context.Context, _, _ string, reader io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	n, _ := io.Copy(io.Discard, reader)
	p.attempt++
	p.readBytes = append(p.readBytes, int(n))
	if p.attempt <= p.failFirst {
		return minio.UploadInfo{}, context.DeadlineExceeded
	}
	return minio.UploadInfo{}, nil
}

// nonSeekReader 包装 reader 去掉 Seeker 能力（模拟 trusted.Wrap 的 io.TeeReader）。
type nonSeekReader struct{ r io.Reader }

func (n nonSeekReader) Read(p []byte) (int, error) { return n.r.Read(p) }

// TestPutObjectWithRetry_ResetsSeekableReader 钉住 P0-3：重试前必须 Seek(0)，
// 否则第二次尝试读到 0 字节 → minio 静默写空对象。
func TestPutObjectWithRetry_ResetsSeekableReader(t *testing.T) {
	t.Parallel()
	p := &consumeFailPutter{failFirst: 1}
	data := bytes.NewReader(bytes.Repeat([]byte("x"), 1000))
	if err := putObjectWithRetry(t.Context(), p, uploadParams{bucket: "b", key: "k", retries: 3}, data, 1000); err != nil {
		t.Fatalf("可 seek 源重试应成功: %v", err)
	}
	if p.attempt != 2 {
		t.Fatalf("应尝试 2 次, got %d", p.attempt)
	}
	if len(p.readBytes) != 2 || p.readBytes[0] != 1000 || p.readBytes[1] != 1000 {
		t.Fatalf("每次尝试都应读到完整 1000 字节（重试前 Seek(0)），got %v", p.readBytes)
	}
}

// TestPutObjectWithRetry_NonSeekableFailClosed 钉住 P0-3 另一半：不可 seek 源不得
// 复用已消费流重试（会静默写坏对象）；必须 fail-closed 返回错误且不重试成功。
func TestPutObjectWithRetry_NonSeekableFailClosed(t *testing.T) {
	t.Parallel()
	p := &consumeFailPutter{failFirst: 1}
	data := nonSeekReader{r: bytes.NewReader(bytes.Repeat([]byte("x"), 1000))}
	err := putObjectWithRetry(t.Context(), p, uploadParams{bucket: "b", key: "k", retries: 3}, data, 1000)
	if err == nil {
		t.Fatal("不可 seek 源重试必须 fail-closed 返回错误（不得静默成功）")
	}
	if p.attempt != 1 {
		t.Fatalf("不可 seek 源不应重试, got %d 次", p.attempt)
	}
}

// TestMultipartOptsFromConfig_DefaultThreshold 文档承诺 multipart_threshold 默认 64MiB：
// 0（未配置/非法）→ 默认；<0 = 显式禁用分片。
func TestMultipartOptsFromConfig_DefaultThreshold(t *testing.T) {
	t.Parallel()
	if got := multipartOptsFromConfig(ClientConfig{}).threshold; got != defaultMultipartThreshold {
		t.Fatalf("缺省 threshold = %d, want %d", got, defaultMultipartThreshold)
	}
	if got := multipartOptsFromConfig(ClientConfig{MultipartThreshold: -1}).threshold; got != -1 {
		t.Fatalf("负值应保留（显式禁用），got %d", got)
	}
	if got := multipartOptsFromConfig(ClientConfig{MultipartThreshold: 1 << 20}).threshold; got != 1<<20 {
		t.Fatalf("显式值应保留，got %d", got)
	}
}
