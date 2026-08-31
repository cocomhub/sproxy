// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// discardLogger 返回丢弃日志的测试 logger，避免测试输出噪音。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// ---------------------------------------------------------------------------
// httpMockFS：模拟远程 sproxy 文件 API 的内存实现（单层列表 + 分块上传管线）。
// ---------------------------------------------------------------------------

type mockFile struct {
	data     []byte
	mtime    int64
	checksum string
}

type mockSession struct {
	filename     string
	fileChecksum string
	fileModTime  int64
	chunkSize    int64
	chunks       map[int][]byte
}

type httpMockFS struct {
	mu       sync.Mutex
	files    map[string]*mockFile // key = 正斜杠相对路径
	dirs     map[string]bool      // key = 正斜杠相对路径
	sessions map[string]*mockSession

	// 失败注入（测试用）
	failStat       bool // Stat 返回 500
	failChunk      bool // 任一 chunk 返回 checksum mismatch
	noStatChecksum bool // Stat 不返回 X-File-Checksum（Rename checksum 为空场景）
	failList       bool // List 返回 500
}

func newHTTPMockFS(t *testing.T) (*httptest.Server, *httpMockFS) {
	t.Helper()
	m := &httpMockFS{
		files:    make(map[string]*mockFile),
		dirs:     make(map[string]bool),
		sessions: make(map[string]*mockSession),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", m.handleList)
	mux.HandleFunc("HEAD /api/files/stat", m.handleStat)
	mux.HandleFunc("GET /download", m.handleDownload)
	mux.HandleFunc("POST /upload", m.handleUpload)
	mux.HandleFunc("GET /upload/status", m.handleUploadStatus)
	mux.HandleFunc("POST /upload/init", m.handleUploadInit)
	mux.HandleFunc("POST /upload/chunk", m.handleUploadChunk)
	mux.HandleFunc("POST /upload/complete", m.handleUploadComplete)
	mux.HandleFunc("POST /rename", m.handleRename)
	mux.HandleFunc("POST /delete", m.handleDelete)
	mux.HandleFunc("POST /mkdir", m.handleMkdir)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, m
}

func (m *httpMockFS) seedFile(t *testing.T, rel, content string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[rel] = &mockFile{data: []byte(content), checksum: sha256Hex([]byte(content))}
}

func (m *httpMockFS) seedDir(t *testing.T, rel string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[rel] = true
}

func (m *httpMockFS) snapshotFiles() map[string]*mockFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*mockFile, len(m.files))
	for k, v := range m.files {
		cp := *v
		cp.data = append([]byte(nil), v.data...)
		out[k] = &cp
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *httpMockFS) handleList(w http.ResponseWriter, r *http.Request) {
	subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/")
	if m.failList {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "注入的 list 失败"})
		return
	}
	// 分页（对齐真实服务端 /api/files：默认 limit=1000，按 name 排序）
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 1000
	}
	m.mu.Lock()
	var files []map[string]any
	for p, f := range m.files {
		var rel string
		if subdir != "" {
			if !strings.HasPrefix(p, subdir+"/") {
				continue
			}
			rel = strings.TrimPrefix(p, subdir+"/")
		} else {
			rel = p
		}
		if strings.Contains(rel, "/") {
			continue // 非直接子项
		}
		files = append(files, map[string]any{
			"name":     rel,
			"size":     len(f.data),
			"checksum": f.checksum,
			"mod_time": f.mtime,
			"is_dir":   false,
		})
	}
	for d := range m.dirs {
		if d == subdir {
			continue
		}
		var rel string
		if subdir != "" {
			if !strings.HasPrefix(d, subdir+"/") {
				continue
			}
			rel = strings.TrimPrefix(d, subdir+"/")
		} else {
			rel = d
		}
		if strings.Contains(rel, "/") {
			continue
		}
		files = append(files, map[string]any{"name": rel, "is_dir": true})
	}
	m.mu.Unlock()
	sort.Slice(files, func(i, j int) bool {
		return files[i]["name"].(string) < files[j]["name"].(string)
	})
	total := len(files)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	files = files[offset:end]
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "total": total})
}

