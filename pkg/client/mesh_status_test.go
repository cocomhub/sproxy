// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client_test

// mesh_status_test.go 钉住 `GET /api/mesh/status` 的客户端（W4：CLI `mesh status --server` 用）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

func TestMeshStatus_ParsesFacesAndNode(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	payload := map[string]any{
		"remote_read":  map[string]any{"enabled": true, "addr": "127.0.0.1:19000", "pinned": 2},
		"remote_write": map[string]any{"enabled": true, "addr": "127.0.0.1:19001", "pinned": 1},
		"node": map[string]any{"running": false, "node_id": "node-b", "webrtc": true,
			"services": []string{"volread", "volwrite"}},
		"hub_url":           "https://hub.example.com:18083",
		"signaling_enabled": true,
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/mesh/status" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer ts.Close()

	st, err := client.NewFileClient(ts.URL).MeshStatus(t.Context())
	if err != nil {
		t.Fatalf("MeshStatus: %v", err)
	}
	if st.RemoteRead == nil || st.RemoteRead.Addr != "127.0.0.1:19000" || st.RemoteRead.Pinned != 2 {
		t.Fatalf("remote_read 解析不符: %+v", st.RemoteRead)
	}
	if st.RemoteWrite == nil || st.RemoteWrite.Pinned != 1 {
		t.Fatalf("remote_write 解析不符: %+v", st.RemoteWrite)
	}
	if st.Node == nil || st.Node.Running || st.Node.NodeID != "node-b" || !st.Node.WebRTC || len(st.Node.Services) != 2 {
		t.Fatalf("node 解析不符: %+v", st.Node)
	}
	if st.HubURL != "https://hub.example.com:18083" || !st.SignalingEnabled {
		t.Fatalf("hub/signaling 解析不符: %+v", st)
	}
}

// TestMeshStatus_EmptyBodyIsFine 钉住「全关」场景：响应可能是 `{}`（旧服务端亦然）⇒ 空结构 + 无错误。
func TestMeshStatus_EmptyBodyIsFine(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	st, err := client.NewFileClient(ts.URL).MeshStatus(t.Context())
	if err != nil {
		t.Fatalf("空响应不应报错: %v", err)
	}
	if st.RemoteRead != nil || st.RemoteWrite != nil || st.Node != nil {
		t.Fatalf("空响应应得到空结构: %+v", st)
	}
}

// TestMeshStatus_Non200Errors 钉住非 200 报错（而非静默返回空结构）。
func TestMeshStatus_Non200Errors(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	if _, err := client.NewFileClient(ts.URL).MeshStatus(t.Context()); err == nil {
		t.Fatal("404 应报错")
	}
}
