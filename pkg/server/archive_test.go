// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchive_SingleFile(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("hello world")
	uploadFile(t, url, "test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	resp, err := http.Post(url+"/api/archive", "application/json", strings.NewReader(`{"files":["test.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("expected application/gzip, got %s", ct)
	}

	gr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()

	tr := tar.NewReader(gr)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if header.Name != "test.txt" {
		t.Fatalf("expected test.txt, got %s", header.Name)
	}
	content, _ := io.ReadAll(tr)
	if string(content) != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", string(content))
	}

	// 确保只有一个文件
	_, err = tr.Next()
	if err != io.EOF {
		t.Fatal("expected EOF, got more files")
	}
}

func TestArchive_MultipleFiles(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	uploadFile(t, url, "a.txt", []byte("aaa"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("aaa")),
	})
	uploadFile(t, url, "b.txt", []byte("bbb"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("bbb")),
	})
	// Upload to subdirectory
	uploadFile(t, url, "sub/c.txt", []byte("ccc"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("ccc")),
		"X-File-Path":     "sub/c.txt",
	})

	resp, err := http.Post(url+"/api/archive", "application/json", strings.NewReader(`{"files":["a.txt","b.txt","sub/c.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	gr, _ := gzip.NewReader(resp.Body)
	defer gr.Close()
	tr := tar.NewReader(gr)

	names := make(map[string]string)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		content, _ := io.ReadAll(tr)
		names[h.Name] = string(content)
	}

	if names["a.txt"] != "aaa" {
		t.Errorf("a.txt: expected aaa, got %s", names["a.txt"])
	}
	if names["b.txt"] != "bbb" {
		t.Errorf("b.txt: expected bbb, got %s", names["b.txt"])
	}
	if names["sub/c.txt"] != "ccc" {
		t.Errorf("sub/c.txt: expected ccc, got %s", names["sub/c.txt"])
	}
}

func TestArchive_InvalidPath(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/archive", "application/json", strings.NewReader(`{"files":["../etc/passwd"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for path traversal, got %d", resp.StatusCode)
	}
}

// TestArchive_RejectsInternalDirSource 验证读取侧守卫（审查 #4 收敛）：归档源不得引用
// 服务端内部目录（.__cloud__ 等只能经 kind=cloud_task 白名单由服务端按 owner 拼接）。
func TestArchive_RejectsInternalDirSource(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	for _, files := range []string{
		`{"files":[".__cloud__/some/file.bin"]}`,
		`{"files":["dir/.__versions__/v1"]}`,
	} {
		resp, err := http.Post(url+"/api/archive", "application/json", strings.NewReader(files))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("archive 源 %s 应 400（内部目录不可作归档源），got %d", files, resp.StatusCode)
		}
	}
}

func TestArchive_EmptyFiles(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Post(url+"/api/archive", "application/json", strings.NewReader(`{"files":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty files, got %d", resp.StatusCode)
	}
}

func TestArchiveDir_Success(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	uploadFile(t, url, "mydir/a.txt", []byte("aaa"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("aaa")),
		"X-File-Path":     "mydir/a.txt",
	})
	uploadFile(t, url, "mydir/sub/b.txt", []byte("bbb"), map[string]string{
		"X-File-Checksum": sha256hex([]byte("bbb")),
		"X-File-Path":     "mydir/sub/b.txt",
	})

	resp, err := http.Get(url + "/api/archive-dir?dirname=mydir")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	gr, _ := gzip.NewReader(resp.Body)
	defer gr.Close()
	tr := tar.NewReader(gr)

	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "mydir/a.txt" {
			found = true
			content, _ := io.ReadAll(tr)
			if string(content) != "aaa" {
				t.Errorf("mydir/a.txt: expected aaa, got %s", string(content))
			}
		}
	}
	if !found {
		t.Error("mydir/a.txt not found in archive")
	}
}

func TestArchiveDir_NotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/archive-dir?dirname=nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for nonexistent dir, got %d", resp.StatusCode)
	}
}

func TestArchiveDir_NotADir(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("file content")
	uploadFile(t, url, "afile.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	resp, err := http.Get(url + "/api/archive-dir?dirname=afile.txt")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-directory, got %d", resp.StatusCode)
	}
}

func TestArchiveDir_EmptyDirname(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/archive-dir?dirname=")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty dirname, got %d", resp.StatusCode)
	}
}

func TestArchive_PreservesMTime(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	body := []byte("hello world")
	uploadFile(t, url, "test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 获取上传后的文件 mtime
	info, _ := os.Stat(filepath.Join(cfgPtr.Load().UploadsDir, "test.txt"))
	originalMTime := info.ModTime()

	resp, err := http.Post(url+"/api/archive", "application/json",
		strings.NewReader(`{"files":["test.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	gr, _ := gzip.NewReader(resp.Body)
	defer gr.Close()
	tr := tar.NewReader(gr)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}

	diff := header.ModTime.Sub(originalMTime)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("tar header ModTime %v differs from original %v (diff: %v)",
			header.ModTime, originalMTime, diff)
	}
}