func (m *httpMockFS) handleStat(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("filename")
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failStat {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "message": "注入的 stat 失败"})
		return
	}
	if f, ok := m.files[name]; ok {
		w.Header().Set("X-File-Size", strconv.Itoa(len(f.data)))
		if !m.noStatChecksum {
			w.Header().Set("X-File-Checksum", f.checksum)
		}
		w.Header().Set("X-File-MTime", strconv.FormatInt(f.mtime, 10))
		w.Header().Set("X-File-IsDir", "false")
		w.WriteHeader(http.StatusOK)
		return
	}
	if m.dirs[name] {
		w.Header().Set("X-File-IsDir", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func (m *httpMockFS) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("filename")
	m.mu.Lock()
	f, ok := m.files[name]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("X-File-Checksum", f.checksum)
	w.Write(f.data)
}

func (m *httpMockFS) handleUpload(w http.ResponseWriter, r *http.Request) {
	cs := r.Header.Get("X-File-Checksum")
	if cs == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "missing X-File-Checksum"})
		return
	}
	var mtime int64
	if s := r.Header.Get("X-File-MTime"); s != "" {
		_, _ = fmt.Sscanf(s, "%d", &mtime)
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
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sha256Hex(data) != cs {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "checksum mismatch"})
		return
	}
	m.mu.Lock()
	m.files[filepathToSlash(h.Filename)] = &mockFile{data: data, checksum: cs, mtime: mtime}
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "ok", "file_checksum": cs})
}

func (m *httpMockFS) handleUploadStatus(w http.ResponseWriter, _ *http.Request) {
	// 简化：无会话返回 404，客户端 willContinue → 走 init 新会话
	http.Error(w, "no session", http.StatusNotFound)
}

func (m *httpMockFS) handleUploadInit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UploadID     string `json:"upload_id"`
		Filename     string `json:"filename"`
		TotalSize    int64  `json:"total_size"`
		ChunkSize    int64  `json:"chunk_size"`
		TotalChunks  int    `json:"total_chunks"`
		FileChecksum string `json:"file_checksum"`
		FileModTime  int64  `json:"file_mod_time"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.files[req.Filename]; ok && f.checksum == req.FileChecksum {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "upload_id": "already_exists"})
		return
	}
	m.sessions[req.UploadID] = &mockSession{
		filename:     req.Filename,
		fileChecksum: req.FileChecksum,
		fileModTime:  req.FileModTime,
		chunkSize:    req.ChunkSize,
		chunks:       make(map[int][]byte),
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "upload_id": req.UploadID, "chunk_size": req.ChunkSize})
}

func (m *httpMockFS) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	uploadID := r.FormValue("upload_id")
	chunkIdx, _ := strconv.Atoi(r.FormValue("chunk_index"))
	chunkCS := r.FormValue("chunk_checksum")
	f, _, err := r.FormFile("chunk")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if sha256Hex(data) != chunkCS {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "chunk checksum mismatch"})
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failChunk {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "should_retry": true, "message": "注入的 chunk 失败"})
		return
	}
	sess, ok := m.sessions[uploadID]
	if !ok {
		http.Error(w, "no session", http.StatusNotFound)
		return
	}
	sess.chunks[chunkIdx] = data
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "should_retry": false, "message": "ok"})
}

func (m *httpMockFS) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UploadID string `json:"upload_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[req.UploadID]
	if !ok {
		http.Error(w, "no session", http.StatusNotFound)
		return
	}
	delete(m.sessions, req.UploadID)
	idxs := make([]int, 0, len(sess.chunks))
	for i := range sess.chunks {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	var data []byte
	for _, i := range idxs {
		data = append(data, sess.chunks[i]...)
	}
	cs := sha256Hex(data)
	m.files[sess.filename] = &mockFile{data: data, checksum: cs, mtime: sess.fileModTime}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "upload_id": req.UploadID, "filename": sess.filename, "file_checksum": cs,
	})
}

func (m *httpMockFS) handleRename(w http.ResponseWriter, r *http.Request) {
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	cs := r.Header.Get("X-File-Checksum")
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[from]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "源文件不存在"})
		return
	}
	if cs == "" || f.checksum != cs {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "checksum mismatch"})
		return
	}
	if _, exists := m.files[to]; exists {
		writeJSON(w, http.StatusConflict, map[string]any{"success": false, "message": "目标路径已存在"})
		return
	}
	m.files[to] = f
	delete(m.files, from)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "ok"})
}

