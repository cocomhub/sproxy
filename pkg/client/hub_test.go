// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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
		json.NewEncoder(w).Encode(map[string]any{"success": true, "max_storage_bytes": req.MaxStorageBytes})
	})

	// GET /api/hub/nodes
	mux.HandleFunc("GET /api/hub/nodes", func(w http.ResponseWriter, r *http.Request) {
		nodes := []HubNodeInfo{
			{ID: "node-1", Addr: "192.168.1.1:18083", Connected: "2026-07-26T00:00:00Z"},
			{ID: "node-2", Addr: "192.168.1.2:18083", Connected: "2026-07-26T00:00:00Z"},
		}
		json.NewEncoder(w).Encode(nodes)
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
	return ts, ts.URL
}

func TestUpdateStorageConfig(t *testing.T) {
	ts, url := hubTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	if err := client.UpdateStorageConfig(context.Background(), 1073741824); err != nil {
		t.Fatal(err)
	}
}

func TestListHubNodes(t *testing.T) {
	ts, url := hubTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	nodes, err := client.ListHubNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
	if nodes[0].ID != "node-1" {
		t.Fatalf("expected node-1, got %q", nodes[0].ID)
	}
}

func TestRemoveHubNode(t *testing.T) {
	ts, url := hubTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	if err := client.RemoveHubNode(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveHubNode_NotFound(t *testing.T) {
	ts, url := hubTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	err := client.RemoveHubNode(context.Background(), "notfound")
	if err == nil {
		t.Fatal("expected error for not found node")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestGetHubStats(t *testing.T) {
	ts, url := hubTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	stats, err := client.GetHubStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.NodesConnected != 2 {
		t.Fatalf("expected 2 nodes connected, got %d", stats.NodesConnected)
	}
}

func TestErrNotFound_Sentinel(t *testing.T) {
	ts, url := hubTestServer(t)
	defer ts.Close()

	client := NewFileClient(url)
	_, err := client.GetCloudTask(context.Background(), "notfound")
	if err == nil {
		t.Fatal("expected error for not found task")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}
