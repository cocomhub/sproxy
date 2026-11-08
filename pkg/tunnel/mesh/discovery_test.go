// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

// discovery_test.go 锁定 ListHubNodes 的**重试语义**（2026-11-08）：
//   - 5xx/网络瞬时错误 ⇒ 自动重试（此前单次失败即返回，discovery 循环要等下一轮 10s 才重试）；
//   - 4xx（配置/凭据错误）⇒ **不重试**，立即失败（fail-fast，避免掩盖真实配置问题）；
//   - 成功路径与最终错误类型（*hubAPIError）保持不变。

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// hubNodesJSON 是 /api/hub/nodes 的最小合法响应。
func hubNodesJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "hub-1", "virtual_ip": "10.0.0.1"}})
}

func TestListHubNodes_RetriesTransient5xx(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		hubNodesJSON(w)
	}))
	defer srv.Close()

	nodes, err := ListHubNodes(t.Context(), srv.URL, "", "", "", false, testutil.IsolatedClient(t))
	if err != nil {
		t.Fatalf("瞬时 5xx 后应重试成功，实际报错: %v（请求次数=%d）", err, atomic.LoadInt32(&calls))
	}
	if len(nodes) != 1 || nodes[0].ID != "hub-1" {
		t.Fatalf("解析结果不符: %+v", nodes)
	}
	if got := atomic.LoadInt32(&calls); got < 2 {
		t.Fatalf("期望至少 2 次请求（首次 502 后重试），实际 %d 次 —— 未接入重试编排", got)
	}
}

func TestListHubNodes_NonRetryable4xxFailsFast(t *testing.T) {
	t.Parallel()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := ListHubNodes(t.Context(), srv.URL, "", "", "", false, testutil.IsolatedClient(t))
	if err == nil {
		t.Fatalf("4xx 应直接失败")
	}
	var apiErr *hubAPIError
	if !errors.As(err, &apiErr) || apiErr.code != http.StatusUnauthorized {
		t.Fatalf("错误类型/状态码不符: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("4xx 不应重试，实际请求 %d 次", got)
	}
}