func (m *httpMockFS) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("filename")
	cs := r.Header.Get("X-File-Checksum")
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "文件不存在"})
		return
	}
	if f.checksum != cs {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "checksum mismatch"})
		return
	}
	delete(m.files, name)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "ok"})
}

func (m *httpMockFS) handleMkdir(w http.ResponseWriter, r *http.Request) {
	dirname := r.URL.Query().Get("dirname")
	if dirname == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "dirname 不能为空"})
		return
	}
	m.mu.Lock()
	m.dirs[dirname] = true
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "ok"})
}

// filepathToSlash 简易正斜杠转换（mock 内使用，避免依赖 filepath 的跨平台差异）。
func filepathToSlash(p string) string { return strings.ReplaceAll(p, "\\", "/") }

// ---------------------------------------------------------------------------
// HTTPTransport 测试
// ---------------------------------------------------------------------------

// newHTTPTransport 构造一个 Dial 直连 mock server 的 HTTPTransport（绕开 mesh）。
func newHTTPTransport(t *testing.T, srv *httptest.Server) *HTTPTransport {
	t.Helper()
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", srv.Listener.Addr().String())
	}
	tr, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL: srv.URL,
		Dial:    dial,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewHTTPTransport error: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func TestHTTPTransport_ListDir(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.seedFile(t, "a.txt", "hello")
	m.seedFile(t, "sub/b.txt", "world")
	m.seedDir(t, "sub")
	tr := newHTTPTransport(t, srv)

	entries, err := tr.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("根目录应有 2 个条目，got %d: %+v", len(entries), entries)
	}
	var file, dir *Entry
	for i := range entries {
		switch entries[i].Name {
		case "a.txt":
			file = &entries[i]
		case "sub":
			dir = &entries[i]
		}
	}
	if file == nil {
		t.Fatalf("缺少 a.txt 条目: %+v", entries)
	}
	if file.Path != "a.txt" || file.Size != 5 || file.Checksum != sha256Hex([]byte("hello")) || file.IsDir {
		t.Fatalf("a.txt 条目不符: %+v", file)
	}
	if dir == nil || !dir.IsDir || dir.Path != "sub" || dir.Checksum != "" {
		t.Fatalf("sub 应为目录条目: %+v", dir)
	}

	subEntries, err := tr.ListDir(context.Background(), "sub")
	if err != nil {
		t.Fatalf("ListDir(sub) error: %v", err)
	}
	if len(subEntries) != 1 {
		t.Fatalf("sub 下应有 1 个条目，got %d: %+v", len(subEntries), subEntries)
	}
	// 路径正斜杠契约：子目录条目的 Path 必须带 sub/ 前缀
	if subEntries[0].Path != "sub/b.txt" || subEntries[0].Name != "b.txt" {
		t.Fatalf("子目录条目路径应含 sub/ 前缀: %+v", subEntries[0])
	}
}

func TestHTTPTransport_Stat_File(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.seedFile(t, "data.txt", "payload")
	tr := newHTTPTransport(t, srv)

	e, err := tr.Stat(context.Background(), "data.txt")
	if err != nil {
		t.Fatalf("Stat error: %v", err)
	}
	if e == nil {
		t.Fatalf("Stat 应返回条目")
	}
	if e.Path != "data.txt" || e.Name != "data.txt" || e.Checksum != sha256Hex([]byte("payload")) || e.IsDir {
		t.Fatalf("Stat 条目不符: %+v", e)
	}
}

func TestHTTPTransport_Stat_Missing(t *testing.T) {
	srv, _ := newHTTPMockFS(t)
	tr := newHTTPTransport(t, srv)

	e, err := tr.Stat(context.Background(), "nope.txt")
	if err != nil {
		t.Fatalf("Stat 缺失不应返回 error，got %v", err)
	}
	if e != nil {
		t.Fatalf("Stat 缺失应返回 (nil, nil)，got %+v", e)
	}
}

