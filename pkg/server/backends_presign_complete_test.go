// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backends_presign_complete_test.go 验证直传完成登记端点（roadmap 3.3 签名 v4 直传闭环）：
//  1. POST /api/backends/{type}/presign/complete?path= → 对象已存在 → 200。
//  2. 对象不存在 → 404。
//  3. type 未注册 → 404。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// completeTestBackend 是支持 Stat 的 fake 后端（complete 端点测试）。
type completeTestBackend struct {
	entries map[string]bool
}

// completeTestFS 是内存 sync.FS（Stat 查 entries；其余方法不支持）。
type completeTestFS struct{ entries map[string]bool }

func (f *completeTestFS) Stat(_ context.Context, rel string) (*sync.Entry, error) {
	if f.entries[rel] {
		return &sync.Entry{Name: rel}, nil
	}
	return nil, os.ErrNotExist
}
func (*completeTestFS) ListDir(context.Context, string) ([]sync.Entry, error) { return nil, nil }
func (*completeTestFS) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}
func (*completeTestFS) WriteFile(context.Context, string, io.Reader, int64, int64) error { return nil }
func (*completeTestFS) Rename(context.Context, string, string) error                     { return nil }
func (*completeTestFS) Delete(context.Context, string) error                             { return nil }
func (*completeTestFS) MakeDir(context.Context, string) error                            { return nil }

func (b *completeTestBackend) FS() sync.FS  { return &completeTestFS{entries: b.entries} }
func (b *completeTestBackend) Close() error { return nil }

// TestBackendsPresignComplete 验证 complete 成功路径（对象存在 → 200）。
func TestBackendsPresignComplete(t *testing.T) {
	t.Parallel()
	registry.RegisterBackend(completeTestType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return &completeTestBackend{entries: map[string]bool{"dir/a.txt": true}}, nil
	})
	defer registry.UnregisterBackendForTest(completeTestType)

	h := &Handlers{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign/complete", h.backendPresignCompleteHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+completeTestType+"/presign/complete?path=dir/a.txt", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

// TestBackendsPresignComplete_NotFound 对象不存在 → 404。
func TestBackendsPresignComplete_NotFound(t *testing.T) {
	t.Parallel()
	registry.RegisterBackend(completeTestType2, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return &completeTestBackend{entries: map[string]bool{}}, nil
	})
	defer registry.UnregisterBackendForTest(completeTestType2)

	h := &Handlers{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign/complete", h.backendPresignCompleteHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/"+completeTestType2+"/presign/complete?path=dir/nope.txt", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestBackendsPresignComplete_Unregistered 未注册 type → 404。
func TestBackendsPresignComplete_Unregistered(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign/complete", h.backendPresignCompleteHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/backends/no-such/presign/complete?path=a.txt", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

const (
	completeTestType  = "backends-complete-test"
	completeTestType2 = "backends-complete-test2"
)
