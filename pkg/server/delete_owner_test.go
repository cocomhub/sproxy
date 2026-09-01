// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// delete_owner_test.go 验证 delete/rename/batch 迁移到 Tenant API 后的新布局行为：
// 用户文件映射到 <root>/<owner>/user/<rel>；checksum key 为 rel（无 owner 前缀），
// 删除时从 per-tenant store 清理，重命名时在 per-tenant store 内迁移 key。

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// actorDelRenameMux 构造把固定 actor 注入请求 ctx 后转发 upload/delete/rename/batch handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 upload_owner_test.go / download_owner_test.go 的模式）。
func actorDelRenameMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", wrap(h.upload))
	mux.HandleFunc("POST /delete", wrap(h.delete))
	mux.HandleFunc("POST /rename", wrap(h.rename))
	mux.HandleFunc("POST /api/batch/delete", wrap(h.batchDelete))
	mux.HandleFunc("POST /api/batch/rename", wrap(h.batchRename))
	return mux
}

// ownerDelRenameEnv 提供共享 Handlers 的多 actor delete/rename/batch 测试环境（新布局装配）。
type ownerDelRenameEnv struct {
	h    *Handlers
	root string            // 存储根（<root>/<owner>/user/...）
	urls map[string]string // actor → httptest 服务地址
}

// newOwnerDelRenameEnv 创建多 actor 测试环境，装配 Tenant API（复用 newAssemblyTestHandlers）。
func newOwnerDelRenameEnv(t *testing.T) *ownerDelRenameEnv {
	t.Helper()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	// 预创建 anonymous 与 alice 租户（与 newOwnerUploadEnv 一致）。
	if h.tenantFor(anonymousOwner) == nil {
		t.Fatal("创建 anonymous 租户失败")
	}
	if h.tenantFor("alice") == nil {
		t.Fatal("创建 alice 租户失败")
	}
	env := &ownerDelRenameEnv{h: h, root: root, urls: map[string]string{}}
	for actor, mux := range map[string]*http.ServeMux{
		"alice": actorDelRenameMux(h, "alice"),
		"":      actorDelRenameMux(h, ""),
	} {
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		env.urls[actor] = ts.URL
	}
	return env
}

// doDelete 以指定 actor 的 mux 发起 POST /delete，返回状态码与响应体。
func (e *ownerDelRenameEnv) doDelete(t *testing.T, actor, filename, checksum string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/delete?filename="+filename, nil)
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	rr := httptest.NewRecorder()
	e.h.delete(rr, req.WithContext(withActor(req.Context(), actor)))
	return rr.Code, rr.Body.Bytes()
}

// doRename 以指定 actor 的 mux 发起 POST /rename，返回状态码与响应体。
func (e *ownerDelRenameEnv) doRename(t *testing.T, actor, from, to, checksum string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest("POST", "/rename?from="+from+"&to="+to, nil)
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	rr := httptest.NewRecorder()
	e.h.rename(rr, req.WithContext(withActor(req.Context(), actor)))
	return rr.Code, rr.Body.Bytes()
}

// TestDelete_NewLayoutChecksumCleaned 验证普通删除迁移到 Tenant API 后：
// 上传 user/dir/f.txt → 删除 → 磁盘文件移除，且 per-tenant checksum store 中
// key "user/dir/f.txt"（无 owner 前缀）被清理。
func TestDelete_NewLayoutChecksumCleaned(t *testing.T) {
	env := newOwnerDelRenameEnv(t)

	body := []byte("delete me")
	cs := sha256hex(body)
	status, respBody := uploadFile(t, env.urls["alice"], "dir/f.txt", body, map[string]string{
		"X-File-Checksum": cs,
		"X-File-Path":     "dir/f.txt",
	})
	if status != http.StatusOK {
		t.Fatalf("上传应成功: %d %s", status, respBody)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "f.txt")); err != nil {
		t.Fatalf("文件应落在 alice/user/dir/f.txt: %v", err)
	}

	csStore := env.h.checksumStoreFor("alice")
	if csStore == nil {
		t.Fatal("per-tenant checksum store 应为非 nil")
	}
	if got, ok := csStore.Get("user/dir/f.txt"); !ok || got != cs {
		t.Fatalf("删除前 checksum key user/dir/f.txt 应存在, got=%q ok=%v", got, ok)
	}

	code, resp := env.doDelete(t, "alice", "dir/f.txt", cs)
	if code != http.StatusOK {
		t.Fatalf("删除应 200, got %d: %s", code, resp)
	}

	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("文件应已从 alice/user/dir/f.txt 删除")
	}
	if _, ok := csStore.Get("user/dir/f.txt"); ok {
		t.Fatalf("per-tenant checksum key user/dir/f.txt 应已被清理")
	}
	if _, ok := csStore.Get("alice/user/dir/f.txt"); ok {
		t.Fatalf("旧 checksum key（含 owner 前缀）不应存在")
	}
}

