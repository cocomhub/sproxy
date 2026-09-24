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

// newDuMock 返回 mock 服务端：/api/du 回固定统计 JSON。
func newDuMock(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/du" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestFileClient_Du(t *testing.T) {
	t.Parallel()
	ts := newDuMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "dir/sub" {
			t.Errorf("path = %q, want dir/sub", r.URL.Query().Get("path"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"path": "user/dir/sub", "dirs": 3, "files": 7, "size": 12345},
		})
	})
	svc := NewFileClient(ts.URL)
	res, err := svc.Du(context.Background(), "dir/sub")
	if err != nil {
		t.Fatalf("Du: %v", err)
	}
	if res.Path != "user/dir/sub" || res.Dirs != 3 || res.Files != 7 || res.Size != 12345 {
		t.Fatalf("Du = %+v, want user/dir/sub/3/7/12345", res)
	}
}

func TestFileClient_DuRoot(t *testing.T) {
	t.Parallel()
	ts := newDuMock(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("path") != "" {
			t.Errorf("path = %q, want empty（卷根）", r.URL.Query().Get("path"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"data":    map[string]any{"path": "user", "dirs": 1, "files": 0, "size": 0},
		})
	})
	svc := NewFileClient(ts.URL)
	res, err := svc.Du(context.Background(), "")
	if err != nil {
		t.Fatalf("Du: %v", err)
	}
	if res.Path != "user" {
		t.Fatalf("Du.Path = %q, want user", res.Path)
	}
}

func TestFileClient_DuServerError(t *testing.T) {
	t.Parallel()
	ts := newDuMock(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"success":false,"message":"存储统计尚未完成"}`, http.StatusServiceUnavailable)
	})
	svc := NewFileClient(ts.URL)
	if _, err := svc.Du(context.Background(), "x"); err == nil {
		t.Fatal("Du 期望错误，got nil")
	}
}

func TestFileClient_DuSuccessFalse(t *testing.T) {
	t.Parallel()
	ts := newDuMock(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "遍历失败"})
	})
	svc := NewFileClient(ts.URL)
	if _, err := svc.Du(context.Background(), "x"); err == nil {
		t.Fatal("Du 期望 success=false 错误，got nil")
	}
}
