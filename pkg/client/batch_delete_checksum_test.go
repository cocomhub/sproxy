// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBatchDelete_ResolvesMissingChecksum 锁定：BatchDelete 在 checksum 缺失时经
// Stat 补齐（服务端按 checksum 校验身份，空值会被拒为「缺少 checksum」）。
func TestBatchDelete_ResolvesMissingChecksum(t *testing.T) {
	t.Parallel()
	const sum = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/api/files/stat":
			w.Header().Set("X-File-Checksum", sum)
			w.Header().Set("X-File-Size", "5")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/batch/delete":
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[{"filename":"a.txt","success":true,"message":"ok"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL, WithHTTPClient(srv.Client()))
	res, err := c.BatchDelete(context.Background(), []BatchDeleteFile{{Filename: "a.txt"}})
	if err != nil {
		t.Fatalf("BatchDelete: %v", err)
	}
	if len(res) != 1 || !res[0].Success {
		t.Fatalf("结果异常: %+v", res)
	}
	if !strings.Contains(gotBody, sum) {
		t.Fatalf("请求体应含经 Stat 补齐的 checksum，实际: %s", gotBody)
	}
}

// TestBatchDelete_KeepsExplicitChecksum 锁定：显式 checksum 不被 Stat 覆盖
// （调用方已知摘要时跳过额外 RTT）。
func TestBatchDelete_KeepsExplicitChecksum(t *testing.T) {
	t.Parallel()
	var statCalled bool
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/api/files/stat":
			statCalled = true
			w.Header().Set("X-File-Checksum", "should-not-be-used")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/api/batch/delete":
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte(`{"results":[{"filename":"a.txt","success":true,"message":"ok"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL, WithHTTPClient(srv.Client()))
	if _, err := c.BatchDelete(context.Background(), []BatchDeleteFile{{Filename: "a.txt", Checksum: "explicit-sum"}}); err != nil {
		t.Fatalf("BatchDelete: %v", err)
	}
	if statCalled {
		t.Error("显式给了 checksum 时不应再调 Stat")
	}
	if !strings.Contains(gotBody, "explicit-sum") {
		t.Fatalf("应保留显式 checksum，实际: %s", gotBody)
	}
}
