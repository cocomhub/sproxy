// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// download_owner_test.go 验证普通下载迁移到 Tenant API 后的新布局：
// 用户文件映射到 <root>/<owner>/user/<rel>；功能桶名首段解析到 user/ 桶（文件不存在 404），
// .__ 内部前缀经 UserRel 拒绝（400）。

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// actorDownloadMux 构造把固定 actor 注入请求 ctx 后转发下载/stat/chunk handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 cloud_owner_test.go 的模式）。
func actorDownloadMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download", wrap(h.download))
	mux.HandleFunc("GET /download/chunk", wrap(h.downloadChunk))
	mux.HandleFunc("HEAD /api/files/stat", wrap(h.stat))
	return mux
}

// ownerDownloadEnv 提供共享 Handlers 的多 actor 普通下载/stat 测试环境（新布局装配）。
type ownerDownloadEnv struct {
	h    *Handlers
	root string // 存储根（<root>/<owner>/user/...）
	mux  map[string]*http.ServeMux
}

// newOwnerEnv 创建多 actor 普通下载环境，装配 Tenant API（复用 newAssemblyTestHandlers）。
func newOwnerEnv(t *testing.T) *ownerDownloadEnv {
	t.Helper()
	root := t.TempDir()
	h := newAssemblyTestHandlers(t, root)
	// 全局 checksum store（cloud kind 兼容；普通下载迁移后走 per-tenant store）。
	h.checksumStore = NewChecksumStore(root, testLogger())
	env := &ownerDownloadEnv{h: h, root: root, mux: map[string]*http.ServeMux{}}
	maps.Copy(env.mux, map[string]*http.ServeMux{
		"alice": actorDownloadMux(h, "alice"),
		"":      actorDownloadMux(h, ""),
	})
	return env
}

// doGet 以指定 actor 的 mux 发起 GET 请求，返回状态码。
func (e *ownerDownloadEnv) doGet(t *testing.T, actor, path string) int {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	return rr.Code
}

// TestDownload_FeatureBucketFirstSegment 验证普通下载（无 kind）对功能桶名首段路径
// 解析到 user/ 桶（UserRel 首段功能桶名合法，user/ 前缀物理隔离）：文件不存在 → 404，
// 存在则正常下载。普通 user 文件不受影响。
func TestDownload_FeatureBucketFirstSegment(t *testing.T) {
	env := newOwnerEnv(t)
	for _, name := range []string{"cloud/t1/f.bin", "archive/x.tar.gz", "chunk/s/00000.chunk", "meta/cloud/t1.json"} {
		code := env.doGet(t, "alice", "/download?filename="+url.QueryEscape(name))
		if code != http.StatusNotFound {
			t.Fatalf("%q 应 404（解析到 user/ 桶且文件不存在）, got %d", name, code)
		}
	}

	// 普通用户文件（映射到 user 桶）不受影响：新布局 <root>/alice/user/normal.txt 可正常下载。
	normal := filepath.Join(env.root, "alice", "user", "normal.txt")
	if err := os.MkdirAll(filepath.Dir(normal), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(normal, []byte("normal file"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := env.doGet(t, "alice", "/download?filename=normal.txt")
	if code != http.StatusOK {
		t.Fatalf("普通下载应 200, got %d", code)
	}

	// 功能桶名首段在 user/ 桶内是合法用户路径：<root>/alice/user/cloud/t1/f.bin 可正常下载。
	cloudFile := filepath.Join(env.root, "alice", "user", "cloud", "t1", "f.bin")
	if err := os.MkdirAll(filepath.Dir(cloudFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cloudFile, []byte("cloud data"), 0o644); err != nil {
		t.Fatal(err)
	}
	code = env.doGet(t, "alice", "/download?filename=cloud/t1/f.bin")
	if code != http.StatusOK {
		t.Fatalf("功能桶名首段文件应 200, got %d", code)
	}
}