// TestRename_NewLayoutChecksumMigrated 验证普通重命名迁移到 Tenant API 后：
// 文件在 user 桶内移动，per-tenant checksum key 从 fromRel 迁移到 toRel（无 owner 前缀）。
func TestRename_NewLayoutChecksumMigrated(t *testing.T) {
	env := newOwnerDelRenameEnv(t)

	body := []byte("rename me")
	cs := sha256hex(body)
	status, respBody := uploadFile(t, env.urls["alice"], "dir/f.txt", body, map[string]string{
		"X-File-Checksum": cs,
		"X-File-Path":     "dir/f.txt",
	})
	if status != http.StatusOK {
		t.Fatalf("上传应成功: %d %s", status, respBody)
	}

	csStore := env.h.checksumStoreFor("alice")
	if csStore == nil {
		t.Fatal("per-tenant checksum store 应为非 nil")
	}
	if got, ok := csStore.Get("user/dir/f.txt"); !ok || got != cs {
		t.Fatalf("重命名前 checksum key user/dir/f.txt 应存在, got=%q ok=%v", got, ok)
	}

	code, resp := env.doRename(t, "alice", "dir/f.txt", "dir/g.txt", cs)
	if code != http.StatusOK {
		t.Fatalf("重命名应 200, got %d: %s", code, resp)
	}

	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "g.txt")); err != nil {
		t.Fatalf("文件应落在 alice/user/dir/g.txt: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("旧路径 alice/user/dir/f.txt 应已移除")
	}
	if got, ok := csStore.Get("user/dir/g.txt"); !ok || got != cs {
		t.Fatalf("重命名后 checksum key 应迁移到 user/dir/g.txt, got=%q ok=%v", got, ok)
	}
	if _, ok := csStore.Get("user/dir/f.txt"); ok {
		t.Fatalf("旧 checksum key user/dir/f.txt 应已移除")
	}
}

// TestBatchDelete_NewLayoutOwnerScoped 验证批量删除迁移到 Tenant API 后：
// 以 owner 作用域删除文件，且仅清理该 owner 的 per-tenant checksum store。
func TestBatchDelete_NewLayoutOwnerScoped(t *testing.T) {
	env := newOwnerDelRenameEnv(t)

	body := []byte("batch delete")
	cs := sha256hex(body)
	status, respBody := uploadFile(t, env.urls["alice"], "a.txt", body, map[string]string{
		"X-File-Checksum": cs,
		"X-File-Path":     "a.txt",
	})
	if status != http.StatusOK {
		t.Fatalf("上传应成功: %d %s", status, respBody)
	}

	csStore := env.h.checksumStoreFor("alice")
	if csStore == nil {
		t.Fatal("per-tenant checksum store 应为非 nil")
	}
	if got, ok := csStore.Get("user/a.txt"); !ok || got != cs {
		t.Fatalf("批量删除前 checksum key user/a.txt 应存在, got=%q ok=%v", got, ok)
	}

	reqBody := fmt.Sprintf(`{"files":[{"filename":"a.txt","checksum":"%s"}]}`, cs)
	req := httptest.NewRequest("POST", "/api/batch/delete", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	env.h.batchDelete(rr, req.WithContext(withActor(req.Context(), "alice")))
	if rr.Code != http.StatusOK {
		t.Fatalf("批量删除应 200, got %d: %s", rr.Code, rr.Body.String())
	}

	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("文件应已从 alice/user/a.txt 删除")
	}
	if _, ok := csStore.Get("user/a.txt"); ok {
		t.Fatalf("per-tenant checksum key user/a.txt 应已被清理")
	}
}