func TestHTTPTransport_OpenRead(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.seedFile(t, "file.txt", "readme stream content")
	tr := newHTTPTransport(t, srv)

	rc, err := tr.OpenRead(context.Background(), "file.txt")
	if err != nil {
		t.Fatalf("OpenRead error: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if string(data) != "readme stream content" {
		t.Fatalf("OpenRead 内容不符: %q", data)
	}
}

func TestHTTPTransport_WriteFile_Empty(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	tr := newHTTPTransport(t, srv)

	// size==0：走轻量 multipart Upload（不走分块会话）
	if err := tr.WriteFile(context.Background(), "empty.txt", bytes.NewReader(nil), 0, 0); err != nil {
		t.Fatalf("WriteFile(empty) error: %v", err)
	}
	files := m.snapshotFiles()
	f, ok := files["empty.txt"]
	if !ok {
		t.Fatalf("empty.txt 应已创建")
	}
	if len(f.data) != 0 {
		t.Fatalf("empty.txt 内容应为空，got %q", f.data)
	}
}

func TestHTTPTransport_WriteFile_NonEmpty_PreservesMTime(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	tr := newHTTPTransport(t, srv)

	content := "hello chunked world"
	// 100ns 对齐：Windows NTFS/FAT 的 mtime 精度为 100ns，任意纳秒值会被截断
	const mtime = 1700000000123456700
	if err := tr.WriteFile(context.Background(), "sub/chunked.txt", strings.NewReader(content), int64(len(content)), mtime); err != nil {
		t.Fatalf("WriteFile(non-empty) error: %v", err)
	}
	files := m.snapshotFiles()
	f, ok := files["sub/chunked.txt"]
	if !ok {
		t.Fatalf("sub/chunked.txt 应已创建")
	}
	if string(f.data) != content {
		t.Fatalf("chunked 内容不符: got %q, want %q", f.data, content)
	}
	if f.checksum != sha256Hex([]byte(content)) {
		t.Fatalf("chunked checksum 不符: %s", f.checksum)
	}
	// mtime 保留：spool Chtimes → ChunkedUpload 用 spool ModTime 作 file_mod_time → 落盘
	if f.mtime != mtime {
		t.Fatalf("mtime 保留失败: got %d, want %d", f.mtime, mtime)
	}
}

func TestHTTPTransport_Rename(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.seedFile(t, "old.txt", "data")
	tr := newHTTPTransport(t, srv)

	if err := tr.Rename(context.Background(), "old.txt", "new.txt"); err != nil {
		t.Fatalf("Rename error: %v", err)
	}
	files := m.snapshotFiles()
	if _, ok := files["old.txt"]; ok {
		t.Fatalf("old.txt 应已被移走")
	}
	if f, ok := files["new.txt"]; !ok || string(f.data) != "data" {
		t.Fatalf("new.txt 应存在且内容正确: %+v", files["new.txt"])
	}
}

func TestHTTPTransport_Rename_MissingSource(t *testing.T) {
	srv, _ := newHTTPMockFS(t)
	tr := newHTTPTransport(t, srv)

	if err := tr.Rename(context.Background(), "no-such.txt", "x.txt"); err == nil {
		t.Fatalf("重命名不存在的源应报错")
	}
}

func TestHTTPTransport_Rename_Dir_Unsupported(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.seedDir(t, "somedir")
	tr := newHTTPTransport(t, srv)

	// 服务端 /rename 需要源文件 SHA-256 checksum；目录无 checksum → 明确报错
	if err := tr.Rename(context.Background(), "somedir", "somedir2"); err == nil {
		t.Fatalf("目录重命名应明确报错（服务端不支持）")
	}
}

func TestHTTPTransport_Delete(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.seedFile(t, "del.txt", "x")
	tr := newHTTPTransport(t, srv)

	if err := tr.Delete(context.Background(), "del.txt"); err != nil {
		t.Fatalf("Delete error: %v", err)
	}
	if _, ok := m.snapshotFiles()["del.txt"]; ok {
		t.Fatalf("del.txt 应已被删除")
	}
}

func TestHTTPTransport_MakeDir(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	tr := newHTTPTransport(t, srv)

	if err := tr.MakeDir(context.Background(), "newdir"); err != nil {
		t.Fatalf("MakeDir error: %v", err)
	}
	m.mu.Lock()
	exists := m.dirs["newdir"]
	m.mu.Unlock()
	if !exists {
		t.Fatalf("newdir 应已创建")
	}
}

func TestHTTPTransport_Close(t *testing.T) {
	srv, _ := newHTTPMockFS(t)
	tr := newHTTPTransport(t, srv)
	if err := tr.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// deadline 兜底（DoD 8）：对端停读/停发 → 请求超时失败，而非无限挂起。
// 说明（审查第二轮 Minor #2）：本测试用 GET（无 body），请求头极小不阻塞写，实际验证
// 的是 http.Transport 的 ResponseHeaderTimeout（读路径，内部 timer + pc.close，不依赖
// deadlineConn 包装）；活跃写超时（写路径对端停读）由 TestDeadlineConn_WriteTimeout_*
// （net.Pipe 单测）与 TestHTTPTransport_DialWrapsDeadline（包装与 WriteTimeout 传递）
// 覆盖。
// ---------------------------------------------------------------------------

// noDeadlineConn 模拟 mesh 流（MuxStreamConn）的 SetDeadline no-op 行为。
type noDeadlineConn struct {
	net.Conn
}

func (n *noDeadlineConn) SetDeadline(time.Time) error      { return nil }
func (n *noDeadlineConn) SetReadDeadline(time.Time) error  { return nil }
func (n *noDeadlineConn) SetWriteDeadline(time.Time) error { return nil }

func TestHTTPTransport_Deadline_ServerStopsReading(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen error: %v", err)
	}
	defer ln.Close()

	// 服务端 accept 后停读：保持连接打开但不消费任何字节，模拟对端背压。
	stop := make(chan struct{})
	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			<-stop
			_ = conn.Close()
		}
	}()
	defer close(stop)

	// Dial 返回「deadline no-op 的 mesh 流」。http.Transport 的 ResponseHeaderTimeout
	// 用内部 timer + pc.close()（不依赖 conn.SetDeadline），故本测试在 mesh 流上验证
	// 读路径超时端到端；活跃写超时由 deadlineConn 单测与 DialWrapsDeadline 覆盖。
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		c, derr := d.DialContext(ctx, "tcp", ln.Addr().String())
		if derr != nil {
			return nil, derr
		}
		return &noDeadlineConn{Conn: c}, nil
	}

	tr, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL:               "http://" + ln.Addr().String(),
		Dial:                  dial,
		ResponseHeaderTimeout: 300 * time.Millisecond,
		Logger:                discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewHTTPTransport error: %v", err)
	}
	defer func() { _ = tr.Close() }()

	start := time.Now()
	_, err = tr.ListDir(context.Background(), "")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("对端停读时请求应超时失败")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("请求未在 deadline 内失败（耗时 %v），deadline 兜底失效", elapsed)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("请求过早失败（%v），可能是连接层错误而非 ResponseHeaderTimeout", elapsed)
	}
}

