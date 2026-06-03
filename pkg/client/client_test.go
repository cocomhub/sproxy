// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
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
		var allFiles []FileInfo
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, _ := e.Info()
			allFiles = append(allFiles, FileInfo{Name: e.Name(), Size: info.Size()})
		}

		offset := 0
		limit := 0
		fmt.Sscanf(r.URL.Query().Get("offset"), "%d", &offset)
		fmt.Sscanf(r.URL.Query().Get("limit"), "%d", &limit)

		var files []FileInfo
		if limit > 0 && offset < len(allFiles) {
			end := min(offset+limit, len(allFiles))
			files = allFiles[offset:end]
		} else {
			files = allFiles
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"files": files, "total": len(allFiles)})
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

	// search handler: GET /api/files/search?q=<keyword>
	mux.HandleFunc("GET /api/files/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if q == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"files": []FileInfo{}})
			return
		}
		q = strings.ToLower(q)
		var matches []FileInfo
		filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(dir, path)
			if strings.Contains(strings.ToLower(rel), q) {
				info, _ := d.Info()
				matches = append(matches, FileInfo{Name: filepath.ToSlash(rel), Size: info.Size()})
			}
			return nil
		})
		_ = json.NewEncoder(w).Encode(map[string]any{"files": matches})
	})

	// batch delete: POST /api/batch/delete
	mux.HandleFunc("POST /api/batch/delete", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var req struct {
			Files []struct {
				Filename string `json:"filename"`
				Checksum string `json:"checksum"`
			} `json:"files"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, `{"results":[]}`, http.StatusBadRequest)
			return
		}
		var results []map[string]any
		for _, f := range req.Files {
			p := filepath.Join(dir, f.Filename)
			err := os.Remove(p)
			r := map[string]any{"filename": f.Filename, "success": true, "message": "deleted"}
			if err != nil {
				r["success"] = false
				r["message"] = err.Error()
			}
			results = append(results, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	})

	// batch rename: POST /api/batch/rename
	mux.HandleFunc("POST /api/batch/rename", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		var req struct {
			Operations []struct {
				From     string `json:"from"`
				To       string `json:"to"`
				Checksum string `json:"checksum"`
			} `json:"operations"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, `{"results":[]}`, http.StatusBadRequest)
			return
		}
		var results []map[string]any
		for _, op := range req.Operations {
			if op.Checksum == "" {
				results = append(results, map[string]any{
					"filename": op.From, "success": false, "message": "missing checksum",
				})
				continue
			}
			fromPath := filepath.Join(dir, op.From)
			toPath := filepath.Join(dir, op.To)
			_ = os.MkdirAll(filepath.Dir(toPath), 0755)
			err := os.Rename(fromPath, toPath)
			r := map[string]any{"filename": op.From, "success": true, "message": "renamed"}
			if err != nil {
				r["success"] = false
				r["message"] = err.Error()
			}
			results = append(results, r)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
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

// TestClient_Search 测试 Search 方法的基本搜索功能。
func TestClient_Search(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)

	// 在服务端目录创建测试文件
	for _, name := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewFileClient(ts.URL)

	// 搜索 "beta" -> 只返回 beta.txt
	files, err := c.Search(context.Background(), "beta")
	if err != nil {
		t.Fatalf("Search beta: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	if files[0].Name != "beta.txt" {
		t.Fatalf("want beta.txt, got %s", files[0].Name)
	}

	// 搜索空字符串 -> 返回空列表
	files, err = c.Search(context.Background(), "")
	if err != nil {
		t.Fatalf("Search empty: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("want 0 files for empty search, got %d", len(files))
	}
}

// TestClient_BatchDelete 测试 BatchDelete 方法。
func TestClient_BatchDelete(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)

	// 在服务端目录创建多个测试文件
	for _, name := range []string{"del1.txt", "del2.txt", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// 先 stat 获取 checksum
	c := NewFileClient(ts.URL)
	info1, _ := c.Stat(context.Background(), "del1.txt")
	info2, _ := c.Stat(context.Background(), "del2.txt")

	results, err := c.BatchDelete(context.Background(), []BatchDeleteFile{
		{Filename: "del1.txt", Checksum: info1.Checksum},
		{Filename: "del2.txt", Checksum: info2.Checksum},
	})
	if err != nil {
		t.Fatalf("BatchDelete: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	for _, r := range results {
		if !r.Success {
			t.Fatalf("expected success for %s: %s", r.Filename, r.Message)
		}
	}

	// 确认文件已被删除
	if _, err := os.Stat(filepath.Join(dir, "del1.txt")); !os.IsNotExist(err) {
		t.Fatal("expected del1.txt to be deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "del2.txt")); !os.IsNotExist(err) {
		t.Fatal("expected del2.txt to be deleted")
	}
	// keep.txt 应保留
	if _, err := os.Stat(filepath.Join(dir, "keep.txt")); err != nil {
		t.Fatal("expected keep.txt to remain")
	}
}

// TestClient_BatchDelete_ContinueOnError 测试批量删除中单个失败不影响其余。
func TestClient_BatchDelete_ContinueOnError(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)

	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewFileClient(ts.URL)
	info, _ := c.Stat(context.Background(), "a.txt")

	// 在批量删除中包含不存在的文件
	results, err := c.BatchDelete(context.Background(), []BatchDeleteFile{
		{Filename: "a.txt", Checksum: info.Checksum},
		{Filename: "nonexistent.txt", Checksum: "deadbeef"},
	})
	if err != nil {
		t.Fatalf("BatchDelete: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	// 第一个应该成功
	if !results[0].Success {
		t.Fatalf("expected a.txt to succeed: %s", results[0].Message)
	}
	// 不存在的文件应该失败
	if results[1].Success {
		t.Fatal("expected nonexistent.txt to fail")
	}
}

// TestClient_BatchRename 测试 BatchRename 方法。
func TestClient_BatchRename(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)

	for _, name := range []string{"old1.txt", "old2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewFileClient(ts.URL)
	info1, _ := c.Stat(context.Background(), "old1.txt")
	info2, _ := c.Stat(context.Background(), "old2.txt")

	results, err := c.BatchRename(context.Background(), []BatchRenameOp{
		{From: "old1.txt", To: "new1.txt", Checksum: info1.Checksum},
		{From: "old2.txt", To: "new2.txt", Checksum: info2.Checksum},
	})
	if err != nil {
		t.Fatalf("BatchRename: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	for _, r := range results {
		if !r.Success {
			t.Fatalf("expected success for %s: %s", r.Filename, r.Message)
		}
	}

	// 验证新文件名存在，旧文件名不存在
	if _, err := os.Stat(filepath.Join(dir, "new1.txt")); err != nil {
		t.Fatal("expected new1.txt to exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "new2.txt")); err != nil {
		t.Fatal("expected new2.txt to exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "old1.txt")); !os.IsNotExist(err) {
		t.Fatal("expected old1.txt to be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "old2.txt")); !os.IsNotExist(err) {
		t.Fatal("expected old2.txt to be gone")
	}
}

// TestClient_BatchRename_MissingChecksum 测试缺少 checksum 时批量重命名继续处理。
func TestClient_BatchRename_MissingChecksum(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)

	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewFileClient(ts.URL)
	info, _ := c.Stat(context.Background(), "a.txt")

	// 第一条缺 checksum，第二条合法
	results, err := c.BatchRename(context.Background(), []BatchRenameOp{
		{From: "a.txt", To: "a_renamed.txt", Checksum: info.Checksum},
		{From: "b.txt", To: "b_renamed.txt", Checksum: ""},
	})
	if err != nil {
		t.Fatalf("BatchRename: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if !results[0].Success {
		t.Fatalf("expected a.txt to succeed: %s", results[0].Message)
	}
	if results[1].Success {
		t.Fatal("expected b.txt (empty checksum) to fail")
	}

	// a.txt 已改名，b.txt 不变
	if _, err := os.Stat(filepath.Join(dir, "a_renamed.txt")); err != nil {
		t.Fatal("expected a_renamed.txt to exist")
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal("expected b.txt to remain")
	}
}

// TestClient_ListWithPagination 测试分页列表功能。
func TestClient_ListWithPagination(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)

	// 创建 6 个文件
	for i := range 6 {
		name := fmt.Sprintf("file_%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewFileClient(ts.URL)

	// limit=3，第一页
	files, total, err := c.ListWithPagination(context.Background(), 0, 3)
	if err != nil {
		t.Fatalf("ListWithPagination: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 files on first page, got %d", len(files))
	}
	if total < 5 {
		t.Fatalf("expected total >= 6, got %d", total)
	}

	// limit=3，第二页 -> 应返回 3 个文件
	files, total, err = c.ListWithPagination(context.Background(), 3, 3)
	if err != nil {
		t.Fatalf("ListWithPagination page 2: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("want 3 files on second page, got %d", len(files))
	}

	// limit=5，offset=0 -> 返回 5 个文件
	files, total, err = c.ListWithPagination(context.Background(), 0, 5)
	if err != nil {
		t.Fatalf("ListWithPagination limit=5: %v", err)
	}
	if len(files) != 5 {
		t.Fatalf("want 5 files, got %d", len(files))
	}
}

// TestClient_ListWithPagination_NoLimit 测试不分页的情况。
func TestClient_ListWithPagination_NoLimit(t *testing.T) {
	t.Parallel()
	ts, dir := newMockServer(t)

	for i := range 4 {
		name := fmt.Sprintf("n%d.txt", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
	}

	c := NewFileClient(ts.URL)

	// limit=0 表示不限制
	files, total, err := c.ListWithPagination(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("ListWithPagination: %v", err)
	}
	if len(files) != 4 {
		t.Fatalf("want 4 files, got %d", len(files))
	}
	if total != 4 {
		t.Fatalf("want total=4, got %d", total)
	}
}

// TestClient_ShouldAutoChunk 测试 ShouldAutoChunk 的判断逻辑。
func TestClient_ShouldAutoChunk(t *testing.T) {
	t.Parallel()

	// 小文件 -> false
	if ShouldAutoChunk(1024) {
		t.Fatal("ShouldAutoChunk(1024) should be false")
	}

	// 刚好等于阈值 -> false
	if ShouldAutoChunk(size.AutoChunkThreshold) {
		t.Fatal("ShouldAutoChunk(AutoChunkThreshold) should be false")
	}

	// 超过阈值 -> true
	if !ShouldAutoChunk(size.AutoChunkThreshold + 1) {
		t.Fatal("ShouldAutoChunk(AutoChunkThreshold+1) should be true")
	}

	// 大文件 -> true
	if !ShouldAutoChunk(size.AutoChunkThreshold * 2) {
		t.Fatal("ShouldAutoChunk(AutoChunkThreshold*2) should be true")
	}
}
