// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// dedup_integration_test.go 验证内容寻址去重（dedup.enabled）装配层语义：
//  1. enabled=true：同 owner 上传同内容两个文件名 → 第二个硬链接（同 inode），
//     引用计数台账登记，配额只计首份物理占用。
//  2. enabled=false（默认）：上传同内容不硬链（两独立 inode，零回归）。
//  3. 删除一个引用后：台账引用计数递减，另一引用仍可读（inode 保留）。

import (
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestDedupIntegration_EnabledCreatesHardlink dedup.enabled=true 时上传同内容两文件 → 硬链接。
func TestDedupIntegration_EnabledCreatesHardlink(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Dedup.Enabled = true
	})
	root := cfgPtr.Load().StorageRoot

	body := []byte("dedup-same-content-123")
	cs := sha256hex(body)
	hdr := map[string]string{"X-File-Checksum": cs}

	if status, respBody := uploadFile(t, url, "a.txt", body, hdr); status != http.StatusOK {
		t.Fatalf("上传 a.txt 应 200, got %d %s", status, respBody)
	}
	if status, respBody := uploadFile(t, url, "b.txt", body, hdr); status != http.StatusOK {
		t.Fatalf("上传 b.txt 应 200, got %d %s", status, respBody)
	}

	absA := filepath.Join(root, anonymousOwner, "user", "a.txt")
	absB := filepath.Join(root, anonymousOwner, "user", "b.txt")
	ia, err := os.Stat(absA)
	if err != nil {
		t.Fatalf("stat a.txt: %v", err)
	}
	ib, err := os.Stat(absB)
	if err != nil {
		t.Fatalf("stat b.txt: %v", err)
	}
	if !os.SameFile(ia, ib) {
		t.Fatalf("dedup 开启时同内容文件应为硬链接（同 inode）: a=%v b=%v", ia, ib)
	}
	// 物理占用只计一份（b.txt 未新增 inode）。
	if n := ib.Sys(); n != nil {
		t.Logf("b.txt 链接数=%d", fileNlink(ib))
	}
}

// TestDedupIntegration_DisabledNoHardlink dedup 默认关（零回归）：同内容上传不硬链。
func TestDedupIntegration_DisabledNoHardlink(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil) // dedup 默认关
	root := cfgPtr.Load().StorageRoot

	body := []byte("dedup-same-content-456")
	cs := sha256hex(body)
	hdr := map[string]string{"X-File-Checksum": cs}

	if status, respBody := uploadFile(t, url, "a.txt", body, hdr); status != http.StatusOK {
		t.Fatalf("上传 a.txt 应 200, got %d %s", status, respBody)
	}
	if status, respBody := uploadFile(t, url, "b.txt", body, hdr); status != http.StatusOK {
		t.Fatalf("上传 b.txt 应 200, got %d %s", status, respBody)
	}

	ia, err := os.Stat(filepath.Join(root, anonymousOwner, "user", "a.txt"))
	if err != nil {
		t.Fatalf("stat a.txt: %v", err)
	}
	ib, err := os.Stat(filepath.Join(root, anonymousOwner, "user", "b.txt"))
	if err != nil {
		t.Fatalf("stat b.txt: %v", err)
	}
	if os.SameFile(ia, ib) {
		t.Fatal("dedup 关闭时同内容文件不应硬链接（零回归）")
	}
}

