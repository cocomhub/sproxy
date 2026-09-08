// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- Upload：卷上下文透传 + X-Volume 响应头 ----

func TestClient_Upload_WithVolumeContext_SendsVolumeField(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var gotVolume string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = r.ParseMultipartForm(1 << 20)
		gotVolume = r.FormValue("volume")
		w.Header().Set(headerVolume, "disk2")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok"})
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL, WithVolume("disk2"))
	res, err := c.Upload(context.Background(), src, "a.txt")
	if err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
	if gotVolume != "disk2" {
		t.Errorf("multipart volume field = %q, want disk2", gotVolume)
	}
	if res.Volume != "disk2" {
		t.Errorf("UploadResult.Volume = %q, want disk2（读取 X-Volume 头）", res.Volume)
	}
	if !res.Success {
		t.Errorf("res.Success = false, want true")
	}
}

func TestClient_Upload_NoVolumeContext_SendsNoVolumeField(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(src, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		if got := r.FormValue("volume"); got != "" {
			t.Errorf("auto 上传不应发送 volume 字段，got %q", got)
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok"})
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	if _, err := c.Upload(context.Background(), src, "a.txt"); err != nil {
		t.Fatalf("Upload failed: %v", err)
	}
}

// ---- List：解析 volume 字段 + 卷上下文透传 ----

func TestClient_List_ParsesVolumeField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("volume"); got != "" {
			t.Errorf("默认 list 不应携带 volume，got %q", got)
		}
		_, _ = io.WriteString(w, `{"files":[{"name":"a.txt","size":3,"checksum":"abc","volume":"disk2"},{"name":"old.txt","size":1}]}`)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	files, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
	if files[0].Volume != "disk2" {
		t.Errorf("files[0].Volume = %q, want disk2", files[0].Volume)
	}
	// 旧响应无 volume 字段 → 空（向后兼容）。
	if files[1].Volume != "" {
		t.Errorf("files[1].Volume = %q, want empty（无 volume 字段的旧响应）", files[1].Volume)
	}
}

func TestClient_List_WithVolumeContext_AddsQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("volume"); got != "disk2" {
			t.Errorf("list volume query = %q, want disk2", got)
		}
		_, _ = io.WriteString(w, `{"files":[]}`)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL, WithVolume("disk2"))
	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("List failed: %v", err)
	}
}

// ---- Stat / Delete / Rename：卷上下文透传（代表性断言） ----

func TestClient_Stat_WithVolumeContext_AddsQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("volume"); got != "disk2" {
			t.Errorf("stat volume query = %q, want disk2", got)
		}
		w.Header().Set(headerFileChecksum, "abc")
		w.Header().Set("X-File-Size", "5")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL, WithVolume("disk2"))
	info, err := c.Stat(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Checksum != "abc" {
		t.Errorf("info.Checksum = %q, want abc", info.Checksum)
	}
}

// ---- Volumes() / MoveVolume() / VolumeOf() ----

func TestClient_Volumes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/volumes" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"volumes":[{"name":"main","mode":"deny","capacity":100,"usage":10,"allowed":true},{"name":"disk2","mode":"deny","capacity":200,"usage":20,"allowed":true}]}`)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	vols, err := c.Volumes(context.Background())
	if err != nil {
		t.Fatalf("Volumes failed: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("len(vols) = %d, want 2", len(vols))
	}
	if vols[0].Name != "main" || vols[0].Capacity != 100 || vols[0].Usage != 10 || !vols[0].Allowed {
		t.Errorf("vols[0] = %+v, want main/100/10/allowed", vols[0])
	}
	if vols[1].Name != "disk2" || vols[1].Mode != "deny" {
		t.Errorf("vols[1] = %+v", vols[1])
	}
}

func TestClient_MoveVolume_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/volumes/move" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("from_volume") != "main" || q.Get("to_volume") != "disk2" || q.Get("filename") != "dir/a.txt" {
			t.Errorf("move query = %v, want from=main/to=disk2/file=dir/a.txt", q)
		}
		_, _ = io.WriteString(w, `{"success":true,"message":"文件已移动"}`)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	if err := c.MoveVolume(context.Background(), "main", "disk2", "dir/a.txt"); err != nil {
		t.Fatalf("MoveVolume failed: %v", err)
	}
}

func TestClient_MoveVolume_EmptyParamsError(t *testing.T) {
	c := NewFileClient("http://127.0.0.1:1")
	if err := c.MoveVolume(context.Background(), "", "disk2", "a.txt"); err == nil {
		t.Fatal("empty from_volume 应报错")
	}
	if err := c.MoveVolume(context.Background(), "main", "main", ""); err == nil {
		t.Fatal("empty filename 应报错")
	}
}

func TestClient_MoveVolume_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"success":false,"message":"volume not allowed"}`)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	err := c.MoveVolume(context.Background(), "main", "secret", "a.txt")
	if err == nil {
		t.Fatal("403 应返回错误")
	}
	if !strings.Contains(err.Error(), "跨卷移动失败") {
		t.Errorf("错误信息应包含上下文，got %v", err)
	}
}

func TestClient_VolumeOf_FindsVolume(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/files" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"files":[{"name":"dir","is_dir":true},{"name":"a.txt","size":3,"volume":"disk2"}]}`)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	vol, err := c.VolumeOf(context.Background(), "/dir/a.txt")
	if err != nil {
		t.Fatalf("VolumeOf failed: %v", err)
	}
	if vol != "disk2" {
		t.Errorf("VolumeOf = %q, want disk2", vol)
	}
}

func TestClient_VolumeOf_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"files":[]}`)
	}))
	defer srv.Close()

	c := NewFileClient(srv.URL)
	_, err := c.VolumeOf(context.Background(), "/dir/missing.txt")
	if err == nil {
		t.Fatal("找不到文件应返回错误")
	}
}
