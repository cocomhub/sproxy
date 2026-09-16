// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// healthProbesTestHandlers 装配一个最小 Handlers（探针测试共用）。
// livez 不依赖任何 store，最小装配即可；readyz/healthz 依赖 per-tenant
// UploadStore，走 RegisterRoutes 自动预创建 anonymous store 的默认路径。
func healthProbesTestHandlers(t *testing.T) *Handlers {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestLivez_AlwaysOK 验证 /livez 是纯进程存活探针：不访问任何外部依赖，恒 200 OK。
func TestLivez_AlwaysOK(t *testing.T) {
	t.Parallel()
	h := healthProbesTestHandlers(t)
	rr := httptest.NewRecorder()
	h.livez(rr, httptest.NewRequest("GET", "/livez", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("livez 应 200, got %d", rr.Code)
	}
	if rr.Body.String() != "OK" {
		t.Fatalf("livez body 应 OK, got %q", rr.Body.String())
	}
}

// TestReadyz_Healthy_200 验证就绪探针在 store 健康时返回 200。
func TestReadyz_Healthy_200(t *testing.T) {
	t.Parallel()
	h := healthProbesTestHandlers(t)
	rr := httptest.NewRecorder()
	h.readyz(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz 应 200, got %d", rr.Code)
	}
	if rr.Body.String() != "OK" {
		t.Fatalf("readyz body 应 OK, got %q", rr.Body.String())
	}
}

// TestReadyz_StoreStopped_503 验证任一 UploadStore 停止（Health 报错）时就绪探针 503，
// 与 /healthz 原语义一致。
func TestReadyz_StoreStopped_503(t *testing.T) {
	t.Parallel()
	h := healthProbesTestHandlers(t)
	// 停止 uploadStore 使其 Health() 返回错误（与 coverage_gaps_test.go 的
	// TestHealthz_UploadStoreStopped 同法）。
	_ = h.Close()

	rr := httptest.NewRecorder()
	h.readyz(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz 应 503, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "UploadStore:") {
		t.Fatalf("readyz body 应含 UploadStore 错误, got %q", rr.Body.String())
	}
}

// TestHealthz_StoreStopped_503 验证 /healthz 保持原语义：store 停止时仍 503（兼容现有监控）。
func TestHealthz_StoreStopped_503(t *testing.T) {
	t.Parallel()
	h := healthProbesTestHandlers(t)
	_ = h.Close()

	rr := httptest.NewRecorder()
	h.healthz(rr, httptest.NewRequest("GET", "/healthz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("healthz 应 503, got %d", rr.Code)
	}
}

// TestProbes_RoutesRegistered 验证 /livez、/readyz 与 /healthz 三个探针端点在主 mux
// 上真实注册可达（裸路由，无认证拦截）。
func TestProbes_RoutesRegistered(t *testing.T) {
	t.Parallel()
	h := healthProbesTestHandlers(t)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)

	// 每测试自建独立 client（禁共享 DefaultTransport——并行用例的 server.Close()
	// 会打断共享池在途连接）。
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)

	for _, path := range []string{"/livez", "/readyz", "/healthz"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s 应 200, got %d", path, resp.StatusCode)
		}
	}
}
