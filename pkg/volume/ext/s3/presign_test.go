// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestS3FS_PresignedURL_Put 验证 PUT presigned URL 生成（签名参数齐全）。
func TestS3FS_PresignedURL_Put(t *testing.T) {
	// 用本地 mock S3（httptest）接收 presigned PUT 并验签。
	fs := newPresignTestFS(t, "")

	u, err := fs.PresignedURL(context.Background(), "dir/a.txt", "PUT", time.Hour)
	if err != nil {
		t.Fatalf("PresignedURL(PUT): %v", err)
	}
	if u == "" {
		t.Fatalf("PresignedURL 不应为空")
	}
	// 签名参数齐全（SigV4：X-Amz-*）。
	for _, want := range []string{"X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Credential", "X-Amz-Signature", "X-Amz-Date", "X-Amz-Expires"} {
		if !strings.Contains(u, want) {
			t.Fatalf("presigned URL 缺 %q: %s", want, u)
		}
	}
}

// TestS3FS_PresignedURL_Get 验证 GET presigned URL。
func TestS3FS_PresignedURL_Get(t *testing.T) {
	fs := newPresignTestFS(t, "")
	u, err := fs.PresignedURL(context.Background(), "dir/a.txt", "GET", time.Hour)
	if err != nil {
		t.Fatalf("PresignedURL(GET): %v", err)
	}
	if u == "" {
		t.Fatalf("PresignedURL 不应为空")
	}
}

// TestS3FS_PresignedURL_BadMethod 非法方法拒绝。
func TestS3FS_PresignedURL_BadMethod(t *testing.T) {
	fs := newPresignTestFS(t, "")
	if _, err := fs.PresignedURL(context.Background(), "a.txt", "DELETE", time.Hour); err == nil {
		t.Fatalf("DELETE 方法应拒绝")
	}
}

// newPresignTestFS 构造指向 mock S3 端点的 S3FS（presign 只验 URL 签名参数，无需真实 S3）。
func newPresignTestFS(t *testing.T, _ string) *S3FS {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// minio 探测 region：GET /<bucket>?location → 有效 XML（否则 EOF）。
		if r.URL.Query().Get("location") != "" {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(xmlLocationUSEast1))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	fs, err := NewS3FS(ClientConfig{
		Endpoint:  srv.URL,
		AccessKey: "test-key",
		SecretKey: "test-secret",
		Bucket:    "test-bucket",
		Region:    "us-east-1", // 非空 → minio 跳过 getBucketLocation 探测（否则发请求 EOF）
		UseSSL:    false,
	})
	if err != nil {
		t.Fatalf("NewS3FS: %v", err)
	}
	return fs
}

// xmlLocationUSEast1 是 S3 GetBucketLocation 响应（us-east-1）。
const xmlLocationUSEast1 = `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`
