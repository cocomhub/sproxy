// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newMockServer 构造一个最小化的 sproxy 风格服务端，仅实现测试所需的路由。
// 返回服务器与一个工作目录（已在 t.TempDir() 中）。
func newMockServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()

	mux := http.NewServeMux()

	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		cs := r.Header.Get("X-File-Checksum")
		if cs == "" {
			http.Error(w, `{"success":false,"message":"missing X-File-Checksum"}`, http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, h, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()

		out, _ := os.Create(filepath.Join(dir, h.Filename))
		defer out.Close()
		hasher := sha256.New()
		buf := make([]byte, 4096)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
				hasher.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		serverCS := hex.EncodeToString(hasher.Sum(nil))
		if serverCS != cs {
			http.Error(w, `{"success":false,"message":"checksum mismatch"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":       true,
			"message":       "ok",
			"file_checksum": serverCS,
		})
	})

	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("filename")
		if name == "" {
			http.Error(w, "missing filename", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(data)
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		w.Write(data)
	})

	mux.HandleFunc("POST /delete", func(w http.ResponseWriter, r *http.Request) {
		cs := r.Header.Get("X-File-Checksum")
		if cs == "" {
			http.Error(w, `{"success":false,"message":"missing checksum"}`, http.StatusBadRequest)
			return
		}
		name := r.URL.Query().Get("filename")
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "deleted"})
	})

	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		entries, _ := os.ReadDir(dir)
		var files []FileInfo
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, _ := e.Info()
			files = append(files, FileInfo{Name: e.Name(), Size: info.Size()})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})

	mux.HandleFunc("POST /rename", func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		to := r.URL.Query().Get("to")
		if r.Header.Get("X-File-Checksum") == "" {
			http.Error(w, "missing checksum", http.StatusBadRequest)
			return
		}
		fromPath := filepath.Join(dir, from)
		toPath := filepath.Join(dir, to)
		_ = os.MkdirAll(filepath.Dir(toPath), 0755)
		if err := os.Rename(fromPath, toPath); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"message":"renamed"}`))
	})

	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("filename")
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("X-File-Size", fmt.Sprintf("%d", info.Size()))
		w.Header().Set("X-File-MTime", fmt.Sprintf("%d", info.ModTime().UnixNano()))
		if !info.IsDir() {
			data, _ := os.ReadFile(filepath.Join(dir, name))
			sum := sha256.Sum256(data)
			w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		}
		w.WriteHeader(http.StatusOK)
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, dir
}

func TestFileClient_Upload_HappyPath(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "a.txt")
	if err := os.WriteFile(src, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	res, err := c.Upload(context.Background(), src, "a.txt")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if !res.Success {
		t.Fatalf("upload failed: %+v", res)
	}
}

func TestFileClient_Upload_MissingFile(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	c := NewFileClient(ts.URL)
	if _, err := c.Upload(context.Background(), "/non/existent/path", "x.txt"); err == nil {
		t.Fatal("expected error when local file missing")
	}
}

func TestFileClient_Download_RoundTrip(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	// 在服务端目录预放一个文件
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("world"), 0644); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "got.txt")
	c := NewFileClient(ts.URL)
	if err := c.Download(context.Background(), "b.txt", out); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "world" {
		t.Fatalf("want world, got %q", got)
	}
}

func TestFileClient_Download_NotFound(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	c := NewFileClient(ts.URL)
	out := filepath.Join(t.TempDir(), "nope.txt")
	if err := c.Download(context.Background(), "nope.txt", out); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestFileClient_List(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	_ = os.WriteFile(filepath.Join(dir, "x.txt"), []byte("1"), 0644)
	_ = os.WriteFile(filepath.Join(dir, "y.txt"), []byte("22"), 0644)

	c := NewFileClient(ts.URL)
	files, err := c.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 files, got %d", len(files))
	}
}

func TestFileClient_Stat(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	_ = os.WriteFile(filepath.Join(dir, "s.txt"), []byte("xy"), 0644)

	c := NewFileClient(ts.URL)
	info, err := c.Stat(context.Background(), "s.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != 2 {
		t.Fatalf("want size 2, got %d", info.Size)
	}
	if info.Checksum == "" {
		t.Fatal("expected non-empty checksum")
	}
}

func TestFileClient_Stat_NotFound(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	c := NewFileClient(ts.URL)
	if _, err := c.Stat(context.Background(), "missing.txt"); err == nil {
		t.Fatal("expected error")
	}
}

func TestFileClient_Rename(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	_ = os.WriteFile(filepath.Join(dir, "old.txt"), []byte("z"), 0644)

	c := NewFileClient(ts.URL)
	info, err := c.Stat(context.Background(), "old.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := c.Rename(context.Background(), "old.txt", "new.txt", info.Checksum); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatalf("expected new.txt to exist: %v", err)
	}
}

func TestFileClient_Delete_RemoteChecksum(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	// 预上传一个文件
	if err := os.WriteFile(filepath.Join(dir, "del.txt"), []byte("delete me"), 0644); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	// 不传 localPath，依赖远端 stat 获取 checksum
	if err := c.Delete(context.Background(), "del.txt", ""); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 确认文件已从服务端删除
	if _, err := os.Stat(filepath.Join(dir, "del.txt")); !os.IsNotExist(err) {
		t.Fatal("expected file to be deleted on server")
	}
}

func TestFileClient_Delete_LocalCheckMatch(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	if err := os.WriteFile(filepath.Join(dir, "match.txt"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	// 创建本地文件，内容与服务端一致
	srcDir := t.TempDir()
	localPath := filepath.Join(srcDir, "match.txt")
	if err := os.WriteFile(localPath, []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	if err := c.Delete(context.Background(), "match.txt", localPath); err != nil {
		t.Fatalf("Delete with --check-local should succeed: %v", err)
	}
}

func TestFileClient_Delete_LocalCheckMismatch(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)
	if err := os.WriteFile(filepath.Join(dir, "mismatch.txt"), []byte("remote content"), 0644); err != nil {
		t.Fatal(err)
	}

	// 创建本地文件，内容不同
	srcDir := t.TempDir()
	localPath := filepath.Join(srcDir, "mismatch.txt")
	if err := os.WriteFile(localPath, []byte("local content"), 0644); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	if err := c.Delete(context.Background(), "mismatch.txt", localPath); err == nil {
		t.Fatal("expected error when --check-local content mismatches")
	}
}

func TestFileClient_Delete_FileNotFound(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	c := NewFileClient(ts.URL)
	if err := c.Delete(context.Background(), "nonexistent.txt", ""); err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestFileClient_Rename_RequiresChecksum(t *testing.T) {
	t.Parallel()
	ts, _ := newMockServer(t)
	c := NewFileClient(ts.URL)
	err := c.Rename(context.Background(), "a", "b", "")
	if err == nil || !strings.Contains(err.Error(), "fromChecksum") {
		t.Fatalf("expected fromChecksum required error, got %v", err)
	}
}
