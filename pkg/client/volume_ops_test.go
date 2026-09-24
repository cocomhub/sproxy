// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newVolumeOpsMock 返回 mock 服务端：/api/volumes/{copy,move,rebalance} 固定回包。
func newVolumeOpsMock(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/volumes/copy":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "copied"})
		case "/api/volumes/move":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "moved"})
		case "/api/volumes/rebalance":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "rebalanced", "moved": 5, "bytes_moved": 1024, "remaining": 2048})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestFileClient_CopyVolume(t *testing.T) {
	t.Parallel()
	ts := newVolumeOpsMock(t)
	svc := NewFileClient(ts.URL)
	if err := svc.CopyVolume(context.Background(), "volA", "volB", "dir/f.txt"); err != nil {
		t.Fatalf("CopyVolume: %v", err)
	}
}

func TestFileClient_CopyVolumeMissingArgs(t *testing.T) {
	t.Parallel()
	svc := NewFileClient("http://127.0.0.1:1")
	if err := svc.CopyVolume(context.Background(), "", "b", "f"); err == nil {
		t.Fatal("CopyVolume 期望空参错误，got nil")
	}
}

func TestFileClient_MoveVolume(t *testing.T) {
	t.Parallel()
	ts := newVolumeOpsMock(t)
	svc := NewFileClient(ts.URL)
	if err := svc.MoveVolume(context.Background(), "volA", "volB", "f.txt"); err != nil {
		t.Fatalf("MoveVolume: %v", err)
	}
}

func TestFileClient_RebalanceVolume(t *testing.T) {
	t.Parallel()
	ts := newVolumeOpsMock(t)
	svc := NewFileClient(ts.URL)
	res, err := svc.RebalanceVolume(context.Background(), "volA", "volB", 0)
	if err != nil {
		t.Fatalf("RebalanceVolume: %v", err)
	}
	if res.Moved != 5 || res.BytesMoved != 1024 || res.Remaining != 2048 {
		t.Fatalf("Rebalance = %+v, want moved=5 bytes=1024 remaining=2048", res)
	}
}

func TestFileClient_RebalanceVolumeMissingArgs(t *testing.T) {
	t.Parallel()
	svc := NewFileClient("http://127.0.0.1:1")
	if _, err := svc.RebalanceVolume(context.Background(), "", "b", 0); err == nil {
		t.Fatal("RebalanceVolume 期望空参错误，got nil")
	}
}

func TestFileClient_RebalanceVolumeServerFalse(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "卷功能未装配"})
	}))
	defer ts.Close()
	svc := NewFileClient(ts.URL)
	if _, err := svc.RebalanceVolume(context.Background(), "a", "b", 0); err == nil {
		t.Fatal("RebalanceVolume 期望 success=false 错误，got nil")
	}
}
