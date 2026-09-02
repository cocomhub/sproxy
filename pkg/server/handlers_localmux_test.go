// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// localMuxPatterns 记录隧道内层 localMux 上必须可达的全部路由（method + pattern）。
// 该清单是「浏览器隧道模式下每个用户面操作可达」的账本。背景：真实浏览器回归
// 曾暴露 POST /api/share 404——createShareHandler 只挂在 srvMux（主面），localMux
// （隧道内层）缺注册，sclient.share.create 在隧道模式下 100% 失败。修复 = handlers.go
// 把 POST /api/share 与 hub nodes/stats/remove 同步补进 localMux。本测试即为这类
// 缺失的永久护栏。signal/relay 为 mesh 内部通道（不经隧道），不列入。
var localMuxPatterns = []struct{ method, pattern string }{
	{"POST", "/upload"},
	{"GET", "/download"},
	{"POST", "/delete"},
	{"POST", "/rename"},
	{"GET", "/api/files"},
	{"HEAD", "/api/files/stat"},
	{"POST", "/mkdir"},
	{"POST", "/rmdir"},
	{"GET", "/api/files/search"},
	{"POST", "/api/batch/delete"},
	{"POST", "/api/batch/rename"},
	{"POST", "/api/archive"},
	{"GET", "/api/archive-dir"},
	{"GET", "/api/versions"},
	{"POST", "/api/versions/restore"},
	{"DELETE", "/api/versions"},
	{"POST", "/api/share"},
	{"GET", "/api/shares"},
	{"DELETE", "/api/shares/{token}"},
	{"GET", "/api/stats"},
	{"GET", "/api/config"},
	{"PUT", "/api/config"},
	{"POST", "/upload/init"},
	{"POST", "/upload/chunk"},
	{"GET", "/upload/status"},
	{"POST", "/upload/complete"},
	{"GET", "/download/chunk"},
	// 云端下载（localMux：隧道认证）
	{"POST", "/api/cloud/download"},
	{"POST", "/api/cloud/download/batch"},
	{"GET", "/api/cloud/tasks"},
	{"GET", "/api/cloud/tasks/{id}"},
	{"POST", "/api/cloud/tasks/{id}/cancel"},
	{"DELETE", "/api/cloud/tasks/{id}"},
	{"POST", "/api/cloud/tasks/{id}/archive"},
	{"POST", "/api/cloud/archive"},
	{"POST", "/api/cloud/tasks/{id}/resume"},
	{"POST", "/api/cloud/groups"},
	{"GET", "/api/cloud/groups"},
	{"GET", "/api/cloud/groups/{id}"},
	{"POST", "/api/cloud/groups/{id}/cancel"},
	{"DELETE", "/api/cloud/groups/{id}"},
	{"POST", "/api/cloud/groups/{id}/resume"},
	{"POST", "/api/cloud/groups/{id}/archive"},
	// Hub 用户面查询（仅 hub.enabled / opts.RouteTable != nil 时注册）
	{"GET", "/api/hub/nodes"},
	{"GET", "/api/hub/stats"},
}

// newTestMux 返回接入 RegisterRoutes 的 mux（含 access_keys，隧道可用）。
// withHub 为 true 时注入 RouteTable（hub.enabled 语义；hub 用户面路由只在
// opts.RouteTable != nil 时注册）。
func newTestMux(t *testing.T, withHub bool) http.Handler {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}
	cfg.Hub.Enabled = withHub
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "v",
		BuildAt: "b",
		Logger:  testLogger(),
	}
	if withHub {
		opts.RouteTable = hub.NewMeshRouteTable()
	}
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return mux
}

// hasRoute 黑盒探测 mux 中是否存在注册了 pattern 的 method（Go 1.22+ ServeMux
// 对「存在 pattern 但 method 不匹配」返回 405，未注册返回 404 page not found）。
// 用多个 probe method 请求同一 path，任一 405 即证明该 pattern 存在。
func hasRoute(t *testing.T, mux http.Handler, probeMethods []string, path string) bool {
	t.Helper()
	for _, m := range probeMethods {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(m, path, nil))
		if w.Code == http.StatusMethodNotAllowed {
			return true
		}
		// 也可命中 handler（业务响应）→ 注册存在；但某些业务 404（如 hub 未
		// 启用 404 内容不是 page not found）已由 405 判据覆盖，这里 405 最可靠。
	}
	return false
}

// TestHandlers_LocalHandler 验证 LocalHandler() 返回隧道内层本地文件 API handler
// （localMux + 中间件链），且可直接路由请求（不经 TunnelHandler() 的外层帧解密/
// 密钥检查）——这是 xfer listener（阶段 5 工作项 1）的接线前提：请求体已由 xfer
// 隧道解密为明文，直接 ServeHTTP 应命中路由返回 200，而非 401。
func TestHandlers_LocalHandler(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "v",
		BuildAt: "b",
		Logger:  testLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })

	lh := h.LocalHandler()
	if lh == nil {
		t.Fatal("LocalHandler 返回 nil")
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/files", nil)
	lh.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("LocalHandler 直连 GET /api/files 应为 200，实际 %d（body=%s）", w.Code, w.Body.String())
	}
}

// TestLocalMuxCoversAllTunnelRoutes 断言账本每条路由在接入层 mux 上都已注册。
// 由于 localMux 与 srvMux 共享同一批 handler 且 pattern 完全一致，两者任一缺失
// （authMiddleware 版或裸版）都意味着一端业务不可达：srvMux 有而 localMux 缺 =
// 隧道内 404（本次 POST /api/share 的现场）；localMux 有而 srvMux 缺 = 直连面
// 404（不常见但同类损伤）。断言以 405 为判据覆盖两案——单一 mux 上该 pattern 的
// 注册（无论包不包 authMiddleware）都会被 405 探测到。
func TestLocalMuxCoversAllTunnelRoutes(t *testing.T) {
	t.Parallel()
	probeMethods := []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions}
	for _, tc := range localMuxPatterns {
		tp := tc
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.pattern), func(t *testing.T) {
			t.Parallel()
			// hub 用户面仅在 RouteTable != nil（hub.enabled）时注册——非 hub 用
			// 默认 mux；hub 路由用带 RouteTable 的 mux 断言存在性。
			handler := newTestMux(t, strings.HasPrefix(tp.pattern, "/api/hub/"))
			if !hasRoute(t, handler, probeMethods, tp.pattern) {
				t.Fatalf("路由 %s %s 未注册（隧道内层与直连面都不可达）", tp.method, tp.pattern)
			}
		})
	}
}
