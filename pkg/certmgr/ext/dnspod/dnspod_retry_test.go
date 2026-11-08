// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dnspod

// dnspod_retry_test.go 锁定 DNSPod 调用的**重试语义按 Action 区分**（2026-11-08）：
//
// DNSPod 用 **GET 承载写操作**（CreateRecord/DeleteRecord 都在 query string 里），
// 而 `httpc` 默认按 HTTP 方法判定可重试（GET ⇒ 可重试）。若不做区分，瞬时 5xx 重试
// 会**重复创建 ACME 校验记录**（真实缺陷）。因此：
//   - 读操作 / 幂等写（RecordList、DeleteRecord）⇒ 允许重试；
//   - 非幂等写（CreateRecord 及其它写操作）⇒ **显式关闭重试**，fail-fast 交由上层重试。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestCleanupDNSRecord_RetriesTransient5xx(t *testing.T) {
	t.Parallel()
	var listHits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("Action") {
		case "RecordList":
			if listHits.Add(1) == 1 {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Response":{"RecordList":[{"RecordId":7,"Value":"test-key-auth"}]}}`))
		case "DeleteRecord":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Response":{"RecordId":7}}`))
		default:
			http.Error(w, "unexpected action", http.StatusBadRequest)
		}
	}))
	t.Cleanup(ts.Close)

	p := New(Config{SecretId: "test-secret-id", SecretKey: "test-secret-key", Endpoint: ts.URL, HTTPClient: testutil.IsolatedClient(t)})
	if err := p.CleanupDNSRecord(context.Background(), "example.com", "token", "test-key-auth"); err != nil {
		t.Fatalf("RecordList 瞬时 5xx 应重试成功: %v（请求次数=%d）", err, listHits.Load())
	}
	if got := listHits.Load(); got < 2 {
		t.Fatalf("期望至少 2 次 RecordList（首次 502 后重试），实际 %d —— 未接入重试编排", got)
	}
}

func TestSetDNSRecord_NonIdempotentNotRetried(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "bad gateway", http.StatusBadGateway)
	}))
	t.Cleanup(ts.Close)

	p := New(Config{SecretId: "test-secret-id", SecretKey: "test-secret-key", Endpoint: ts.URL, HTTPClient: testutil.IsolatedClient(t)})
	if err := p.SetDNSRecord(context.Background(), "example.com", "token", "test-key-auth"); err == nil {
		t.Fatalf("5xx 应报错")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("CreateRecord 非幂等 ⇒ 不得重试（防重复建记录），实际 %d 次", got)
	}
}