// TestHTTPTransport_DialWrapsDeadline 验证 Dial 返回的连接被 deadlineConn 包装且
// WriteTimeout 正确传递（审查 I-2：Go 1.26 http.Transport HTTP/1.1 不调用
// SetReadDeadline/SetWriteDeadline，写路径对端停读靠 deadlineConn 活跃写超时兜底）。
func TestHTTPTransport_DialWrapsDeadline(t *testing.T) {
	clientSide, _ := net.Pipe()
	tr, err := NewHTTPTransport(HTTPTransportConfig{
		BaseURL:      "http://127.0.0.1:1",
		Dial:         func(ctx context.Context) (net.Conn, error) { return &noDeadlineConn{Conn: clientSide}, nil },
		WriteTimeout: 123 * time.Millisecond,
		Logger:       discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewHTTPTransport error: %v", err)
	}
	defer func() { _ = tr.Close() }()

	c, derr := tr.tr.DialContext(context.Background(), "tcp", "127.0.0.1:1")
	if derr != nil {
		t.Fatalf("DialContext error: %v", derr)
	}
	defer c.Close()
	tc, ok := c.(*trackedConn)
	if !ok {
		t.Fatalf("Dial 返回连接应为 trackedConn，got %T", c)
	}
	dc, ok := tc.Conn.(*deadlineConn)
	if !ok {
		t.Fatalf("trackedConn 底层应为 deadlineConn，got %T", tc.Conn)
	}
	if dc.writeTimeout != 123*time.Millisecond {
		t.Fatalf("writeTimeout 未传递: got %v, want 123ms", dc.writeTimeout)
	}
}

// TestHTTPTransport_Stat_ServerError 验证远程 Stat 500 → 报错（R-2）。
func TestHTTPTransport_Stat_ServerError(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.failStat = true
	tr := newHTTPTransport(t, srv)
	if _, err := tr.Stat(context.Background(), "x.txt"); err == nil {
		t.Fatalf("Stat 500 应返回 error")
	}
}

// TestHTTPTransport_ListDir_Pagination 验证大目录分页拉全（审查 C-1 回归）。
func TestHTTPTransport_ListDir_Pagination(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	for i := range 1100 {
		m.seedFile(t, fmt.Sprintf("f%04d.txt", i), "x")
	}
	tr := newHTTPTransport(t, srv)
	entries, err := tr.ListDir(context.Background(), "")
	if err != nil {
		t.Fatalf("ListDir error: %v", err)
	}
	if len(entries) != 1100 {
		t.Fatalf("分页应拉全 1100 个条目，got %d", len(entries))
	}
}

// TestHTTPTransport_WriteFile_ChunkedFailure 验证 chunk 校验失败 → WriteFile 报错（R-2）。
func TestHTTPTransport_WriteFile_ChunkedFailure(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.failChunk = true
	tr := newHTTPTransport(t, srv)
	content := strings.Repeat("x", 5<<20) // 5MB → 多 chunk
	if err := tr.WriteFile(context.Background(), "big.bin", strings.NewReader(content), int64(len(content)), 0); err == nil {
		t.Fatalf("chunk 失败时 WriteFile 应返回 error")
	}
}

// TestHTTPTransport_Rename_EmptyChecksum 验证源 checksum 为空 → Rename 报错（R-2）。
func TestHTTPTransport_Rename_EmptyChecksum(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.seedFile(t, "a.txt", "data")
	m.noStatChecksum = true
	tr := newHTTPTransport(t, srv)
	if err := tr.Rename(context.Background(), "a.txt", "b.txt"); err == nil {
		t.Fatalf("源 checksum 为空时 Rename 应报错")
	}
}

// TestHTTPTransport_WriteFile_Empty_PreservesMTime 验证空文件走 Upload 且 mtime 保留（R-2）。
func TestHTTPTransport_WriteFile_Empty_PreservesMTime(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	tr := newHTTPTransport(t, srv)
	const mtime = 1700000000123456700
	if err := tr.WriteFile(context.Background(), "empty.txt", bytes.NewReader(nil), 0, mtime); err != nil {
		t.Fatalf("WriteFile(empty+mtime) error: %v", err)
	}
	files := m.snapshotFiles()
	f, ok := files["empty.txt"]
	if !ok {
		t.Fatalf("empty.txt 应已创建")
	}
	if f.mtime != mtime {
		t.Fatalf("空文件 mtime 未保留: got %d, want %d", f.mtime, mtime)
	}
}

// TestHTTPTransport_ListDir_ServerError 验证远程 List 500 → 报错（审查第二轮 Minor #5）。
func TestHTTPTransport_ListDir_ServerError(t *testing.T) {
	srv, m := newHTTPMockFS(t)
	m.failList = true
	tr := newHTTPTransport(t, srv)
	if _, err := tr.ListDir(context.Background(), ""); err == nil {
		t.Fatalf("List 500 应返回 error")
	}
}

// TestHTTPTransport_Close_InterruptsInflight 验证 Close 中断 in-flight 请求（审查
// M-3 核心承诺：Close 不只是关空闲连接）。handler 等待 ctx 取消，Close 后请求快速失败。
func TestHTTPTransport_Close_InterruptsInflight(t *testing.T) {
	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-r.Context().Done() // 等待客户端关闭连接（Close 或请求超时）
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()
	tr := newHTTPTransport(t, srv)

	done := make(chan error, 1)
	go func() {
		_, err := tr.ListDir(context.Background(), "")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatalf("请求未建立连接")
	}
	_ = tr.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("Close 后 in-flight 请求应失败")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Close 未中断 in-flight 请求")
	}
}

