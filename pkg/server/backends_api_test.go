// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_api_test.go 验证 backend 列表 API（V4，schema 驱动）：
//  1. GET /api/backends → 已注册后端类型 + per-type schema（registry.BackendSchemas()）。
//  2. 实现 SchemaProvider 的后端返回 category/fields 数组；未实现者 fields 为空数组。
//  3. volSet nil（未装配卷集合）→ 空列表 200（不报错）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
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
		Backends []registry.BackendSchemaInfo `json:"backends"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(out.Backends) == 0 {
		t.Fatal("backends 为空列表，want 至少含已注册类型")
	}
	found := false
	gotTypes := make([]string, 0, len(out.Backends))
	for _, b := range out.Backends {
		gotTypes = append(gotTypes, b.Type)
		if b.Type == backendsAPITestType {
			found = true
			if b.Fields == nil {
				t.Fatalf("%q 未实现 SchemaProvider，fields 应为空数组（非 nil，JSON 序列化为 []）", backendsAPITestType)
			}
		}
	}
	if !found {
		t.Fatalf("backends 应含 %q，got %v", backendsAPITestType, gotTypes)
	}
}

// fakeSchemaBackend 是带 Schema() 的测试 backend（/api/backends schema 驱动建卷表单测试）。
type fakeSchemaBackend struct{}

func (fakeSchemaBackend) FS() syncpkg.FS { return nil }
func (fakeSchemaBackend) Close() error   { return nil }

func (fakeSchemaBackend) Schema() []registry.FieldSchema {
	return []registry.FieldSchema{
		{Key: "target", Label: "底层卷", Type: "volume-select", Required: true, AllowWrapper: true},
		{Key: "secret_url", Label: "密钥引用", Type: "text", Required: true},
	}
}

// TestBackendsAPI_SchemaDriven 钉住 schema 驱动建卷表单：实现 SchemaProvider 的后端在
// GET /api/backends 响应中含 category/fields 数组；未实现者只含 {type, category}（fields []）。
func TestBackendsAPI_SchemaDriven(t *testing.T) {
	t.Parallel()
	const schemaTyp = "audit-test-schema"
	const plainTyp = "audit-test-plain"
	registry.RegisterBackend(schemaTyp, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return &fakeSchemaBackend{}, nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(schemaTyp) })
	registry.RegisterBackend(plainTyp, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return &fakeUserVolBackend{}, nil
	})
	t.Cleanup(func() { registry.UnregisterBackendForTest(plainTyp) })

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
		Backends []registry.BackendSchemaInfo `json:"backends"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	byType := make(map[string]registry.BackendSchemaInfo, len(out.Backends))
	for _, b := range out.Backends {
		byType[b.Type] = b
	}
	s, ok := byType[schemaTyp]
	if !ok {
		t.Fatalf("backends 应含 %q，got %v", schemaTyp, keysOfSchemaInfo(byType))
	}
	if s.Category != "linked" {
		t.Fatalf("%q category = %q, want linked（普通外部类型）", schemaTyp, s.Category)
	}
	if s.Fields == nil {
		t.Fatalf("%q 实现 SchemaProvider，fields 应为非 nil（JSON 序列化为 []）", schemaTyp)
	}
	if len(s.Fields) != 2 || s.Fields[0].Key != "target" || s.Fields[0].Type != "volume-select" || !s.Fields[0].AllowWrapper {
		t.Fatalf("%q fields = %+v, want 含 target(volume-select, allow_wrapper) 字段", schemaTyp, s.Fields)
	}
	p, ok := byType[plainTyp]
	if !ok {
		t.Fatalf("backends 应含 %q，got %v", plainTyp, keysOfSchemaInfo(byType))
	}
	if p.Category != "linked" {
		t.Fatalf("%q category = %q, want linked", plainTyp, p.Category)
	}
	if p.Fields == nil || len(p.Fields) != 0 {
		t.Fatalf("%q 未实现 SchemaProvider，fields 应为空数组 []，got %+v", plainTyp, p.Fields)
	}
}

// keysOfSchemaInfo 返回 schema 条目 type 列表（断言错误信息用）。
func keysOfSchemaInfo(m map[string]registry.BackendSchemaInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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
		Backends []registry.BackendSchemaInfo `json:"backends"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if out.Backends == nil {
		t.Fatal("backends 应为空切片（非 nil），JSON 序列化为 []")
	}
}
