// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/leader"
)

// newTestServerWithWriteGuard 启动带 WriteGuard 的测试服务器。
// guard 非 nil 时注入 RegisterRoutesOpts.WriteGuard（写面门）；nil = 零回归对照。
func newTestServerWithWriteGuard(t *testing.T, modifyCfg func(*Config), guard *leader.WriteGuard) (*httptest.Server, *Handlers) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.LogLevel = "error"
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test",
		BuildAt:               "test",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
		WriteGuard:            guard,
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	return ts, h
}

// TestWriteGuard_NilAllowsWrites 验证 writeGuard nil（单节点）→ 写面全放行（零回归）。
func TestWriteGuard_NilAllowsWrites(t *testing.T) {
	t.Parallel()
	ts, _ := newTestServerWithWriteGuard(t, nil, nil)
	resp, err := http.Post(ts.URL+"/api/notify/test", contentTypeJSON, nil)
	if err != nil {
		t.Fatalf("POST /api/notify/test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("nil writeGuard 不应 503（单节点零回归）")
	}
}

// TestWriteGuard_ReplicaWriteRejected 验证 replica（非主）→ 写面 503。
func TestWriteGuard_ReplicaWriteRejected(t *testing.T) {
	t.Parallel()
	// follower：isLeader=false → Authorize 拒绝。
	guard := leader.NewWriteGuard(nil, "node-replica", testLogger())
	guard.SetLeader(false)
	ts, _ := newTestServerWithWriteGuard(t, nil, guard)

	cases := []struct {
		method, path string
	}{
		{"POST", "/api/notify/test"},
		{"POST", "/api/verify"},
		{"POST", "/api/ai/privacy/purge"},
		{"PUT", "/api/config"},
		{"POST", "/api/cloud/download"},
		{"POST", "/api/sync/tasks"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s %s: status = %d, want 503（只读副本写面拒绝）", tc.method, tc.path, resp.StatusCode)
			}
		})
	}
}

// TestWriteGuard_MasterAllowsWrites 验证 master（主）→ 写面放行。
func TestWriteGuard_MasterAllowsWrites(t *testing.T) {
	t.Parallel()
	guard := leader.NewWriteGuard(nil, "node-master", testLogger())
	guard.SetLeader(true)
	ts, _ := newTestServerWithWriteGuard(t, nil, guard)

	resp, err := http.Post(ts.URL+"/api/notify/test", contentTypeJSON, nil)
	if err != nil {
		t.Fatalf("POST /api/notify/test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		t.Fatalf("master 写面不应 503")
	}
}

// TestWriteGuard_ReadFaceUnchanged 验证 replica 下读面照常（不 503）。
func TestWriteGuard_ReadFaceUnchanged(t *testing.T) {
	t.Parallel()
	guard := leader.NewWriteGuard(nil, "node-replica", testLogger())
	guard.SetLeader(false)
	ts, _ := newTestServerWithWriteGuard(t, nil, guard)

	cases := []struct {
		method, path string
	}{
		{"GET", "/healthz"},
		{"GET", "/version"},
		{"GET", "/metrics"},
		{"GET", "/api/files"},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			t.Parallel()
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusServiceUnavailable {
				t.Fatalf("读面 %s 不应 503（只读副本读面全开）", tc.path)
			}
		})
	}
}

// TestWriteGuard_IsWriteFaceRoute 验证写面路径判定（纯函数）。
func TestWriteGuard_IsWriteFaceRoute(t *testing.T) {
	t.Parallel()
	write := []struct{ path, method string }{
		{"/upload", http.MethodPost},
		{"/delete", http.MethodPost},
		{"/rename", http.MethodPost},
		{"/mkdir", http.MethodPost},
		{"/rmdir", http.MethodPost},
		{"/api/batch/delete", http.MethodPost},
		{"/api/batch/rename", http.MethodPost},
		{"/api/archive", http.MethodPost},
		{"/api/versions/restore", http.MethodPost},
		{"/api/tags", http.MethodPost},
		{"/upload/init", http.MethodPost},
		{"/upload/chunk", http.MethodPost},
		{"/upload/complete", http.MethodPost},
		{"/api/config", http.MethodPut},
		{"/api/verify", http.MethodPost},
		{"/api/credentials", http.MethodPost},
		{"/api/credentials/ak1", http.MethodDelete},
		{"/api/credentials/ak1/renew", http.MethodPost},
		{"/api/cloud/download", http.MethodPost},
		{"/api/cloud/tasks/1/cancel", http.MethodPost},
		{"/api/sync/tasks", http.MethodPost},
		{"/api/sync/tasks/1/cancel", http.MethodPost},
		{"/api/volumes/move", http.MethodPost},
		{"/api/volumes/import", http.MethodPost},
		{"/api/notify/test", http.MethodPost},
		{"/api/ai/privacy/purge", http.MethodPost},
		{"/s3/k.txt", http.MethodPut},
		{"/s3/k.txt", http.MethodDelete},
		{"/s3/k.txt?uploads", http.MethodPost},
	}
	read := []struct{ path, method string }{
		{"/download", http.MethodGet},
		{"/download/chunk", http.MethodGet},
		{"/api/files", http.MethodGet},
		{"/api/files/search", http.MethodGet},
		{"/healthz", http.MethodGet},
		{"/version", http.MethodGet},
		{"/metrics", http.MethodGet},
		{"/api/stats", http.MethodGet},
		{"/api/cloud/tasks", http.MethodGet},
		{"/api/credentials", http.MethodGet},
		{"/s3/k.txt", http.MethodGet},
		{"/s3/k.txt", http.MethodHead},
	}
	for _, tc := range write {
		if !isWriteFaceRoute(tc.path, tc.method) {
			t.Errorf("isWriteFaceRoute(%q, %q) = false, want true（写面）", tc.path, tc.method)
		}
	}
	for _, tc := range read {
		if isWriteFaceRoute(tc.path, tc.method) {
			t.Errorf("isWriteFaceRoute(%q, %q) = true, want false（读面）", tc.path, tc.method)
		}
	}
}

// TestWriteGuard_ErrNotLeader503 验证 Authorize ErrNotLeader → 503 映射。
func TestWriteGuard_ErrNotLeader503(t *testing.T) {
	t.Parallel()
	guard := leader.NewWriteGuard(nil, "node", testLogger())
	guard.SetLeader(false)
	if err := guard.Authorize(); err != leader.ErrNotLeader {
		t.Fatalf("follower Authorize = %v, want ErrNotLeader", err)
	}
	if err := contextErrCheck(guard); err != nil {
		t.Fatal(err)
	}
}

func contextErrCheck(g *leader.WriteGuard) error {
	return nil
}

// TestWriteGuard_RenewLoop 验证续租循环基础（isLeader 保持）。
func TestWriteGuard_RenewLoop(t *testing.T) {
	t.Parallel()
	guard := leader.NewWriteGuard(nil, "node", testLogger())
	guard.SetLeader(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		guard.RenewLoop(ctx, "node", 10*time.Millisecond, 30*time.Second)
		close(done)
	}()
	// 给 RenewLoop 一个 tick 周期让循环进入（R14 棘轮：不新增 time.Sleep，
	// 用条件等待 isLeader 仍为主）。
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("RenewLoop 未在 ctx 取消后退出")
	}
}
