// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// upload_owner_test.go 验证普通上传迁移到 Tenant API 后的新布局：
// 文件落在 <root>/<owner>/user/<rel> 下，checksum key 为 rel（无 owner 前缀）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// actorUploadMux 构造把固定 actor 注入请求 ctx 后转发普通上传 handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 cloud_owner_test.go 的模式）。
func actorUploadMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	return mux
}

// ownerUploadEnv 提供共享 Handlers 的多 actor 普通上传测试环境（新布局装配）。
type ownerUploadEnv struct {
	h    *Handlers
	root string            // 存储根（<root>/<owner>/user/...）
	urls map[string]string // actor → httptest 服务地址
}

// newOwnerUploadEnv 创建多 actor 普通上传环境，装配 Tenant API（StorageRoot + 租户缓存）。
func newOwnerUploadEnv(t *testing.T) *ownerUploadEnv {
	t.Helper()
	root := t.TempDir()

	cfg := Default()
	cfg.UploadsDir = root
	cfg.StorageRootPath = root
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	h := &Handlers{
		cfgPtr:        &cfgPtr,
		checksumStore: NewChecksumStore(root, testLogger()), // 旧全局装配（兼容 RED 阶段；迁移后 per-tenant 生效）
		logger:        testLogger(),
		auditLogger:   testLogger(),
		uploadingStop: make(chan struct{}),
	}
	globalRoot, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatalf("打开存储根失败: %v", err)
	}
	h.globalRoot = globalRoot
	h.globalPool = quota.NewPool(cfg.MaxStorageBytes)
	h.tenantRoots = make(map[string]*storage.Tenant)
	h.checksumStores = make(map[string]*ChecksumStore)
	h.quotaScopes = make(map[string]*quota.Scope)
	if h.tenantFor(anonymousOwner) == nil {
		t.Fatal("创建 anonymous 租户失败")
	}
	if h.tenantFor("alice") == nil {
		t.Fatal("创建 alice 租户失败")
	}
	t.Cleanup(func() {
		// h.Close() 内部经 closeOnce 关闭 uploadingStop 并等待 uploadingWg，勿重复手动关闭。
		_ = h.Close()
	})

	env := &ownerUploadEnv{h: h, root: root, urls: map[string]string{}}
	for actor, mux := range map[string]*http.ServeMux{
		"alice": actorUploadMux(h, "alice"),
		"":      actorUploadMux(h, ""),
	} {
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		env.urls[actor] = ts.URL
	}
	return env
}

// TestUpload_NewLayoutDiskPath 验证普通上传迁移到 Tenant API 后：
// 文件落在 <root>/<owner>/user/<rel> 下（而非 <root>/<owner>/<rel>），
// checksum key 为 rel（user/<path>）且无 owner 前缀（per-tenant store）。
func TestUpload_NewLayoutDiskPath(t *testing.T) {
	env := newOwnerUploadEnv(t)

	body := []byte("alice content")
	status, respBody := uploadFile(t, env.urls["alice"], "dir/f.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
		"X-File-Path":     "dir/f.txt",
	})
	if status != http.StatusOK {
		t.Fatalf("上传应成功: %d %s", status, respBody)
	}

	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "f.txt")); err != nil {
		t.Fatalf("文件应落在 alice/user/dir/f.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "dir", "f.txt")); err == nil {
		t.Fatalf("文件不应落在旧布局 alice/dir/f.txt")
	}

	csStore := env.h.checksumStoreFor("alice")
	if csStore == nil {
		t.Fatal("per-tenant checksum store 应为非 nil")
	}
	wantCS := sha256hex(body)
	if got, ok := csStore.Get("user/dir/f.txt"); !ok {
		t.Fatalf("checksum key 应为 user/dir/f.txt（无 owner 前缀）")
	} else if got != wantCS {
		t.Fatalf("checksum 值 = %s, want %s", got, wantCS)
	}
	if _, ok := csStore.Get("alice/user/dir/f.txt"); ok {
		t.Fatalf("旧 checksum key（含 owner 前缀）不应存在")
	}
}
