// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// testFedLogger 返回丢弃日志的 slog.Logger。
func testFedLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testFedPeer 构造一个返回固定节点表的 mock 联邦对端。
// 节点表内容由 resp 决定；fail 非 0 时返回该状态码（模拟拉取失败）。
func testFedPeer(t *testing.T, resp []map[string]string, fail *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && fail.Load() != 0 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFederationClient_SyncFromPeer：从 mock peer 拉取节点表成功，Candidates 返回节点。
func TestFederationClient_SyncFromPeer(t *testing.T) {
	peer := testFedPeer(t, []map[string]string{
		{"id": "node-b1", "addr": "192.168.1.2:9000", "mesh": "M"},
		{"id": "node-b2", "addr": "192.168.1.3:9000", "mesh": ""},
	}, nil)
	fc := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	cands := fc.Candidates()
	byID := make(map[hub.NodeID]hub.FederationNode, len(cands))
	for _, c := range cands {
		byID[c.ID] = c
	}
	if len(byID) != 2 {
		t.Fatalf("Candidates 应含 2 个节点, got %d: %+v", len(byID), cands)
	}
	if n := byID[hub.NodeID("node-b1")]; n.Addr != "192.168.1.2:9000" || n.Mesh != "M" {
		t.Errorf("node-b1 应保留 addr/mesh, got %+v", n)
	}
	if n := byID[hub.NodeID("node-b2")]; n.Mesh != "" {
		t.Errorf("node-b2 应为默认 mesh, got %+v", n)
	}
}

// TestFederationClient_StaleWhileError：拉取失败保留上次成功缓存，不返回错误清空。
func TestFederationClient_StaleWhileError(t *testing.T) {
	var fail atomic.Int32
	peer := testFedPeer(t, []map[string]string{
		{"id": "node-b1", "addr": "192.168.1.2:9000", "mesh": ""},
	}, &fail)
	fc := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("first SyncAll: %v", err)
	}
	fail.Store(1)
	if err := fc.SyncAll(context.Background()); err == nil {
		t.Fatalf("失败拉取应返回 error")
	}
	cands := fc.Candidates()
	if len(cands) != 1 || cands[0].ID != "node-b1" {
		t.Fatalf("失败后应保留上次成功缓存, got %+v", cands)
	}
}

// TestFederationClient_DefaultLoopbackURL：peer.URL 为空时回落默认 loopback 地址。
func TestFederationClient_DefaultLoopbackURL(t *testing.T) {
	fc := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB"}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)
	// 空 URL 应回落默认 loopback：拉取会连 127.0.0.1:18083（无服务 → 网络错误而非 URL 非法）。
	err := fc.SyncAll(context.Background())
	if err == nil {
		t.Fatalf("空 URL 回落默认 loopback 拉取应失败（无服务）")
	}
	if !strings.Contains(err.Error(), "127.0.0.1:18083") {
		t.Fatalf("默认 URL 应指向 127.0.0.1:18083, got err: %v", err)
	}
}

// TestFederationClient_MeshPreserved：peer 返回带 mesh 的节点，Candidates 保留 mesh 供隔离。
func TestFederationClient_MeshPreserved(t *testing.T) {
	peer := testFedPeer(t, []map[string]string{
		{"id": "node-m1", "addr": "10.0.0.1:9000", "mesh": "meshA"},
		{"id": "node-m2", "addr": "10.0.0.2:9000", "mesh": "meshB"},
	}, nil)
	fc := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	meshes := make(map[hub.NodeID]string)
	for _, c := range fc.Candidates() {
		meshes[c.ID] = c.Mesh
	}
	if meshes[hub.NodeID("node-m1")] != "meshA" || meshes[hub.NodeID("node-m2")] != "meshB" {
		t.Fatalf("mesh 应保留, got %+v", meshes)
	}
}

// TestFederationClient_DedupAcrossPeers：多 peer 返回同 (mesh,id) 节点时去重。
func TestFederationClient_DedupAcrossPeers(t *testing.T) {
	body := []map[string]string{{"id": "node-x", "addr": "10.0.0.1:9000", "mesh": "M"}}
	peerA := testFedPeer(t, body, nil)
	peerB := testFedPeer(t, body, nil)
	fc := hub.NewFederationClient([]hub.FederationPeer{
		{ID: "peerA", URL: peerA.URL},
		{ID: "peerB", URL: peerB.URL},
	}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	cands := fc.Candidates()
	if len(cands) != 1 {
		t.Fatalf("跨 peer 同节点应去重, got %d: %+v", len(cands), cands)
	}
}

// TestFederationClient_ConcurrentSync：并发 SyncAll 稳定（-race 覆盖）。
func TestFederationClient_ConcurrentSync(t *testing.T) {
	peerA := testFedPeer(t, []map[string]string{{"id": "node-a", "addr": "10.0.0.1:9000", "mesh": ""}}, nil)
	peerB := testFedPeer(t, []map[string]string{{"id": "node-b", "addr": "10.0.0.2:9000", "mesh": ""}}, nil)
	fc := hub.NewFederationClient([]hub.FederationPeer{
		{ID: "peerA", URL: peerA.URL},
		{ID: "peerB", URL: peerB.URL},
	}, 30*time.Second, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	ctx := context.Background()
	done := make(chan error, 4)
	for range 4 {
		go func() {
			done <- fc.SyncAll(ctx)
		}()
	}
	for range 4 {
		if err := <-done; err != nil {
			t.Fatalf("并发 SyncAll: %v", err)
		}
	}
	cands := fc.Candidates()
	if len(cands) != 2 {
		t.Fatalf("并发同步后应有 2 节点, got %d: %+v", len(cands), cands)
	}
}

// TestFederationClient_StartContextCancel：Start 启动后台拉取，ctx 取消后 goroutine 退出。
func TestFederationClient_StartContextCancel(t *testing.T) {
	peer := testFedPeer(t, []map[string]string{{"id": "node-b1", "addr": "192.168.1.2:9000", "mesh": ""}}, nil)
	fc := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: peer.URL}}, 10*time.Millisecond, 5*time.Second, testFedLogger())
	t.Cleanup(fc.Close)

	ctx, cancel := context.WithCancel(context.Background())
	fc.Start(ctx)
	// 等至少一轮拉取完成。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(fc.Candidates()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(fc.Candidates()) == 0 {
		t.Fatalf("Start 后应拉取到节点")
	}
	cancel()
	// ctx 取消后不 panic、Candidates 仍可读（goroutine 应退出）。
	time.Sleep(50 * time.Millisecond)
	_ = fc.Candidates()
}