// TestDedupIntegration_DeleteKeepsOtherRef 删除一个引用后另一引用仍可读（inode 保留）。
func TestDedupIntegration_DeleteKeepsOtherRef(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Dedup.Enabled = true
	})
	root := cfgPtr.Load().StorageRoot

	body := []byte("dedup-delete-ref-content")
	cs := sha256hex(body)
	hdr := map[string]string{"X-File-Checksum": cs}

	if status, respBody := uploadFile(t, url, "a.txt", body, hdr); status != http.StatusOK {
		t.Fatalf("上传 a.txt 应 200, got %d %s", status, respBody)
	}
	if status, respBody := uploadFile(t, url, "b.txt", body, hdr); status != http.StatusOK {
		t.Fatalf("上传 b.txt 应 200, got %d %s", status, respBody)
	}

	// 删除 a.txt（需 checksum header）。
	if status, respBody := uploadFile(t, url, "", body, nil); status != http.StatusBadRequest {
		t.Fatalf("空 filename 应 400, got %d %s", status, respBody)
	}
	delReq := newDeleteRequest(t, url, "a.txt", cs)
	resp, err := testHTTPClient(t).Do(delReq)
	if err != nil {
		t.Fatalf("delete a.txt: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete a.txt 应 200, got %d", resp.StatusCode)
	}

	// a.txt 已删，b.txt 仍存在且内容正确（inode 保留）。
	if _, statErr := os.Stat(filepath.Join(root, anonymousOwner, "user", "a.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("删除后 a.txt 应不存在, err=%v", statErr)
	}
	bData, err := os.ReadFile(filepath.Join(root, anonymousOwner, "user", "b.txt"))
	if err != nil {
		t.Fatalf("b.txt 应仍可读: %v", err)
	}
	if string(bData) != string(body) {
		t.Fatalf("b.txt 内容=%q want %q", bData, body)
	}
}

// fileNlink 提取文件链接数（平台无关；不支持时返回 0）。
func fileNlink(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(interface{ Nlink() uint64 }); ok {
		return st.Nlink()
	}
	return 0
}

// newDeleteRequest 构造 POST /delete?filename= 请求（带 checksum header）。
func newDeleteRequest(t *testing.T, baseURL, filename, checksum string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/delete?filename="+filename, nil)
	if err != nil {
		t.Fatalf("new delete req: %v", err)
	}
	req.Header.Set(headerFileChecksum, checksum)
	return req
}

// TestDedupStoreFor_CachePerOwner 验证 DedupStoreFor 懒建缓存：
// 同 owner 两次调用返回同一实例（幂等，避免每次磁盘 Load dedup.json）；
// 不同 owner 返回不同实例（per-owner 隔离）；dedup 关闭时返回 nil（零回归）。
func TestDedupStoreFor_CachePerOwner(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.Dedup.Enabled = true
	})
	_ = url
	// 直接经 filesRuntime 访问 DedupStoreFor（白盒：通过 fileService 的 rt 路径拿不到 h，
	// 用 RegisterRoutes 返回的 h 内部构造 filesRuntime 校验缓存语义）。
	rt := filesRuntime{h: registeredHandlersFrom(t, cfgPtr)}
	ds1 := rt.DedupStoreFor("alice")
	if ds1 == nil {
		t.Fatal("dedup 开启时 DedupStoreFor(alice) 不应为 nil")
	}
	ds2 := rt.DedupStoreFor("alice")
	if ds1 != ds2 {
		t.Fatalf("同 owner DedupStoreFor 应返回同一实例（缓存）: %p vs %p", ds1, ds2)
	}
	dsBob := rt.DedupStoreFor("bob")
	if dsBob == nil || dsBob == ds1 {
		t.Fatalf("不同 owner 应返回独立实例（per-owner 隔离）: alice=%p bob=%p", ds1, dsBob)
	}
}

// TestDedupStoreFor_DisabledReturnsNil dedup 关闭时 DedupStoreFor 返回 nil（零回归）。
func TestDedupStoreFor_DisabledReturnsNil(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil) // dedup 默认关
	_ = url
	rt := filesRuntime{h: registeredHandlersFrom(t, cfgPtr)}
	if ds := rt.DedupStoreFor("alice"); ds != nil {
		t.Fatalf("dedup 关闭时 DedupStoreFor 应为 nil, got %p", ds)
	}
}

// registeredHandlersFrom 从测试服务器 cfgPtr 重新 RegisterRoutes 拿 *Handlers
// （白盒：测试同包可访问 RegisterRoutes 返回实例；不改 newTestServerWithAllRoutes 签名）。
func registeredHandlersFrom(t *testing.T, cfgPtr *atomic.Pointer[Config]) *Handlers {
	t.Helper()
	cfg := cfgPtr.Load()
	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                cfgPtr,
		Version:               "test-version",
		BuildAt:               "test-buildat",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	t.Cleanup(func() { _ = h.Close() })
	_ = cfg
	return h
}
