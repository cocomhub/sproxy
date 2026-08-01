// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// hubTestServer 返回一个模拟 Hub 和 StorageConfig handler 的测试服务器。
func hubTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	mux := http.NewServeMux()

	// PUT /api/storage/config
	mux.HandleFunc("PUT /api/storage/config", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxStorageBytes int64 `json:"max_storage_bytes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
			return
		}
		// 验证请求体关键字段
		if req.MaxStorageBytes <= 0 {
			http.Error(w, `{"error":"max_storage_bytes must be positive"}`, http.StatusBadRequest)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "max_storage_bytes": req.MaxStorageBytes})
	})

	// GET /api/hub/nodes
	mux.HandleFunc("GET /api/hub/nodes", func(w http.ResponseWriter, r *http.Request) {
		connectedTime, _ := time.Parse(time.RFC3339, "2026-07-26T00:00:00Z")
		json.NewEncoder(w).Encode([]HubNodeInfo{
			{ID: "node-1", Addr: "192.168.1.1:18083", Connected: connectedTime},
			{ID: "node-2", Addr: "192.168.1.2:18083", Connected: connectedTime},
		})
	})

	// DELETE /api/hub/nodes/{id}
	mux.HandleFunc("DELETE /api/hub/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "notfound" {
			http.Error(w, `{"error":"node not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "removed", "node": id})
	})

	// GET /api/hub/stats
	mux.HandleFunc("GET /api/hub/stats", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(HubStats{NodesConnected: 2})
	})

	// GET /api/cloud/tasks/{id} — 用于 ErrNotFound 测试
	mux.HandleFunc("GET /api/cloud/tasks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if id == "notfound" {
			http.Error(w, `{"error":"task not found"}`, http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(CloudTask{ID: id, Status: "completed"})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, ts.URL
}

func TestUpdateStorageConfig(t *testing.T) {
	t.Parallel()
	_, url := hubTestServer(t)

	client := NewFileClient(url)
	if err := client.UpdateStorageConfig(t.Context(), 1073741824); err != nil {
		t.Fatal(err)
	}
}

func TestListHubNodes(t *testing.T) {
	t.Parallel()
	_, url := hubTestServer(t)

	client := NewFileClient(url)
	nodes, err := client.ListHubNodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].ID != "node-1" {
		t.Fatalf("expected node-1, got %q", nodes[0].ID)
	}
	if nodes[0].Addr != "192.168.1.1:18083" {
		t.Errorf("expected Addr 192.168.1.1:18083, got %q", nodes[0].Addr)
	}
	if nodes[0].Connected.IsZero() {
		t.Error("expected non-zero Connected time")
	}
	if nodes[1].ID != "node-2" {
		t.Errorf("expected node-2, got %q", nodes[1].ID)
	}
	if nodes[1].Addr != "192.168.1.2:18083" {
		t.Errorf("expected Addr 192.168.1.2:18083, got %q", nodes[1].Addr)
	}
	if nodes[1].Connected.IsZero() {
		t.Error("expected non-zero Connected time")
	}
}

func TestRemoveHubNode(t *testing.T) {
	t.Parallel()
	_, url := hubTestServer(t)

	client := NewFileClient(url)
	if err := client.RemoveHubNode(t.Context(), "node-1"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveHubNode_NotFound(t *testing.T) {
	t.Parallel()
	_, url := hubTestServer(t)

	client := NewFileClient(url)
	err := client.RemoveHubNode(t.Context(), "notfound")
	if err == nil {
		t.Fatal("expected error for not found node")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGetHubStats(t *testing.T) {
	t.Parallel()
	_, url := hubTestServer(t)

	client := NewFileClient(url)
	stats, err := client.GetHubStats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodesConnected != 2 {
		t.Fatalf("expected 2 nodes connected, got %d", stats.NodesConnected)
	}
}

func TestErrNotFound_Sentinel(t *testing.T) {
	t.Parallel()
	_, url := hubTestServer(t)

	client := NewFileClient(url)
	_, err := client.GetCloudTask(t.Context(), "notfound")
	if err == nil {
		t.Fatal("expected error for not found task")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
