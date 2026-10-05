// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// range_serve_http_test.go 验证「播放器 Range 请求加密卷视频 → 服务端解密转发 → 206」
// 顶点链路（2026-10-05 通用文件获取 A 态）：真实 SecretdataFS 写文件 → syncpkg.
// RangeSeeker（适配 OpenRangeRead 段级解密）→ http.ServeContent → Range 请求命中
// 返回 206 + Content-Range + 解密明文段一致。这是 PR #733 关键帧分块地基的 HTTP 接线验证。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// zeroTime 是 ServeContent 的零 ModTime（避免 If-Modified-Since 干扰）。
var zeroTime = time.Time{}

// TestServeContent_RangeSeeker_Secretdata：真实加密卷 + ServeContent 顶点验证。
// Range: bytes=2-7 → 206 + Content-Range + 明文段一致（OpenRangeRead 解密转发）。
func TestServeContent_RangeSeeker_Secretdata(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(4096) // 跨多块（Block 64-128B → 32+ 块）
	if err := fs.WriteFile(ctx, "movie.mp4", io.NopCloser(bytesReader(content)), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rs, err := syncpkg.NewRangeSeeker(ctx, fs, "movie.mp4", int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	defer rs.Close()

	req := httptest.NewRequest(http.MethodGet, "/download?filename=movie.mp4", nil)
	req.Header.Set("Range", "bytes=100-107")
	rec := httptest.NewRecorder()
	http.ServeContent(rec, req, "movie.mp4", zeroTime, rs)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range 应 206, got %d: %s", rec.Code, rec.Body.String())
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 100-107/4096" {
		t.Fatalf("Content-Range=%q want bytes 100-107/4096", cr)
	}
	if got := rec.Body.String(); got != string(content[100:108]) {
		t.Fatalf("Range 内容 != 解密明文段：got %q want %q", got, content[100:108])
	}
}

// TestServeContent_RangeSeeker_FullRead：无 Range 请求 → 200 全量整流（解密转发完整）。
func TestServeContent_RangeSeeker_FullRead(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(2048)
	if err := fs.WriteFile(ctx, "movie.mp4", io.NopCloser(bytesReader(content)), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rs, err := syncpkg.NewRangeSeeker(ctx, fs, "movie.mp4", int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	defer rs.Close()

	req := httptest.NewRequest(http.MethodGet, "/download?filename=movie.mp4", nil)
	rec := httptest.NewRecorder()
	http.ServeContent(rec, req, "movie.mp4", zeroTime, rs)

	if rec.Code != http.StatusOK {
		t.Fatalf("整流应 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(rec.Body.Bytes()) != len(content) {
		t.Fatalf("整流长度=%d want %d", len(rec.Body.Bytes()), len(content))
	}
}

// bytesReader 包装字节为 io.Reader（避免引入 bytes import 歧义）。
func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	off int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}