// ---------------------------------------------------------------------------
// IsRetryableError：阶段 6 自动重试的可重试错误判别
// ---------------------------------------------------------------------------

func TestIsRetryableError(t *testing.T) {
	// 网络层错误（连接拒绝/重置/超时，net.Error）→ 可重试
	netErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}
	if !IsRetryableError(fmt.Errorf("列出远程目录失败: %w", netErr)) {
		t.Fatal("net.OpError（连接拒绝）应判为可重试")
	}

	// 超时（context.DeadlineExceeded）→ 可重试
	if !IsRetryableError(fmt.Errorf("枚举源目录失败: %w", context.DeadlineExceeded)) {
		t.Fatal("DeadlineExceeded 应判为可重试")
	}

	// 上下文取消（用户主动取消）→ 不可重试
	if IsRetryableError(context.Canceled) {
		t.Fatal("context.Canceled 不应判为可重试（用户取消）")
	}

	// HTTP 5xx（服务端瞬时故障）→ 可重试
	if !IsRetryableError(errors.New("请求失败 (HTTP 500): internal server error")) {
		t.Fatal("HTTP 500 应判为可重试")
	}
	if !IsRetryableError(fmt.Errorf("列出远程目录失败: %w", errors.New("请求失败 (HTTP 503): unavailable"))) {
		t.Fatal("HTTP 503（包装）应判为可重试")
	}

	// HTTP 4xx（请求本身有问题）→ 不可重试
	if IsRetryableError(errors.New("请求失败 (HTTP 404): not found")) {
		t.Fatal("HTTP 404 不应判为可重试")
	}

	// 业务/确定性错误（路径校验失败）→ 不可重试
	if IsRetryableError(errors.New("路径校验失败")) {
		t.Fatal("业务失败不应判为可重试")
	}

	// nil → 不可重试
	if IsRetryableError(nil) {
		t.Fatal("nil 不应判为可重试")
	}
}

