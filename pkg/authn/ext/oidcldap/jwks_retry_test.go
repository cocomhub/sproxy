// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

// jwks_retry_test.go 锁定 OIDC 出站请求的重试语义（2026-11-08）：
//   - JWKS 拉取（GET，幂等）⇒ 瞬时 5xx/抖动自动重试（此前单次失败即认证失败）；
//   - token 交换（POST）⇒ 由 httpc 按**方法**默认判定为不可重试（不会重复消费 authorization code）。

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestFetchJWKSBody_RetriesTransient5xx(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	const body = `{"keys":[]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	got, err := fetchJWKSBody(t.Context(), newOIDCHTTPClient(), srv.URL)
	if err != nil {
		t.Fatalf("瞬时 5xx 后应重试成功: %v（请求次数=%d）", err, hits.Load())
	}
	if string(got) != body {
		t.Fatalf("body = %q，期望 %q", got, body)
	}
	if n := hits.Load(); n < 2 {
		t.Fatalf("期望至少 2 次请求（首次 502 后重试），实际 %d —— 未接入重试编排", n)
	}
}

func TestFetchJWKSBody_NonRetryable4xxFails(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	if _, err := fetchJWKSBody(t.Context(), newOIDCHTTPClient(), srv.URL); err == nil {
		t.Fatalf("4xx 应直接失败")
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("4xx 不应重试，实际 %d 次", n)
	}
}
