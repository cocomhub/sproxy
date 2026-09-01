// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// dirs_owner_test.go 验证 mkdir/rmdir 迁移到 Tenant API 后的新布局行为：
// 用户目录映射到 <root>/<owner>/user/<rel>；功能桶名（cloud 等）作为用户路径首段
// 完全合法（user/ 前缀物理隔离，不与功能桶冲突）；rmdir 递归删除并清理 per-tenant
// checksum 记录（key = rel，无 owner 前缀）。

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// actorDirsMux 构造把固定 actor 注入请求 ctx 后转发 mkdir/rmdir/list handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 delete_owner_test.go 的模式）。
func actorDirsMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mkdir", wrap(h.mkdir))
	mux.HandleFunc("POST /rmdir", wrap(h.rmdir))
	mux.HandleFunc("GET /api/files", wrap(h.listFiles))
	return mux
}

// ownerDirsEnv 提供共享 Handlers 的多 actor mkdir/rmdir/list 测试环境（新布局装配）。
type ownerDirsEnv struct {
	h    *Handlers
	root string // 存储根（<root>/<owner>/user/...）
	mux  map[string]*http.ServeMux
}

// newOwnerDirsEnv 创建多 actor mkdir/rmdir 测试环境，装配 Tenant API（复用 newAssemblyTestHandlers）。
func newOwnerDirsEnv(t *testing.T) *ownerDirsEnv {
	t.Helper()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	// listFiles 仍读全局 checksum store（resolveListDir 迁移属任务 9），装配全局 store 防 nil。
	h.checksumStore = NewChecksumStore(root, testLogger())
	// 预创建 anonymous 与 alice 租户（与 newOwnerDelRenameEnv 一致）。
	if h.tenantFor(anonymousOwner) == nil {
		t.Fatal("创建 anonymous 租户失败")
	}
	if h.tenantFor("alice") == nil {
		t.Fatal("创建 alice 租户失败")
	}
	env := &ownerDirsEnv{h: h, root: root, mux: map[string]*http.ServeMux{}}
	for _, actor := range []string{"alice", ""} {
		env.mux[actor] = actorDirsMux(h, actor)
	}
	return env
}

// doPost 以指定 actor 的 mux 发起 POST 请求，返回状态码。
func (e *ownerDirsEnv) doPost(t *testing.T, actor, path string) int {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	return rr.Code
}

// doGet 以指定 actor 的 mux 发起 GET 请求，返回响应体。
func (e *ownerDirsEnv) doGet(t *testing.T, actor, path string) string {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	return rr.Body.String()
}

// TestMkdir_UserBucketUnderUser 验证 mkdir 迁移到 Tenant API 后：
// 用户在 user/ 桶内创建 "cloud" 目录 → 200，落盘 <root>/alice/user/cloud；
// 顶层列表不暴露功能桶。注意：meta/user 桶目录在 resolveListDir 迁移（任务 9）
// 之前仍可见（<tenant>/meta 物理存在），故只断言未落盘的功能桶不出现。
func TestMkdir_UserBucketUnderUser(t *testing.T) {
	env := newOwnerDirsEnv(t)

	if code := env.doPost(t, "alice", "/mkdir?dirname=cloud"); code != http.StatusOK {
		t.Fatalf("mkdir cloud 应 200, got %d", code)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "cloud")); err != nil {
		t.Fatalf("应落盘 alice/user/cloud: %v", err)
	}

	body := env.doGet(t, "alice", "/api/files")
	for _, bucket := range []string{"cloud", "archive", "chunk", "version"} {
		if strings.Contains(body, `"`+bucket+`"`) {
			t.Fatalf("顶层列表不应出现功能桶 %q: %s", bucket, body)
		}
	}
}

// TestRmdir_NewLayoutChecksumCleaned 验证 rmdir 迁移到 Tenant API 后：
// 删除 user 桶内目录，且 per-tenant checksum store 中 rel 前缀与 rel 自身记录被清理，
// 目录外记录不受影响。
func TestRmdir_NewLayoutChecksumCleaned(t *testing.T) {
	env := newOwnerDirsEnv(t)

	dir := filepath.Join(env.root, "alice", "user", "subdir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	csStore := env.h.checksumStoreFor("alice")
	if csStore == nil {
		t.Fatal("per-tenant checksum store 应为非 nil")
	}
	csStore.Set("user/subdir/a.txt", "sha256hex")
	csStore.Set("user/subdir", "dirchecksum") // 目录自身的记录
	csStore.Set("user/other.txt", "other")

	if code := env.doPost(t, "alice", "/rmdir?dirname=subdir&force=true"); code != http.StatusOK {
		t.Fatalf("rmdir 应 200, got %d", code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("目录应已从 alice/user/subdir 删除")
	}
	if _, ok := csStore.Get("user/subdir/a.txt"); ok {
		t.Fatal("子文件 checksum 应被 DeletePrefix(rel + \"/\") 清理")
	}
	if _, ok := csStore.Get("user/subdir"); ok {
		t.Fatal("目录自身 checksum 应被 Delete(rel) 清理")
	}
	if _, ok := csStore.Get("user/other.txt"); !ok {
		t.Fatal("目录外 checksum 不应被误删")
	}
}
