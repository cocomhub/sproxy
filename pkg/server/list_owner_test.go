// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// list_owner_test.go 验证 list/search 迁移到 Tenant API 后的新布局行为：
// 列表/搜索根为请求者租户 user/ 桶，功能桶（cloud/archive/chunk/version/meta）
// 在租户根顶层物理隔离、不可枚举（R1 修复）；subdir 经 UserRel 解析到 user 桶内。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// actorListMux 构造把固定 actor 注入请求 ctx 后转发 list/search handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 dirs_owner_test.go 的模式）。
func actorListMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", wrap(h.listFiles))
	mux.HandleFunc("GET /api/files/search", wrap(h.searchFiles))
	return mux
}

// ownerListEnv 提供共享 Handlers 的多 actor list/search 测试环境（新布局装配）。
type ownerListEnv struct {
	h    *Handlers
	root string // 存储根（<root>/<owner>/user/...）
	mux  map[string]*http.ServeMux
}

// newOwnerListEnv 创建多 actor list/search 测试环境，装配 Tenant API（复用 newAssemblyTestHandlers）。
func newOwnerListEnv(t *testing.T) *ownerListEnv {
	t.Helper()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	// 预创建 anonymous 与 alice 租户（与 newOwnerDirsEnv 一致）。
	if h.tenantFor(anonymousOwner) == nil {
		t.Fatal("创建 anonymous 租户失败")
	}
	if h.tenantFor("alice") == nil {
		t.Fatal("创建 alice 租户失败")
	}
	env := &ownerListEnv{h: h, root: root, mux: map[string]*http.ServeMux{}}
	for _, actor := range []string{"alice", ""} {
		env.mux[actor] = actorListMux(h, actor)
	}
	return env
}

// doGet 以指定 actor 的 mux 发起 GET 请求，解析为 listResponse。
func (e *ownerListEnv) doGet(t *testing.T, actor, path string) *listResponse {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: 期望 200, got %d: %s", path, rr.Code, rr.Body.String())
	}
	var resp listResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("GET %s: 解析响应失败: %v", path, err)
	}
	return &resp
}

// hasName 判断列表条目中是否存在指定 name。
func hasName(files []fileInfo, name string) bool {
	for _, f := range files {
		if f.Name == name {
			return true
		}
	}
	return false
}

// TestList_FeatureBucketsHidden 验证 list 迁移到 Tenant API 后功能桶不可枚举（R1 修复）：
// 租户根顶层物理存在的功能桶（cloud/archive/chunk/version/meta）不出现在 user 桶
// 顶层列表中；subdir 指向功能桶名解析到 user/<bucket>（不存在）→ 空列表。
func TestList_FeatureBucketsHidden(t *testing.T) {
	env := newOwnerListEnv(t)
	// 在租户根顶层物理创建功能桶并落数据，模拟 P2 功能数据（cloud 下载/归档/分块/版本/meta）。
	for _, bucket := range []string{"cloud", "archive", "chunk", "version", "meta"} {
		p := filepath.Join(env.root, "alice", bucket, "data.bin")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("feature data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 用户文件落在 user 桶内。
	userFile := filepath.Join(env.root, "alice", "user", "normal.txt")
	if err := os.MkdirAll(filepath.Dir(userFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userFile, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 顶层列表：user 桶文件可见，功能桶不可枚举。
	resp := env.doGet(t, "alice", "/api/files")
	if !hasName(resp.Files, "normal.txt") {
		t.Fatalf("顶层列表应包含用户文件 normal.txt: %+v", resp.Files)
	}
	for _, bucket := range []string{"cloud", "archive", "chunk", "version", "meta"} {
		if hasName(resp.Files, bucket) {
			t.Fatalf("顶层列表不应出现功能桶 %q: %+v", bucket, resp.Files)
		}
	}

	// subdir=cloud 解析到 user/cloud（该路径在 user 桶内不存在）→ 空列表。
	resp = env.doGet(t, "alice", "/api/files?subdir=cloud")
	if len(resp.Files) != 0 {
		t.Fatalf("subdir=cloud 应返回空列表（功能桶不在 user 桶下）: %+v", resp.Files)
	}
}
