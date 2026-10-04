// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeRangeServer 模拟 Range 服务器：offset < boundary 返回 206，>= boundary 返回 416。
func fakeRangeServer(boundary int64, total int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rangeHeader := r.Header.Get("Range")
		if rangeHeader == "" {
			http.Error(w, "no range", http.StatusBadRequest)
			return
		}
		var start int64
		if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-", &start); err != nil {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		if start >= boundary {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, start+1023, total))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 1024))
	}))
}

// TestProbeBoundary 验证 416 边界探测：返回可下上限（< boundary 的最大对齐点）。
func TestProbeBoundary(t *testing.T) {
	total := int64(128 << 20)   // 128MB
	boundary := int64(64 << 20) // 分享直链可下 64MB（50%）
	srv := fakeRangeServer(boundary, total)
	defer srv.Close()

	d := &HybridDownloader{client: srv.Client()}
	got, err := d.probeBoundary(context.Background(), srv.URL, total)
	if err != nil {
		t.Fatalf("probeBoundary error: %v", err)
	}
	// 应探测到 < 64MB 的最大值（二分收敛到 boundary 附近）
	if got < boundary-4<<20 || got > boundary {
		t.Errorf("probeBoundary = %d, want near %d (50%%)", got, boundary)
	}
}

// TestProbeBoundary_FullRange 验证分享直链全 Range 可用（boundary = total）→ 返回 total。
func TestProbeBoundary_FullRange(t *testing.T) {
	total := int64(32 << 20)
	srv := fakeRangeServer(total, total) // boundary = total（全可下）
	defer srv.Close()
	d := &HybridDownloader{client: srv.Client()}
	got, err := d.probeBoundary(context.Background(), srv.URL, total)
	if err != nil {
		t.Fatalf("probeBoundary error: %v", err)
	}
	if got != total {
		t.Errorf("probeBoundary full = %d, want %d", got, total)
	}
}
