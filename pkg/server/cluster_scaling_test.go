// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// TestClusterConfig_Validate 验证集群段校验：NodeID 必填/Role 枚举/resync 非负。
func TestClusterConfig_Validate(t *testing.T) {
	t.Parallel()
	// 合法 master
	if err := (ClusterConfig{Enabled: true, NodeID: "n1", Role: "master"}).Validate(); err != nil {
		t.Fatalf("合法 master 应通过: %v", err)
	}
	// 合法 replica
	if err := (ClusterConfig{Enabled: true, NodeID: "r2", Role: "replica"}).Validate(); err != nil {
		t.Fatalf("合法 replica 应通过: %v", err)
	}
	// NodeID 空
	if err := (ClusterConfig{Enabled: true, Role: "master"}).Validate(); err == nil {
		t.Fatal("NodeID 空应拒绝")
	}
	// Role 非法
	if err := (ClusterConfig{Enabled: true, NodeID: "n1", Role: "bogus"}).Validate(); err == nil {
		t.Fatal("Role 非法应拒绝")
	}
	// resync 负
	if err := (ClusterConfig{Enabled: true, NodeID: "n1", Role: "master", ResyncInterval: -1}).Validate(); err == nil {
		t.Fatal("resync 负应拒绝")
	}
	// 未启用 → 任意配置通过（零回归）
	if err := (ClusterConfig{Enabled: false, Role: "bogus"}).Validate(); err != nil {
		t.Fatalf("未启用应通过: %v", err)
	}
}

// TestNodeRegistry_CASJoin 验证并发同 id 加入 → 恰一个成功。
func TestNodeRegistry_CASJoin(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(t.TempDir(), slog.Default())
	r := NewNodeRegistry(st, slog.Default())
	if err := r.Join(context.Background(), "node-1", "replica"); err != nil {
		t.Fatalf("首次 Join: %v", err)
	}
	// 同 id 二次加入（CAS joining→active 已被占）→ 失败
	if err := r.Join(context.Background(), "node-1", "replica"); err == nil {
		t.Fatal("同 id 二次加入应失败（CAS 仲裁）")
	}
}

// TestNodeRegistry_Lifecycle 验证状态机 joining→active→draining→leaving。
func TestNodeRegistry_Lifecycle(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(t.TempDir(), slog.Default())
	r := NewNodeRegistry(st, slog.Default())
	_ = r.Join(context.Background(), "node-1", "master")
	if err := r.SetStatus(context.Background(), "node-1", "draining"); err != nil {
		t.Fatalf("SetStatus draining: %v", err)
	}
	if err := r.SetStatus(context.Background(), "node-1", "leaving"); err != nil {
		t.Fatalf("SetStatus leaving: %v", err)
	}
	// 非法跳转（leaving → active）拒绝
	if err := r.SetStatus(context.Background(), "node-1", "active"); err == nil {
		t.Fatal("leaving → active 非法跳转应拒绝")
	}
}

// TestClusterNodesEndpoint 验证 /api/cluster/nodes：未装配 400 + 装配返回节点。
func TestClusterNodesEndpoint(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	// 未装配 → 400
	req := httptest.NewRequest(http.MethodGet, "/api/cluster/nodes", nil)
	rec := httptest.NewRecorder()
	h.handleClusterNodes(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未装配 code = %d, want 400", rec.Code)
	}
	// 装配 → 200 + 节点
	st := state.NewLocalStateStore(t.TempDir(), slog.Default())
	h.nodeRegistry = NewNodeRegistry(st, slog.Default())
	_ = h.nodeRegistry.Join(context.Background(), "node-1", "master")
	req2 := httptest.NewRequest(http.MethodGet, "/api/cluster/nodes", nil)
	rec2 := httptest.NewRecorder()
	h.handleClusterNodes(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("装配 code = %d, want 200", rec2.Code)
	}
	if !contains(rec2.Body.String(), "node-1") || !contains(rec2.Body.String(), "master") {
		t.Fatalf("响应缺节点: %s", rec2.Body.String())
	}
}

// TestClusterSelfEndpoint 验证 /api/cluster/self：未启用 400 + 启用返回身份。
func TestClusterSelfEndpoint(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	req := httptest.NewRequest(http.MethodGet, "/api/cluster/self", nil)
	rec := httptest.NewRecorder()
	h.handleClusterSelf(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未启用 code = %d, want 400", rec.Code)
	}
	// 启用 → cfgPtr 注入
	var cfgPtr atomic.Pointer[Config]
	cfg := Default()
	cfg.Cluster = ClusterConfig{Enabled: true, NodeID: "node-1", Role: "replica"}
	cfgPtr.Store(cfg)
	h.cfgPtr = &cfgPtr
	req2 := httptest.NewRequest(http.MethodGet, "/api/cluster/self", nil)
	rec2 := httptest.NewRecorder()
	h.handleClusterSelf(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("启用 code = %d, want 200", rec2.Code)
	}
	if !contains(rec2.Body.String(), "node-1") || !contains(rec2.Body.String(), "replica") {
		t.Fatalf("响应缺身份: %s", rec2.Body.String())
	}
}
