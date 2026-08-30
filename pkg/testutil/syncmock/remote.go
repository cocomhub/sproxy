// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package syncmock 提供同步测试用的 mock 远程 sproxy HTTP 服务
// （单层列表 + 分块上传管线），供 pkg/syncexec、pkg/server/syncmgr 等测试复用。
package syncmock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// RemoteFile 是 mock 远程上的一个文件。
type RemoteFile struct {
	Data     []byte
	MTime    int64
	Checksum string
}

// Remote 模拟远程 sproxy 文件 API（GET /api/files、HEAD /api/files/stat、
// GET /download、POST /upload、分块上传管线、/rename、/delete、/mkdir）。
type Remote struct {
	mu           sync.Mutex
	files        map[string]*RemoteFile // key = 正斜杠相对路径
	dirs         map[string]bool
	sessions     map[string]*session
	SleepOnChunk time.Duration // 测试用：chunk 上传时休眠（模拟慢传输）
}

type session struct {
	filename     string
	fileChecksum string
	fileModTime  int64
	chunkSize    int64
	chunks       map[int][]byte
}

// SHA256Hex 计算 data 的 SHA-256 hex。
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// NewServer 创建 mock 远程 HTTP 服务（t.Cleanup 自动关闭）。
func NewServer(t *testing.T) (*httptest.Server, *Remote) {
	t.Helper()
	m := &Remote{
		files:    make(map[string]*RemoteFile),
		dirs:     make(map[string]bool),
		sessions: make(map[string]*session),
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

// SeedFile 在 mock 远程上预置一个文件。
func (m *Remote) SeedFile(rel, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[rel] = &RemoteFile{Data: []byte(content), Checksum: SHA256Hex([]byte(content))}
}

// SeedDir 在 mock 远程上预置一个目录。
func (m *Remote) SeedDir(rel string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[rel] = true
}

// SnapshotFiles 返回 mock 远程当前全部文件的深拷贝。
func (m *Remote) SnapshotFiles() map[string]*RemoteFile {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*RemoteFile, len(m.files))
	for k, v := range m.files {
		cp := *v
		cp.Data = append([]byte(nil), v.Data...)
		out[k] = &cp
	}
	return out
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// listItem 是 mock 列表响应的条目（对齐服务端 /api/files 响应字段）。
type listItem struct {
	Name     string `json:"name"`
	Size     int    `json:"size,omitempty"`
	Checksum string `json:"checksum,omitempty"`
	ModTime  int64  `json:"mod_time,omitempty"`
	IsDir    bool   `json:"is_dir"`
}

func (m *Remote) handleList(w http.ResponseWriter, r *http.Request) {
	subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/")
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 1000
	}
	m.mu.Lock()
	var files []listItem
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
		files = append(files, listItem{
			Name: rel, Size: len(f.Data), Checksum: f.Checksum, ModTime: f.MTime,
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
		files = append(files, listItem{Name: rel, IsDir: true})
	}
	m.mu.Unlock()
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name < files[j].Name
	})
	total := len(files)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	files = files[offset:end]
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "total": total})
}

func (m *Remote) handleStat(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("filename")
	m.mu.Lock()
	defer m.mu.Unlock()
	if f, ok := m.files[name]; ok {
		w.Header().Set("X-File-Size", strconv.Itoa(len(f.Data)))
		w.Header().Set("X-File-Checksum", f.Checksum)
		w.Header().Set("X-File-MTime", strconv.FormatInt(f.MTime, 10))
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

func (m *Remote) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("filename")
	m.mu.Lock()
	f, ok := m.files[name]
	m.mu.Unlock()
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("X-File-Checksum", f.Checksum)
	_, _ = w.Write(f.Data)
}

func (m *Remote) handleUpload(w http.ResponseWriter, r *http.Request) {
	cs := r.Header.Get("X-File-Checksum")
	if cs == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "missing X-File-Checksum"})
		return
	}
	var mtime int64
	if s := r.Header.Get("X-File-MTime"); s != "" {
		_, _ = strconv.ParseInt(s, 10, 64)
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20) // mock 服务：固定 32 MiB 请求体上限
	//nolint:gosec // mock 测试基础设施：MaxBytesReader 已限 32 MiB，固定上限
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
	if SHA256Hex(data) != cs {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "checksum mismatch"})
		return
	}
	m.mu.Lock()
	m.files[strings.ReplaceAll(h.Filename, "\\", "/")] = &RemoteFile{Data: data, Checksum: cs, MTime: mtime}
	m.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "ok", "file_checksum": cs})
}

func (m *Remote) handleUploadStatus(w http.ResponseWriter, _ *http.Request) {
	// 简化：无会话返回 404，客户端 willContinue → 走 init 新会话
	http.Error(w, "no session", http.StatusNotFound)
}

func (m *Remote) handleUploadInit(w http.ResponseWriter, r *http.Request) {
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
	if f, ok := m.files[req.Filename]; ok && f.Checksum == req.FileChecksum {
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "upload_id": "already_exists"})
		return
	}
	m.sessions[req.UploadID] = &session{
		filename:     req.Filename,
		fileChecksum: req.FileChecksum,
		fileModTime:  req.FileModTime,
		chunkSize:    req.ChunkSize,
		chunks:       make(map[int][]byte),
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "upload_id": req.UploadID, "chunk_size": req.ChunkSize})
}

func (m *Remote) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	if m.SleepOnChunk > 0 {
		time.Sleep(m.SleepOnChunk)
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20) // mock 服务：固定 32 MiB 请求体上限
	//nolint:gosec // mock 测试基础设施：MaxBytesReader 已限 32 MiB，固定上限
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
	if SHA256Hex(data) != chunkCS {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "chunk checksum mismatch"})
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[uploadID]
	if !ok {
		http.Error(w, "no session", http.StatusNotFound)
		return
	}
	sess.chunks[chunkIdx] = data
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "should_retry": false, "message": "ok"})
}

func (m *Remote) handleUploadComplete(w http.ResponseWriter, r *http.Request) {
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
	cs := SHA256Hex(data)
	m.files[sess.filename] = &RemoteFile{Data: data, Checksum: cs, MTime: sess.fileModTime}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "upload_id": req.UploadID, "filename": sess.filename, "file_checksum": cs,
	})
}

func (m *Remote) handleRename(w http.ResponseWriter, r *http.Request) {
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
	if cs == "" || f.Checksum != cs {
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

func (m *Remote) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("filename")
	cs := r.Header.Get("X-File-Checksum")
	m.mu.Lock()
	defer m.mu.Unlock()
	f, ok := m.files[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "message": "文件不存在"})
		return
	}
	if cs != "" && f.Checksum != cs {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "message": "checksum mismatch"})
		return
	}
	delete(m.files, name)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "message": "ok"})
}

func (m *Remote) handleMkdir(w http.ResponseWriter, r *http.Request) {
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
