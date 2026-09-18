// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_api_test.go 验证 backend 列表 API（V4）：
//  1. GET /api/backends → 已注册后端类型列表（registry.BackendTypes()）。
//  2. volSet nil（未装配卷集合）→ 空列表 200（不报错）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// backendsAPITestType 是 backend 列表 API 测试的 fake 类型（全局注册一次）。
const backendsAPITestType = "backends-api-test"

func TestBackendsAPI(t *testing.T) {
	t.Parallel()
	registry.RegisterBackend(backendsAPITestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return &fakeUserVolBackend{}, nil
	})
	defer func() {
		registry.UnregisterBackendForTest(backendsAPITestType)
	}()

	h := &Handlers{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/backends", h.backendsHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/backends", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Backends []string `json:"backends"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(out.Backends) == 0 {
		t.Fatal("backends 为空列表，want 至少含已注册类型")
	}
	found := false
	for _, b := range out.Backends {
		if b == backendsAPITestType {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("backends 应含 %q，got %v", backendsAPITestType, out.Backends)
	}
}

// TestBackendsAPI_NoVolSet 钉住 volSet nil（未装配卷集合）→ 空列表 200。
func TestBackendsAPI_NoVolSet(t *testing.T) {
	t.Parallel()
	h := &Handlers{} // volSet nil
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/backends", h.backendsHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/backends", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out struct {
		Backends []string `json:"backends"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if out.Backends == nil {
		t.Fatal("backends 应为空切片（非 nil），JSON 序列化为 []")
	}
}