// TestIsRetryableFileFailure 验证"全部文件传输失败"判定（审查 I-2）：
// completed + FilesTotal>0 + FilesDone==0 + 网络类 ActionError → 可重试；
// 业务性 ActionError（无网络特征）/ 部分成功 → 不可重试。
func TestIsRetryableFileFailure(t *testing.T) {
	cases := []struct {
		name string
		job  *Job
		want bool
	}{
		{
			name: "all-fail-network",
			job: &Job{
				Status: StatusCompleted,
				Stats:  Progress{FilesTotal: 3, FilesDone: 0},
				Results: []FileResult{
					{Path: "a.txt", Action: ActionError, Error: "写入目标失败: dial tcp 127.0.0.1:18083: connect: connection refused"},
					{Path: "b.txt", Action: ActionError, Error: "写入目标失败: i/o timeout"},
				},
			},
			want: true,
		},
		{
			name: "all-fail-business-no-network",
			job: &Job{
				Status: StatusCompleted,
				Stats:  Progress{FilesTotal: 2, FilesDone: 0},
				Results: []FileResult{
					{Path: "a.txt", Action: ActionError, Error: "写入目标失败: permission denied"},
					{Path: "b.txt", Action: ActionError, Error: "路径非法"},
				},
			},
			want: false, // 业务性全失败（无网络特征）：不整体重试
		},
		{
			name: "partial-success",
			job: &Job{
				Status: StatusCompleted,
				Stats:  Progress{FilesTotal: 2, FilesDone: 1},
				Results: []FileResult{
					{Path: "a.txt", Action: ActionError, Error: "写入目标失败: connection refused"},
				},
			},
			want: false, // 部分成功：不整体重试
		},
		{
			name: "no-files",
			job: &Job{
				Status: StatusCompleted,
				Stats:  Progress{FilesTotal: 0, FilesDone: 0},
			},
			want: false,
		},
		{
			name: "not-completed",
			job: &Job{
				Status: StatusFailed,
				Stats:  Progress{FilesTotal: 1, FilesDone: 0},
				Results: []FileResult{
					{Path: "a.txt", Action: ActionError, Error: "写入目标失败: connection refused"},
				},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableFileFailure(tc.job); got != tc.want {
				t.Fatalf("IsRetryableFileFailure = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestHTTPStatusFromErrorText_FileNameNoFalsePositive 验证（审查 M-1）：错误文本含
// 用户可控路径（文件名带 "HTTP 500"）时不误判为 5xx（LastIndex 从后部匹配）。
func TestHTTPStatusFromErrorText_FileNameNoFalsePositive(t *testing.T) {
	msg := `列出远程目录 "HTTP 500 weird" 失败: 请求失败 (HTTP 400): 路径非法`
	code, ok := httpStatusFromErrorText(msg)
	if !ok || code != 400 {
		t.Fatalf("应从末尾匹配真实状态 400，got code=%d ok=%v", code, ok)
	}
	// 文件名含 "HTTP 500" 不应误判为 500。
	if code >= 500 {
		t.Fatalf("文件名中的 HTTP 500 不应误判为服务端 5xx，got %d", code)
	}
}
