// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

func TestParsePagination_Defaults(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files", nil)
	offset, limit := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_NegativeOffset(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=-1", nil)
	offset, _ := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
}

func TestParsePagination_ZeroLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?limit=0", nil)
	_, limit := parsePagination(r)
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_LargeLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?limit=99999", nil)
	_, limit := parsePagination(r)
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_Valid(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=10&limit=50", nil)
	offset, limit := parsePagination(r)
	if offset != 10 {
		t.Errorf("offset = %d, want 10", offset)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
}

func TestParsePagination_NonNumeric(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=abc&limit=xyz", nil)
	offset, limit := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_OffsetOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=5", nil)
	offset, limit := parsePagination(r)
	if offset != 5 {
		t.Errorf("offset = %d, want 5", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

// newAssemblyTestHandlers 构建装配好 globalRoot/globalPool 的 Handlers，供装配辅助
// （tenantFor/checksumStoreFor/quotaFor）测试使用。storageRoot 必须已存在（用 t.TempDir()）。
func newAssemblyTestHandlers(t *testing.T, storageRoot string) *Handlers {
	t.Helper()
	cfg := Default()
	cfg.UploadsDir = storageRoot
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	globalRoot, err := storage.OpenRoot(storageRoot)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{
		cfgPtr:         &cfgPtr,
		logger:         testLogger(),
		auditLogger:    testLogger(),
		uploadingStop:  make(chan struct{}),
		globalRoot:     globalRoot,
		globalPool:     quota.NewPool(cfg.MaxStorageBytes),
		tenantRoots:    make(map[string]*storage.Tenant),
		checksumStores: make(map[string]*ChecksumStore),
		quotaScopes:    make(map[string]*quota.Scope),
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestHandlers_TenantFor 验证租户懒创建：空 owner → anonymous、非空 owner 建租户、
// 非法 owner fail-closed 返回 nil、缓存复用。
func TestHandlers_TenantFor(t *testing.T) {
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)

	// 空 owner → anonymous 租户，且 anonymous 租户根与 meta 目录创建。
	tnt := h.tenantFor("")
	if tnt == nil || tnt.ID != "anonymous" {
		t.Fatalf("tenantFor(\"\")=%+v want anonymous", tnt)
	}
	if _, err := os.Stat(filepath.Join(root, "anonymous", "meta")); err != nil {
		t.Fatalf("anonymous/meta 未创建: %v", err)
	}

	// 懒创建 alice：首次访问建租户根 + meta 目录。
	tnt = h.tenantFor("alice")
	if tnt == nil || tnt.ID != "alice" {
		t.Fatalf("tenantFor(alice)=%+v", tnt)
	}
	if _, err := os.Stat(filepath.Join(root, "alice", "meta")); err != nil {
		t.Fatalf("alice/meta 未创建: %v", err)
	}
	// 已创建的缓存复用（同一实例）。
	if h.tenantFor("alice") != tnt {
		t.Fatal("tenantFor(alice) 应缓存复用同一实例")
	}

	// 非法 owner fail-closed：返回 nil，绝不回落全局根。
	for _, bad := range []string{"..", "a/b", "CON", "foo.", ".__x__"} {
		if got := h.tenantFor(bad); got != nil {
			t.Fatalf("tenantFor(%q) 应为 nil（fail-closed）, got %+v", bad, got)
		}
	}
}

// TestHandlers_ChecksumStoreFor 验证 per-tenant checksum 存储懒创建：storePath 落
// <tenant>/meta/checksums.json，租户间隔离。
func TestHandlers_ChecksumStoreFor(t *testing.T) {
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)

	cs := h.checksumStoreFor("alice")
	if cs == nil {
		t.Fatal("checksumStoreFor(alice)=nil")
	}
	cs.Set("user/dir/f.txt", "sha256hex")
	if got, ok := h.checksumStoreFor("alice").Get("user/dir/f.txt"); !ok || got != "sha256hex" {
		t.Fatalf("checksum 读写失败 got=%q ok=%v", got, ok)
	}
	// per-tenant 隔离：bob 的 store 看不到 alice 的记录。
	if _, ok := h.checksumStoreFor("bob").Get("user/dir/f.txt"); ok {
		t.Fatal("bob 不应看到 alice 的 checksum 记录")
	}
	// storePath 落 alice/meta/checksums.json。
	if _, err := os.Stat(filepath.Join(root, "alice", "meta", "checksums.json")); err != nil {
		t.Fatalf("checksums.json 未写入 alice/meta: %v", err)
	}
	// 空 owner → anonymous 租户的 store。
	if h.checksumStoreFor("") == nil {
		t.Fatal("checksumStoreFor(\"\")=nil（应映射 anonymous）")
	}
}

// TestHandlers_QuotaFor 验证 per-tenant 配额 Scope 懒创建：显式 owner 配额 > "*" 默认；
// 空 owner → anonymous（OwnerQuotaFor("anonymous")）。
func TestHandlers_QuotaFor(t *testing.T) {
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)

	cfg := h.cfgPtr.Load()
	cfg.OwnerQuotas = map[string]int64{"*": 5 << 30, "alice": 10 << 30}

	q := h.quotaFor("alice")
	if q == nil || q.MaxBytes() != 10<<30 {
		t.Fatalf("quotaFor(alice) MaxBytes=%d want %d", q.MaxBytes(), 10<<30)
	}
	qa := h.quotaFor("")
	if qa == nil || qa.MaxBytes() != 5<<30 {
		t.Fatalf("quotaFor(空→anonymous) MaxBytes=%d want %d（回退 * 默认）", qa.MaxBytes(), 5<<30)
	}
	// 缓存复用同一 Scope。
	if h.quotaFor("alice") != q {
		t.Fatal("quotaFor(alice) 应缓存复用同一 Scope")
	}
}

// TestHandlers_TenantFor_Concurrent 验证懒创建无竞态：并发访问同一 owner 只创建一个租户。
func TestHandlers_TenantFor_Concurrent(t *testing.T) {
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)

	const n = 32
	results := make([]*storage.Tenant, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.tenantFor("alice")
		}(i)
	}
	wg.Wait()
	for i, tn := range results {
		if tn == nil || tn.ID != "alice" {
			t.Fatalf("results[%d]=%+v", i, tn)
		}
	}
	if got := len(h.tenantRoots); got != 1 {
		t.Fatalf("tenantRoots 应有 1 个租户, got %d", got)
	}
}

// TestHandlers_TenantOf 验证 tenantOf 从请求 ctx 解析 owner 映射到租户：
// 未认证（owner 空）→ anonymous；认证 → 对应 owner 租户；非法 owner fail-closed 返回 nil。
func TestHandlers_TenantOf(t *testing.T) {
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)

	// 未认证请求 → anonymous 租户。
	r := httptest.NewRequest("GET", "/api/files", nil)
	if tnt := h.tenantOf(r); tnt == nil || tnt.ID != "anonymous" {
		t.Fatalf("tenantOf(未认证)=%+v want anonymous", tnt)
	}
	// 认证请求（ctx 带 actor）→ 对应 owner 租户。
	r2 := httptest.NewRequest("GET", "/api/files", nil)
	r2 = r2.WithContext(withActor(r2.Context(), "alice"))
	if tnt := h.tenantOf(r2); tnt == nil || tnt.ID != "alice" {
		t.Fatalf("tenantOf(alice)=%+v want alice", tnt)
	}
	// 非法 actor fail-closed（绝不回落全局根）。
	r3 := httptest.NewRequest("GET", "/api/files", nil)
	r3 = r3.WithContext(withActor(r3.Context(), "a/b"))
	if tnt := h.tenantOf(r3); tnt != nil {
		t.Fatalf("tenantOf(非法 owner)=%+v want nil", tnt)
	}
}

// TestRegisterRoutes_CreatesAnonymousTenantAndLayoutVersion 验证启动装配：OpenRoot 写入
// LAYOUT_VERSION、anonymous 租户根预创建、healthz 正常。
func TestRegisterRoutes_CreatesAnonymousTenantAndLayoutVersion(t *testing.T) {
	root := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = root
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:         mux,
		CfgPtr:      &cfgPtr,
		Version:     "test",
		BuildAt:     "test",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })

	// LAYOUT_VERSION 写入存储根。
	if _, err := os.Stat(filepath.Join(root, "LAYOUT_VERSION")); err != nil {
		t.Fatalf("LAYOUT_VERSION 未写入存储根: %v", err)
	}
	// anonymous 租户根预创建（含 meta 目录）。
	if _, err := os.Stat(filepath.Join(root, "anonymous", "meta")); err != nil {
		t.Fatalf("anonymous 租户根未预创建: %v", err)
	}
	// healthz 正常。
	rr := httptest.NewRecorder()
	h.healthz(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz 应 200, got %d", rr.Code)
	}
}
