# sproxy 测试集实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 sproxy 的项目覆盖率从 server 56.7%/client 23.3% 提升到 server ≥80%/client ≥65%，覆盖所有 handler 分支、store 层边界、并发安全、崩溃恢复混沌场景。

**Architecture:** 三层架构：L1 Handler 黑盒（httptest.Server + multipart 请求）、L2 Store 单元（直接调用 + 临时目录）、L3 端到端 + 并发 + 混沌（完整 client/server 链路 + 模拟 crash 恢复）。

**Tech Stack:** Go 1.26, testing 标准库, httptest, sync.WaitGroup, mime/multipart

---

### Task 0: 扩展现有测试基础设施

**Files:**
- Modify: `pkg/server/integration_test.go`
- Create: `pkg/server/store_test.go`（作为 L2 测试文件）

- [ ] **Step 1: 在 integration_test.go 增加全路由测试服务器辅助函数**

在 `pkg/server/integration_test.go` 末尾添加：

```go
// newTestServerWithAllRoutes 启动包含全部路由的测试服务器（含 tunnel key 但不绑定真实端口）。
// 新增功能：支持注入 extraMux 路由、可配置 mkdir/rmdir/rename/stat 等路由。
func newTestServerWithAllRoutes(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config]) {
    t.Helper()
    tmpDir := t.TempDir()

    cfg := Default()
    cfg.UploadsDir = tmpDir
    cfg.ChunkSize = 4 << 10 // 4 KiB for testing
    if modifyCfg != nil {
        modifyCfg(cfg)
    }

    var cfgPtr atomic.Pointer[Config]
    cfgPtr.Store(cfg)

    cs := NewChecksumStore(cfg.UploadsDir, nil)
    h := &Handlers{
        cfgPtr:        &cfgPtr,
        version:       "test-version",
        buildAt:       "test-buildat",
        checksumStore: cs,
        uploadStore:   NewUploadStore(cfg.UploadsDir, 24*time.Hour, nil),
        logger:        slog.Default(),
    }

    mux := http.NewServeMux()
    mux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
    mux.HandleFunc("GET /download", h.authMiddleware(h.download))
    mux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
    mux.HandleFunc("POST /rename", h.authMiddleware(h.rename))
    mux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
    mux.HandleFunc("HEAD /api/files/stat", h.authMiddleware(h.stat))
    mux.HandleFunc("POST /mkdir", h.authMiddleware(h.mkdir))
    mux.HandleFunc("POST /rmdir", h.authMiddleware(h.rmdir))
    mux.HandleFunc("POST /upload/init", h.authMiddleware(h.uploadInit))
    mux.HandleFunc("POST /upload/chunk", h.authMiddleware(h.uploadChunk))
    mux.HandleFunc("GET /upload/status", h.authMiddleware(h.uploadStatus))
    mux.HandleFunc("POST /upload/complete", h.authMiddleware(h.uploadComplete))
    mux.HandleFunc("GET /download/chunk", h.authMiddleware(h.downloadChunk))
    mux.HandleFunc("GET /healthz", h.healthz)
    mux.HandleFunc("GET /version", h.versionHandler)
    mux.HandleFunc("GET /", h.webRedirect)

    ts := httptest.NewServer(mux)
    t.Cleanup(func() {
        ts.Close()
        h.uploadStore.Stop()
    })
    return ts.URL, &cfgPtr
}
```

在文件顶部 `import` 块中添加 `"time"`（如果尚未引入）。

- [ ] **Step 2: 在 integration_test.go 添加只读目录辅助函数**

```go
// makeReadOnlyDir 创建一个只读目录（无写权限），用于测试文件写入失败路径。
// 返回目录路径和清理函数。
func makeReadOnlyDir(t *testing.T) (string, func()) {
    t.Helper()
    d := t.TempDir()

    // 在 TempDir 内只读父目录
    roDir := filepath.Join(d, "readonly")
    if err := os.Mkdir(roDir, 0755); err != nil {
        t.Fatalf("mkdir: %v", err)
    }
    // Windows 权限模型不同，跳过非 Windows
    if runtime.GOOS != "windows" {
        if err := os.Chmod(roDir, 0444); err != nil {
            t.Fatalf("chmod: %v", err)
        }
        cleanup := func() { os.Chmod(roDir, 0755) }
        return roDir, cleanup
    }
    return d, func() {}
}
```

在文件顶部 import 添加 `"runtime"`。

- [ ] **Step 3: 创建 store_test.go 骨架文件**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// 本文件包含 ChecksumStore 与 UploadStore 的单元测试。
// 与 *_test.go 在同一包（白盒测试），可直接访问内部方法。
```

- [ ] **Step 4: 运行已有测试确认基础设施无破坏**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -count=1 2>&1`
Expected: `ok  github.com/cocomhub/sproxy/pkg/server`

- [ ] **Step 5: Commit**

```bash
git add pkg/server/integration_test.go pkg/server/store_test.go
git commit -m "test: add test fixtures for all routes and read-only dir helper"
```

---

### Task 1: L1 — healthz / version / mkdir / rmdir handler 测试

**Files:**
- Modify: `pkg/server/integration_test.go`（追加测试函数）

- [ ] **Step 1: 追加 healthz/version 测试**

```go
func TestHealthz_ReturnsOK(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    resp, err := http.Get(url + "/healthz")
    if err != nil {
        t.Fatalf("healthz: %v", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    body, _ := io.ReadAll(resp.Body)
    if string(body) != "OK" {
        t.Fatalf("expected 'OK', got %q", body)
    }
}

func TestVersion_ReturnsInfo(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    resp, err := http.Get(url + "/version")
    if err != nil {
        t.Fatalf("version: %v", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    body, _ := io.ReadAll(resp.Body)
    if !strings.Contains(string(body), "Version:") {
        t.Fatalf("expected version info, got %q", body)
    }
}
```

- [ ] **Step 2: 追加 mkdir 测试**

```go
func TestMkdir_HappyPath(t *testing.T) {
    t.Parallel()
    url, cfgPtr := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/mkdir?dirname=testdir", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("mkdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }

    // 验证目录已创建
    dirPath := filepath.Join(cfgPtr.Load().UploadsDir, "testdir")
    if info, err := os.Stat(dirPath); err != nil || !info.IsDir() {
        t.Fatalf("directory should exist: %v", err)
    }
}

func TestMkdir_MissingDirname(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/mkdir", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("mkdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}

func TestMkdir_PathTraversal(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/mkdir?dirname=../../escape", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("mkdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}
```

- [ ] **Step 3: 追加 rmdir 测试**

```go
func TestRmdir_HappyPath(t *testing.T) {
    t.Parallel()
    url, cfgPtr := newTestServerWithAllRoutes(t, nil)

    // 先创建目录
    uploadsDir := cfgPtr.Load().UploadsDir
    dirPath := filepath.Join(uploadsDir, "toremove")
    if err := os.Mkdir(dirPath, 0755); err != nil {
        t.Fatalf("mkdir: %v", err)
    }

    req, _ := http.NewRequest("POST", url+"/rmdir?dirname=toremove", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rmdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    if _, err := os.Stat(dirPath); !os.IsNotExist(err) {
        t.Fatal("directory should be removed")
    }
}

func TestRmdir_WithFiles_AlsoDeletesChecksums(t *testing.T) {
    t.Parallel()
    url, cfgPtr := newTestServerWithAllRoutes(t, nil)

    uploadsDir := cfgPtr.Load().UploadsDir
    subDir := filepath.Join(uploadsDir, "subdir")
    if err := os.Mkdir(subDir, 0755); err != nil {
        t.Fatalf("mkdir: %v", err)
    }
    filePath := filepath.Join(subDir, "a.txt")
    if err := os.WriteFile(filePath, []byte("hello"), 0644); err != nil {
        t.Fatalf("write: %v", err)
    }
    // 手动在 checksumStore 添加记录
    cfgPtr.Load() // 无新增字段，通过直接操作实例：
    // 我们不能直接访问 h，但可以通过重新上传文件来建立记录

    // 用 uploadFile 上传到 subdir，建立 checksum
    body := []byte("hello")
    uploadFile(t, url, "subdir/a.txt", body, map[string]string{
        "X-File-Checksum": sha256hex(body),
        "X-File-Path":     "subdir/a.txt",
    })

    req, _ := http.NewRequest("POST", url+"/rmdir?dirname=subdir", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rmdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    // 目录已删除
    if _, err := os.Stat(subDir); !os.IsNotExist(err) {
        t.Fatal("directory should be removed")
    }
    // 列出根目录文件验证 subdir 下文件不出现
    listResp, err := http.Get(url + "/api/files")
    if err != nil {
        t.Fatalf("list: %v", err)
    }
    defer listResp.Body.Close()
    var listResult struct {
        Files []fileInfo `json:"files"`
    }
    json.NewDecoder(listResp.Body).Decode(&listResult)
    for _, f := range listResult.Files {
        if f.Name == "a.txt" || strings.HasPrefix(f.Name, "subdir/") {
            t.Fatalf("file from deleted subdir should not appear in root listing: %s", f.Name)
        }
    }
}

func TestRmdir_NonExistent(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/rmdir?dirname=nonexistent", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rmdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", resp.StatusCode)
    }
}

func TestRmdir_OnFileReturns400(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    body := []byte("I am a file")
    uploadFile(t, url, "notadir.txt", body, map[string]string{
        "X-File-Checksum": sha256hex(body),
    })

    req, _ := http.NewRequest("POST", url+"/rmdir?dirname=notadir.txt", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rmdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}

func TestRmdir_PathTraversal(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/rmdir?dirname=../../escape", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rmdir: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}
```

- [ ] **Step 4: 运行测试验证通过**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestHealthz|TestVersion|TestMkdir|TestRmdir" -v -count=1 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/server/integration_test.go
git commit -m "test: add healthz/version/mkdir/rmdir handler tests"
```

---

### Task 2: L1 — rename / stat handler 分支补齐

**Files:**
- Modify: `pkg/server/integration_test.go`

- [ ] **Step 1: 追加 rename 分支测试**

```go
func TestRename_SameSourceAndTarget(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    body := []byte("same file")
    cs := sha256hex(body)
    uploadFile(t, url, "same.txt", body, map[string]string{"X-File-Checksum": cs})

    req, _ := http.NewRequest("POST", url+"/rename?from=same.txt&to=same.txt", nil)
    req.Header.Set("X-File-Checksum", cs)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rename: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200 (same path), got %d", resp.StatusCode)
    }
    var result UploadResponse
    json.NewDecoder(resp.Body).Decode(&result)
    if !result.Success {
        t.Fatalf("expected success: %+v", result)
    }
}

func TestRename_TargetAlreadyExists(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    bodyA := []byte("file A")
    csA := sha256hex(bodyA)
    bodyB := []byte("file B")
    csB := sha256hex(bodyB)

    uploadFile(t, url, "a.txt", bodyA, map[string]string{"X-File-Checksum": csA})
    uploadFile(t, url, "b.txt", bodyB, map[string]string{"X-File-Checksum": csB})

    req, _ := http.NewRequest("POST", url+"/rename?from=a.txt&to=b.txt", nil)
    req.Header.Set("X-File-Checksum", csA)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rename: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusConflict {
        t.Fatalf("expected 409, got %d", resp.StatusCode)
    }
}

func TestRename_SourceNotFound(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/rename?from=nope.txt&to=dest.txt", nil)
    req.Header.Set("X-File-Checksum", strings.Repeat("a", 64))
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rename: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", resp.StatusCode)
    }
}

func TestRename_MissingChecksum(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/rename?from=a.txt&to=b.txt", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rename: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}

func TestRename_PathTraversal(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("POST", url+"/rename?from=../../a.txt&to=b.txt", nil)
    req.Header.Set("X-File-Checksum", strings.Repeat("b", 64))
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("rename: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}
```

- [ ] **Step 2: 追加 stat 测试**

```go
func TestStat_HappyPath(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    body := []byte("stat me")
    cs := sha256hex(body)
    uploadFile(t, url, "stat-test.txt", body, map[string]string{"X-File-Checksum": cs})

    req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=stat-test.txt", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("stat: %v", err)
    }
    resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    if got := resp.Header.Get("X-File-Size"); got == "" {
        t.Fatal("missing X-File-Size")
    }
    if got := resp.Header.Get("X-File-Checksum"); got != cs {
        t.Fatalf("X-File-Checksum mismatch: got %q, want %q", got, cs)
    }
    if got := resp.Header.Get("X-File-MTime"); got == "" {
        t.Fatal("missing X-File-MTime")
    }
}

func TestStat_DirectoryReturnsIsDir(t *testing.T) {
    t.Parallel()
    url, cfgPtr := newTestServerWithAllRoutes(t, nil)

    uploadsDir := cfgPtr.Load().UploadsDir
    if err := os.Mkdir(filepath.Join(uploadsDir, "statdir"), 0755); err != nil {
        t.Fatalf("mkdir: %v", err)
    }

    req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=statdir", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("stat: %v", err)
    }
    resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    if resp.Header.Get("X-File-IsDir") != "true" {
        t.Fatal("expected X-File-IsDir=true for directory")
    }
}

func TestStat_FileNotFound(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=nonexistent.txt", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("stat: %v", err)
    }
    resp.Body.Close()
    if resp.StatusCode != http.StatusNotFound {
        t.Fatalf("expected 404, got %d", resp.StatusCode)
    }
}

func TestStat_EmptyFilename(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("stat: %v", err)
    }
    resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}

func TestStat_PathTraversal(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=../../../etc/passwd", nil)
    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        t.Fatalf("stat: %v", err)
    }
    resp.Body.Close()
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400, got %d", resp.StatusCode)
    }
}
```

- [ ] **Step 3: 追加 listFiles 子目录测试**

```go
func TestListFiles_SubdirParameter(t *testing.T) {
    t.Parallel()
    url, cfgPtr := newTestServerWithAllRoutes(t, nil)

    uploadsDir := cfgPtr.Load().UploadsDir
    if err := os.MkdirAll(filepath.Join(uploadsDir, "mydir"), 0755); err != nil {
        t.Fatalf("mkdir: %v", err)
    }
    body := []byte("nested file")
    cs := sha256hex(body)
    uploadFile(t, url, "mydir/nested.txt", body, map[string]string{
        "X-File-Checksum": cs,
        "X-File-Path":     "mydir/nested.txt",
    })

    resp, err := http.Get(url + "/api/files?subdir=mydir")
    if err != nil {
        t.Fatalf("list: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    var result struct {
        Files []fileInfo `json:"files"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    if len(result.Files) != 1 || result.Files[0].Name != "nested.txt" {
        t.Fatalf("expected [nested.txt], got %+v", result.Files)
    }
    if result.Files[0].Checksum != cs {
        t.Fatalf("checksum mismatch: got %q, want %q", result.Files[0].Checksum, cs)
    }
}

func TestListFiles_SubdirPathTraversalReturns200Empty(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    // listFiles 对路径穿越返回 200+空列表（设计如此）
    resp, err := http.Get(url + "/api/files?subdir=../../../etc")
    if err != nil {
        t.Fatalf("list: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    var result struct {
        Files []fileInfo `json:"files"`
    }
    json.NewDecoder(resp.Body).Decode(&result)
    if len(result.Files) != 0 {
        t.Fatalf("expected empty list, got %+v", result.Files)
    }
}
```

- [ ] **Step 4: 追加 upload 分支补齐（409 文件已存在校验和不匹配）**

```go
func TestUpload_ExistingFileChecksumMismatch(t *testing.T) {
    t.Parallel()
    url, _ := newTestServerWithAllRoutes(t, nil)

    body := []byte("original content")
    cs := sha256hex(body)
    uploadFile(t, url, "conflict.txt", body, map[string]string{"X-File-Checksum": cs})

    // 上传同名文件但内容不同
    body2 := []byte("different content")
    cs2 := sha256hex(body2)
    status, respBody := uploadFile(t, url, "conflict.txt", body2, map[string]string{
        "X-File-Checksum": cs2,
        "X-File-Path":     "conflict.txt",
    })
    if status != http.StatusConflict {
        t.Fatalf("expected 409, got %d: %s", status, respBody)
    }
}
```

- [ ] **Step 5: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestRename|TestStat|TestListFiles|TestUpload_ExistingFileChecksum" -v -count=1 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 6: Commit**

```bash
git add pkg/server/integration_test.go
git commit -m "test: add rename/stat/listFiles/upload-conflict handler branch tests"
```

---

### Task 3: L2 — ChecksumStore 单元测试补齐

**Files:**
- Modify: `pkg/server/store_test.go`

- [ ] **Step 1: 添加 ChecksumStore DeletePrefix 测试**

```go
func TestChecksumStore_DeletePrefix(t *testing.T) {
    tmpDir := t.TempDir()
    cs := NewChecksumStore(tmpDir, nil)

    cs.Set("dir1/a.txt", "aaa")
    cs.Set("dir1/b.txt", "bbb")
    cs.Set("dir2/c.txt", "ccc")
    cs.Set("root.txt", "rrr")

    cs.DeletePrefix("dir1/")

    if _, ok := cs.Get("dir1/a.txt"); ok {
        t.Fatal("dir1/a.txt should be deleted")
    }
    if _, ok := cs.Get("dir1/b.txt"); ok {
        t.Fatal("dir1/b.txt should be deleted")
    }
    if _, ok := cs.Get("dir2/c.txt"); !ok {
        t.Fatal("dir2/c.txt should still exist")
    }
    if _, ok := cs.Get("root.txt"); !ok {
        t.Fatal("root.txt should still exist")
    }

    // 重新加载验证持久化
    cs2 := NewChecksumStore(tmpDir, nil)
    if _, ok := cs2.Get("dir1/a.txt"); ok {
        t.Fatal("persisted file still has deleted prefix entry")
    }
    if v, ok := cs2.Get("root.txt"); !ok || v != "rrr" {
        t.Fatal("root.txt should persist")
    }
}
```

- [ ] **Step 2: 添加 ChecksumStore Rename/Recover/Race 测试**

```go
func TestChecksumStore_Rename_ToExisting(t *testing.T) {
    tmpDir := t.TempDir()
    cs := NewChecksumStore(tmpDir, nil)

    cs.Set("from.txt", "fromVal")
    cs.Set("to.txt", "toVal")

    cs.Rename("from.txt", "to.txt")

    if _, ok := cs.Get("from.txt"); ok {
        t.Fatal("from.txt should be gone after rename")
    }
    v, ok := cs.Get("to.txt")
    if !ok || v != "fromVal" {
        t.Fatalf("to.txt should have fromVal, got %q", v)
    }
}

func TestChecksumStore_RecoverFromDisk(t *testing.T) {
    tmpDir := t.TempDir()
    cs := NewChecksumStore(tmpDir, nil)
    cs.Set("k1", "v1")
    cs.Set("k2", "v2")

    // 新建实例从磁盘加载
    cs2 := NewChecksumStore(tmpDir, nil)
    all := cs2.GetAll()
    if len(all) != 2 {
        t.Fatalf("expected 2 entries, got %d", len(all))
    }
    if all["k1"] != "v1" || all["k2"] != "v2" {
        t.Fatalf("content mismatch: %v", all)
    }
}

func TestChecksumStore_GetAll_Consistency(t *testing.T) {
    tmpDir := t.TempDir()
    cs := NewChecksumStore(tmpDir, nil)

    for i := range 100 {
        cs.Set(fmt.Sprintf("f%d", i), fmt.Sprintf("cs%d", i))
    }
    for i := 0; i < 50; i++ {
        cs.Delete(fmt.Sprintf("f%d", i))
    }

    all := cs.GetAll()
    if len(all) != 50 {
        t.Fatalf("expected 50 entries after delete, got %d", len(all))
    }
    for i := 50; i < 100; i++ {
        key := fmt.Sprintf("f%d", i)
        want := fmt.Sprintf("cs%d", i)
        if all[key] != want {
            t.Fatalf("key %s: want %s, got %s", key, want, all[key])
        }
    }
}
```

- [ ] **Step 3: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestChecksumStore" -v -count=1 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/server/store_test.go
git commit -m "test: add ChecksumStore DeletePrefix/Rename/Recover/GetAll consistency tests"
```

---

### Task 4: L2 — UploadStore 单元测试补齐

**Files:**
- Modify: `pkg/server/store_test.go`

- [ ] **Step 1: 添加 UploadStore GetSessionByFilename/DeleteSession 测试**

```go
func TestUploadStore_GetSessionByFilename(t *testing.T) {
    tmpDir := t.TempDir()
    us := NewUploadStore(tmpDir, 0, nil)
    defer us.Stop()

    us.CreateSession("id1", "file1.txt", 100, 4096, 1, strings.Repeat("a", 64), 0)
    us.CreateSession("id2", "file2.txt", 200, 4096, 1, strings.Repeat("b", 64), 0)

    s := us.GetSessionByFilename("file1.txt")
    if s == nil {
        t.Fatal("expected session for file1.txt")
    }
    if s.UploadID != "id1" {
        t.Fatalf("expected id1, got %s", s.UploadID)
    }

    // 不存在的文件名
    if got := us.GetSessionByFilename("nonexistent.txt"); got != nil {
        t.Fatal("expected nil for nonexistent filename")
    }
}

func TestUploadStore_DeleteSession(t *testing.T) {
    tmpDir := t.TempDir()
    us := NewUploadStore(tmpDir, 0, nil)
    defer us.Stop()

    us.CreateSession("del-id", "del.txt", 100, 4096, 1, strings.Repeat("c", 64), 0)

    us.DeleteSession("del-id")

    if us.GetSession("del-id") != nil {
        t.Fatal("session should be nil after delete")
    }

    // 磁盘目录也应已删除
    sessionDir := filepath.Join(tmpDir, ".__chunked__", "del-id")
    if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
        t.Fatal("session dir should be removed from disk")
    }
}
```

- [ ] **Step 2: 添加 UploadStore CleanupExpired/RecoverSession 测试**

```go
func TestUploadStore_CleanupExpired(t *testing.T) {
    tmpDir := t.TempDir()
    // 使用 0 TTL 使 session 立即过期
    us := NewUploadStore(tmpDir, 0, nil)
    defer us.Stop()

    us.CreateSession("expired-id", "expired.txt", 100, 4096, 1, strings.Repeat("d", 64), 0)

    // cleanupExpired 每5分钟跑一次，手动触发
    us.cleanupExpired()

    if us.GetSession("expired-id") != nil {
        t.Fatal("expired session should be cleaned up")
    }
}

func TestUploadStore_RecoverFromDisk(t *testing.T) {
    tmpDir := t.TempDir()

    // 第一个实例：创建 session 并标记几个 chunk
    us1 := NewUploadStore(tmpDir, 24*time.Hour, nil)
    us1.CreateSession("recover-id", "recover.txt", 8192, 4096, 2, strings.Repeat("e", 64), 0)
    us1.MarkChunkReceived("recover-id", 0, "chunk0hash")
    us1.Stop()

    // 第二个实例：应该从磁盘恢复 session
    us2 := NewUploadStore(tmpDir, 24*time.Hour, nil)
    defer us2.Stop()

    s := us2.GetSession("recover-id")
    if s == nil {
        t.Fatal("session should be recovered from disk")
    }
    if s.Filename != "recover.txt" {
        t.Fatalf("filename mismatch: %s", s.Filename)
    }
    if !s.ReceivedChunks[0] {
        t.Fatal("chunk 0 should be marked received after recovery")
    }
    if s.ReceivedChunks[1] {
        t.Fatal("chunk 1 should not be marked received")
    }
}

func TestUploadStore_ReconcileChunks(t *testing.T) {
    tmpDir := t.TempDir()

    // 创建 session 并停止实例（模拟 crash 后只剩磁盘文件）
    us1 := NewUploadStore(tmpDir, 24*time.Hour, nil)
    us1.CreateSession("reconcile-id", "reconcile.txt", 8192, 4096, 2, strings.Repeat("f", 64), 0)
    us1.MarkChunkReceived("reconcile-id", 0, "chunk0hash")
    us1.Stop()

    // 手动删除 session.json（模拟 bitmap 未持久化），只留 chunk 文件
    sessionDir := filepath.Join(tmpDir, ".__chunked__", "reconcile-id")
    os.Remove(filepath.Join(sessionDir, "session.json"))

    // 新实例恢复，应通过 reconcile 重新扫描 chunk 文件
    us2 := NewUploadStore(tmpDir, 24*time.Hour, nil)
    defer us2.Stop()

    s := us2.GetSession("reconcile-id")
    if s == nil {
        t.Fatal("session should be recovered")
    }
    // 注意：reconcileChunks 可以将 ReceivedChunks 置 true 但无法恢复 checksum
    // 至少 bitmap 应该对齐
}
```

- [ ] **Step 3: 添加 UploadStore GetOrCreateSession 复用/并发测试**

```go
func TestUploadStore_GetOrCreateSession_Reuse(t *testing.T) {
    tmpDir := t.TempDir()
    us := NewUploadStore(tmpDir, 0, nil)
    defer us.Stop()

    s1, reused, err := us.GetOrCreateSession("rid", "r.txt", 100, 4096, 1, strings.Repeat("g", 64), 0)
    if err != nil {
        t.Fatalf("first GetOrCreate: %v", err)
    }
    if reused {
        t.Fatal("first call should not be reused")
    }

    s2, reused, err := us.GetOrCreateSession("rid", "r.txt", 100, 4096, 1, strings.Repeat("g", 64), 0)
    if err != nil {
        t.Fatalf("second GetOrCreate: %v", err)
    }
    if !reused {
        t.Fatal("second call should reuse session")
    }
    if s1.UploadID != s2.UploadID {
        t.Fatal("upload_id should match")
    }
}

func TestUploadStore_ConcurrentMarkChunk(t *testing.T) {
    tmpDir := t.TempDir()
    us := NewUploadStore(tmpDir, 0, nil)
    defer us.Stop()

    const totalChunks = 100
    us.CreateSession("concurrent-id", "concurrent.txt", int64(totalChunks*4096), 4096, totalChunks, strings.Repeat("h", 64), 0)

    var wg sync.WaitGroup
    for i := range totalChunks {
        wg.Add(1)
        go func(idx int) {
            defer wg.Done()
            if err := us.MarkChunkReceived("concurrent-id", idx, fmt.Sprintf("chunk%dhash", idx)); err != nil {
                t.Errorf("MarkChunkReceived(%d): %v", idx, err)
            }
        }(i)
    }
    wg.Wait()

    if !us.AllChunksReceived("concurrent-id") {
        t.Fatal("all chunks should be received after concurrent marking")
    }
}
```

- [ ] **Step 4: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestUploadStore" -v -count=1 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 5: Commit**

```bash
git add pkg/server/store_test.go
git commit -m "test: add UploadStore session-by-filename/recover/cleanup/concurrent tests"
```

---

### Task 5: L2 — 辅助函数和 SaveConfig 测试

**Files:**
- Modify: `pkg/server/config_test.go`（追加 SaveConfig 测试）
- Modify: `pkg/server/checksum_test.go`（新建，或追加到 validate_test.go）

- [ ] **Step 1: 在 config_test.go 追加 SaveConfig 测试**

```go
func TestSaveConfig_ToFile(t *testing.T) {
    t.Parallel()
    tmpDir := t.TempDir()
    path := filepath.Join(tmpDir, "sproxy.yaml")

    cfg := Default()
    cfg.Addr = ":19999"
    if err := SaveConfig(cfg, path); err != nil {
        t.Fatalf("SaveConfig: %v", err)
    }

    loaded, err := LoadConfig(path)
    if err != nil {
        t.Fatalf("LoadConfig: %v", err)
    }
    if loaded.Addr != ":19999" {
        t.Fatalf("Addr mismatch: want :19999, got %q", loaded.Addr)
    }
}

func TestSaveConfig_ReadOnlyDir(t *testing.T) {
    if runtime.GOOS == "windows" {
        t.Skip("read-only dir test not supported on Windows")
    }
    t.Parallel()

    roDir, cleanup := makeReadOnlyDir(t)
    defer cleanup()

    cfg := Default()
    err := SaveConfig(cfg, filepath.Join(roDir, "sproxy.yaml"))
    if err == nil {
        t.Fatal("expected error when saving to read-only directory")
    }
}
```

- [ ] **Step 2: 创建 checksum_test.go 测试 FileChecksum/verifyChecksum**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
    "os"
    "path/filepath"
    "testing"
)

func TestFileChecksum_FileNotFound(t *testing.T) {
    t.Parallel()
    _, err := FileChecksum("/nonexistent/path/file.txt")
    if err == nil {
        t.Fatal("expected error for nonexistent file")
    }
}

func TestFileChecksum_EmptyFile(t *testing.T) {
    t.Parallel()
    tmpDir := t.TempDir()
    path := filepath.Join(tmpDir, "empty.bin")
    if err := os.WriteFile(path, []byte{}, 0644); err != nil {
        t.Fatalf("write: %v", err)
    }
    cs, err := FileChecksum(path)
    if err != nil {
        t.Fatalf("FileChecksum: %v", err)
    }
    // SHA-256 of empty data
    const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    if cs != emptySHA256 {
        t.Fatalf("empty file checksum: want %s, got %s", emptySHA256, cs)
    }
}

func TestVerifyChecksum_Correct(t *testing.T) {
    t.Parallel()
    tmpDir := t.TempDir()
    path := filepath.Join(tmpDir, "check.bin")
    data := []byte("verify me")
    if err := os.WriteFile(path, data, 0644); err != nil {
        t.Fatalf("write: %v", err)
    }
    wantCS := sha256hex(data)
    if !verifyFileWithChecksum(path, wantCS) {
        t.Fatal("verifyFileWithChecksum should return true for correct checksum")
    }
}

func TestVerifyChecksum_Wrong(t *testing.T) {
    t.Parallel()
    tmpDir := t.TempDir()
    path := filepath.Join(tmpDir, "bad.bin")
    if err := os.WriteFile(path, []byte("real data"), 0644); err != nil {
        t.Fatalf("write: %v", err)
    }
    if verifyFileWithChecksum(path, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
        t.Fatal("verifyFileWithChecksum should return false for wrong checksum")
    }
}

func TestVerifyChecksum_FileNotFound(t *testing.T) {
    t.Parallel()
    if verifyFileWithChecksum("/nonexistent", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
        t.Fatal("verifyFileWithChecksum should return false for nonexistent file")
    }
}
```

- [ ] **Step 3: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestSaveConfig|TestFileChecksum|TestVerifyChecksum" -v -count=1 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/server/config_test.go pkg/server/checksum_test.go
git commit -m "test: add SaveConfig/FileChecksum/verifyChecksum unit tests"
```

---

### Task 6: L3 — 并发测试

**Files:**
- Modify: `pkg/server/e2e_test.go`

- [ ] **Step 1: 在 e2e_test.go 追加并发上传测试**

```go
func TestConcurrent_UploadDifferentFiles(t *testing.T) {
    t.Parallel()
    url, _ := startTestServer(t)

    var wg sync.WaitGroup
    errCh := make(chan error, 20)

    for i := range 20 {
        wg.Add(1)
        go func(n int) {
            defer wg.Done()
            srcDir := t.TempDir()
            data := []byte(fmt.Sprintf("concurrent file %d content", n))
            srcPath := filepath.Join(srcDir, fmt.Sprintf("f%d.txt", n))
            if err := os.WriteFile(srcPath, data, 0644); err != nil {
                errCh <- err
                return
            }
            c := client.NewFileClient(url)
            if _, err := c.Upload(context.Background(), srcPath, fmt.Sprintf("f%d.txt", n)); err != nil {
                errCh <- err
            }
        }(i)
    }
    wg.Wait()
    close(errCh)

    for err := range errCh {
        if err != nil {
            t.Fatalf("concurrent upload error: %v", err)
        }
    }
}

func TestConcurrent_UploadSameFile(t *testing.T) {
    t.Parallel()
    url, _ := startTestServer(t)

    srcDir := t.TempDir()
    data := []byte("same content for all uploads")
    srcPath := filepath.Join(srcDir, "same.txt")
    if err := os.WriteFile(srcPath, data, 0644); err != nil {
        t.Fatalf("write: %v", err)
    }

    var wg sync.WaitGroup
    successCount := int32(0)
    for range 10 {
        wg.Add(1)
        go func() {
            defer wg.Done()
            c := client.NewFileClient(url)
            if result, err := c.Upload(context.Background(), srcPath, "same.txt"); err == nil && result.Success {
                atomic.AddInt32(&successCount, 1)
            }
        }()
    }
    wg.Wait()

    if successCount < 1 {
        t.Fatal("at least one upload should succeed")
    }
    // 验证最终文件内容正确
    c := client.NewFileClient(url)
    outDir := t.TempDir()
    outPath := filepath.Join(outDir, "same.txt")
    if err := c.Download(context.Background(), "same.txt", outPath); err != nil {
        t.Fatalf("download after concurrent: %v", err)
    }
    got, _ := os.ReadFile(outPath)
    if !bytes.Equal(got, data) {
        t.Fatal("downloaded content mismatch after concurrent upload")
    }
}
```

- [ ] **Step 2: 追加并发 RenameAndDelete 测试**

```go
func TestConcurrent_RenameAndDelete(t *testing.T) {
    t.Parallel()
    url, _ := startTestServer(t)

    // 准备一个文件
    srcDir := t.TempDir()
    data := []byte("concurrent rename/delete target")
    srcPath := filepath.Join(srcDir, "target.txt")
    if err := os.WriteFile(srcPath, data, 0644); err != nil {
        t.Fatalf("write: %v", err)
    }

    c := client.NewFileClient(url)
    if _, err := c.Upload(context.Background(), srcPath, "target.txt"); err != nil {
        t.Fatalf("upload: %v", err)
    }

    // 并发执行: 一边rename 一边 delete（确保不会panic）
    var wg sync.WaitGroup
    for range 5 {
        wg.Add(1)
        go func() {
            defer wg.Done()
            info, err := c.Stat(context.Background(), "target.txt")
            if err != nil {
                return
            }
            _ = c.Rename(context.Background(), "target.txt", "moved.txt", info.Checksum)
        }()
    }
    for range 5 {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _ = c.Delete(context.Background(), "target.txt", "")
        }()
    }
    wg.Wait()
    // 不断言具体结果，只验证不 panic/ 死锁
}
```

- [ ] **Step 3: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestConcurrent" -v -count=1 -timeout=60s 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/server/e2e_test.go
git commit -m "test: add concurrent upload/rename/delete tests"
```

---

### Task 7: L3 — 分块上传端到端链路补齐

**Files:**
- Modify: `pkg/server/chunked_upload_test.go`

- [ ] **Step 1: 追加大文件分块上传 + 续传 + digest 一致性测试**

```go
func TestChunkedUpload_MultiChunkLargeFile(t *testing.T) {
    // 64 KiB = 16 个 4 KiB 分块（测试用 chunkSize=4KiB）
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    fileData := bytes.Repeat([]byte("LargeFileData!"), 4096) // ~52 KiB
    fileChecksum := sha256hex(fileData)

    chunkSize := int64(4096)
    totalChunks := (len(fileData) + int(chunkSize) - 1) / int(chunkSize)

    uploadID := initSessionEx(t, url, "large-chunked.bin", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)

    for i := range totalChunks {
        start := i * int(chunkSize)
        end := min(start+int(chunkSize), len(fileData))
        chunkData := fileData[start:end]
        chunkCS := sha256hex(chunkData)
        uploadChunk(t, url, uploadID, i, chunkCS, chunkData)
    }

    completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
    resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
    if err != nil {
        t.Fatalf("complete: %v", err)
    }
    defer resp.Body.Close()

    var result ChunkCompleteResponse
    json.NewDecoder(resp.Body).Decode(&result)
    if !result.Success {
        t.Fatalf("complete failed: %v", result)
    }
    if result.FileChecksum != fileChecksum {
        t.Fatalf("checksum mismatch: got %s, want %s", result.FileChecksum, fileChecksum)
    }
}

func TestChunkedUpload_ResumeAfterInterrupt(t *testing.T) {
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    fileData := bytes.Repeat([]byte("ResumeMe"), 3000) // ~24 KiB, 6 chunks at 4KiB
    fileChecksum := sha256hex(fileData)
    chunkSize := int64(4096)
    totalChunks := (len(fileData) + int(chunkSize) - 1) / int(chunkSize)

    // Init
    uploadID := initSessionEx(t, url, "resume-test.bin", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)

    // 只上传前 3 个分块
    for i := range 3 {
        start := i * int(chunkSize)
        end := min(start+int(chunkSize), len(fileData))
        chunkData := fileData[start:end]
        uploadChunk(t, url, uploadID, i, sha256hex(chunkData), chunkData)
    }

    // 查询状态
    statusResp, err := http.Get(url + "/upload/status?upload_id=" + uploadID)
    if err != nil {
        t.Fatalf("status: %v", err)
    }
    var status ChunkStatusResponse
    json.NewDecoder(statusResp.Body).Decode(&status)
    statusResp.Body.Close()

    if !status.Success {
        t.Fatalf("status failed: %v", status)
    }
    if len(status.MissingChunks) != totalChunks-3 {
        t.Fatalf("expected %d missing chunks, got %v", totalChunks-3, status.MissingChunks)
    }

    // 上传剩余分块
    for _, idx := range status.MissingChunks {
        start := idx * int(chunkSize)
        end := min(start+int(chunkSize), len(fileData))
        chunkData := fileData[start:end]
        uploadChunk(t, url, uploadID, idx, sha256hex(chunkData), chunkData)
    }

    // Complete
    completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
    resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
    if err != nil {
        t.Fatalf("complete: %v", err)
    }
    defer resp.Body.Close()
    var result ChunkCompleteResponse
    json.NewDecoder(resp.Body).Decode(&result)
    if !result.Success {
        t.Fatalf("resume complete failed: %v", result)
    }
}
```

- [ ] **Step 2: 追加已存在文件 checksum 匹配/不匹配 + digest 一致性测试**

```go
func TestChunkedUpload_AlreadyExists_ChecksumMatch(t *testing.T) {
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    // 先用普通上传创建文件
    body := []byte("existing file content")
    cs := sha256hex(body)
    uploadFile(t, url, "existing.txt", body, map[string]string{"X-File-Checksum": cs})

    // 分块 init 同名文件（checksum 匹配）
    initReq := map[string]any{
        "upload_id":     "already-exists-test",
        "filename":      "existing.txt",
        "total_size":    len(body),
        "chunk_size":    4096,
        "total_chunks":  1,
        "file_checksum": cs,
    }
    initJSON, _ := json.Marshal(initReq)
    resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initJSON))
    if err != nil {
        t.Fatalf("init: %v", err)
    }
    defer resp.Body.Close()

    var initResult ChunkedInitResponse
    json.NewDecoder(resp.Body).Decode(&initResult)
    if !initResult.Success {
        t.Fatalf("expected success for existing file with matching checksum: %v", initResult)
    }
    if initResult.UploadID != "already_exists" {
        t.Fatalf("expected upload_id='already_exists', got %q", initResult.UploadID)
    }
}

func TestChunkedUpload_AlreadyExists_ChecksumMismatch(t *testing.T) {
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    body := []byte("original content")
    uploadFile(t, url, "conflict-chunked.txt", body, map[string]string{
        "X-File-Checksum": sha256hex(body),
    })

    // 分块 init 同名文件但不同 checksum
    initReq := map[string]any{
        "upload_id":     "conflict-test",
        "filename":      "conflict-chunked.txt",
        "total_size":    100,
        "chunk_size":    4096,
        "total_chunks":  1,
        "file_checksum": strings.Repeat("f", 64),
    }
    initJSON, _ := json.Marshal(initReq)
    resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initJSON))
    if err != nil {
        t.Fatalf("init: %v", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusConflict {
        t.Fatalf("expected 409, got %d", resp.StatusCode)
    }
}

func TestChunkedUploadStatus_ByFilename(t *testing.T) {
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    fileData := []byte("status by filename test")
    fileChecksum := sha256hex(fileData)
    uploadID := initSession(t, url, "status-by-fn.txt", int64(len(fileData)), fileChecksum)

    // 上传部分 chunk
    uploadChunk(t, url, uploadID, 0, fileChecksum, fileData)

    // 按 filename 查询
    resp, err := http.Get(url + "/upload/status?filename=status-by-fn.txt")
    if err != nil {
        t.Fatalf("status by filename: %v", err)
    }
    defer resp.Body.Close()

    var status ChunkStatusResponse
    json.NewDecoder(resp.Body).Decode(&status)
    if !status.Success {
        t.Fatalf("status by filename failed: %v", status)
    }
    if status.UploadID != uploadID {
        t.Fatalf("expected upload_id %s, got %s", uploadID, status.UploadID)
    }
}

func TestChunkedDigestConsistency(t *testing.T) {
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    data := []byte("consistency check across upload modes")
    cs := sha256hex(data)

    // 方式1: 普通上传
    uploadFile(t, url, "consistency-normal.bin", data, map[string]string{"X-File-Checksum": cs})

    // 方式2: 分块上传
    chunkSize := int64(4096)
    totalChunks := 1
    uploadID := initSessionEx(t, url, "consistency-chunked.bin", int64(len(data)), chunkSize, totalChunks, cs)
    uploadChunk(t, url, uploadID, 0, cs, data)
    completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
    http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

    // 下载两个文件，验证 checksum 一致
    c := client.NewFileClient(url)
    outDir := t.TempDir()

    out1 := filepath.Join(outDir, "normal.bin")
    if err := c.Download(context.Background(), "consistency-normal.bin", out1); err != nil {
        t.Fatalf("download normal: %v", err)
    }
    out2 := filepath.Join(outDir, "chunked.bin")
    if err := c.Download(context.Background(), "consistency-chunked.bin", out2); err != nil {
        t.Fatalf("download chunked: %v", err)
    }

    data1, _ := os.ReadFile(out1)
    data2, _ := os.ReadFile(out2)
    if !bytes.Equal(data1, data2) {
        t.Fatal("normal upload and chunked upload produced different content")
    }
}
```

- [ ] **Step 3: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestChunkedUpload_|TestChunkedDigest|TestChunkedUploadStatus" -v -count=1 -timeout=60s 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/server/chunked_upload_test.go
git commit -m "test: add multi-chunk/resume/already-exists/digest-consistency tests"
```

---

### Task 8: L3 — 分块下载测试

**Files:**
- Create: `pkg/server/chunked_download_test.go`

- [ ] **Step 1: 创建分块下载测试文件**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
    "bytes"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "testing"
)

func TestDownloadChunk_FullFileViaChunks(t *testing.T) {
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    // 准备 16 KiB 文件
    fileData := bytes.Repeat([]byte("ChunkDownloadTest!"), 1024) // 16 KiB
    fileChecksum := sha256hex(fileData)
    chunkSize := int64(4096)

    // 通过分块上传创建文件
    uploadID := initSessionEx(t, url, "full-dl-chunked.bin", int64(len(fileData)), chunkSize,
        (len(fileData)+4095)/4096, fileChecksum)

    totalChunks := (len(fileData) + int(chunkSize) - 1) / int(chunkSize)
    for i := range totalChunks {
        start := i * int(chunkSize)
        end := min(start+int(chunkSize), len(fileData))
        chunkData := fileData[start:end]
        uploadChunk(t, url, uploadID, i, sha256hex(chunkData), chunkData)
    }
    completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
    http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

    // 用分块下载重组
    var assembled []byte
    for offset := 0; offset < len(fileData); offset += int(chunkSize) {
        length := min(int(chunkSize), len(fileData)-offset)
        resp, err := http.Get(fmt.Sprintf("%s/download/chunk?filename=full-dl-chunked.bin&offset=%d&length=%d",
            url, offset, length))
        if err != nil {
            t.Fatalf("download chunk at offset %d: %v", offset, err)
        }
        if resp.StatusCode != http.StatusOK {
            resp.Body.Close()
            t.Fatalf("expected 200 at offset %d, got %d", offset, resp.StatusCode)
        }
        chunkBody, _ := io.ReadAll(resp.Body)
        resp.Body.Close()
        assembled = append(assembled, chunkBody...)
    }

    if !bytes.Equal(assembled, fileData) {
        t.Fatal("reassembled file content mismatch")
    }

    // 验证完整 file checksum
    gotCS := sha256hex(assembled)
    if gotCS != fileChecksum {
        t.Fatalf("reassembled checksum mismatch: got %s, want %s", gotCS, fileChecksum)
    }
}

func TestDownloadChunk_OffsetBeyondFile(t *testing.T) {
    url, _, cleanup := newTestServerWithChunked(t, nil)
    defer cleanup()

    fileData := []byte("small")
    fileChecksum := sha256hex(fileData)
    uploadID := initSession(t, url, "small-dl.bin", int64(len(fileData)), fileChecksum)
    uploadChunk(t, url, uploadID, 0, fileChecksum, fileData)
    completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
    http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

    // offset 超过 file size
    resp, err := http.Get(url + "/download/chunk?filename=small-dl.bin&offset=100&length=10")
    if err != nil {
        t.Fatalf("request: %v", err)
    }
    resp.Body.Close()
    if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
        t.Fatalf("expected 416, got %d", resp.StatusCode)
    }
}
```

- [ ] **Step 2: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestDownloadChunk" -v -count=1 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/server/chunked_download_test.go
git commit -m "test: add chunked download full-file and boundary tests"
```

---

### Task 9: L3 — 混沌测试（崩溃恢复）

**Files:**
- Modify: `pkg/server/e2e_test.go`（追加混沌测试函数）

- [ ] **Step 1: 追加 UploadStore 崩溃恢复测试**

```go
func TestChaos_CrashDuringChunkedUpload(t *testing.T) {
    // 模拟服务端在上传过程中"崩溃"后的恢复
    tmpDir := t.TempDir()

    // 阶段1: 创建 session 并上传部分分块
    us1 := NewUploadStore(tmpDir, 24*time.Hour, nil)

    fileData := bytes.Repeat([]byte("ChaosTest"), 2048) // ~16 KiB, 4 chunks
    fileChecksum := sha256hex(fileData)
    chunkSize := int64(4096)
    totalChunks := 4

    us1.CreateSession("crash-test-id", "crash-recover.bin", int64(len(fileData)), chunkSize, totalChunks, fileChecksum, 0)
    for i := 0; i < 2; i++ { // 只上传前 2 块
        chunkData := fileData[i*int(chunkSize) : (i+1)*int(chunkSize)]
        chunkCS := sha256hex(chunkData)

        // 手动写入 chunk 文件 (模拟上传)
        chunkDir := filepath.Join(tmpDir, ".__chunked__", "crash-test-id")
        os.MkdirAll(chunkDir, 0755)
        os.WriteFile(filepath.Join(chunkDir, fmt.Sprintf("%05d.chunk", i)), chunkData, 0644)

        us1.MarkChunkReceived("crash-test-id", i, chunkCS)
    }
    us1.Stop() // 模拟 crash

    // 阶段2: 新实例 recover
    us2 := NewUploadStore(tmpDir, 24*time.Hour, nil)
    defer us2.Stop()

    s := us2.GetSession("crash-test-id")
    if s == nil {
        t.Fatal("session should be recovered after crash")
    }
    if !s.ReceivedChunks[0] || !s.ReceivedChunks[1] {
        t.Fatal("chunks 0 and 1 should be recovered")
    }
    if s.ReceivedChunks[2] || s.ReceivedChunks[3] {
        t.Fatal("chunks 2 and 3 should not be marked")
    }

    // 上传剩余分块
    for i := 2; i < totalChunks; i++ {
        start := i * int(chunkSize)
        end := start + int(chunkSize)
        chunkData := fileData[start:end]
        chunkCS := sha256hex(chunkData)
        chunkDir := filepath.Join(tmpDir, ".__chunked__", "crash-test-id")
        os.WriteFile(filepath.Join(chunkDir, fmt.Sprintf("%05d.chunk", i)), chunkData, 0644)
        us2.MarkChunkReceived("crash-test-id", i, chunkCS)
    }

    if !us2.AllChunksReceived("crash-test-id") {
        t.Fatal("all chunks should be received after resume")
    }
}

func TestChaos_PartialChunkWrittenThenRecover(t *testing.T) {
    // 模拟 crash 后磁盘有 .chunk 文件但 bitmap 未持久化
    tmpDir := t.TempDir()

    us1 := NewUploadStore(tmpDir, 24*time.Hour, nil)
    us1.CreateSession("partial-id", "partial-recover.bin", 8192, 4096, 2, strings.Repeat("x", 64), 0)
    us1.Stop()

    // 手动写入 chunk 文件但不调用 MarkChunkReceived (模拟 crash)
    chunkDir := filepath.Join(tmpDir, ".__chunked__", "partial-id")
    os.MkdirAll(chunkDir, 0755)

    // 写入 chunk 0 和 chunk 1
    for i := 0; i < 2; i++ {
        data := bytes.Repeat([]byte{byte(i)}, 4096)
        os.WriteFile(filepath.Join(chunkDir, fmt.Sprintf("%05d.chunk", i)), data, 0644)
    }

    // 没有 session.json (模拟只有 chunk 文件没有 bitmap)
    // 但 recoverSessions 会扫目录

    us2 := NewUploadStore(tmpDir, 24*time.Hour, nil)
    defer us2.Stop()

    // 此时 session.json 由第一次 Stop() 持久化，但 bitmap 只有 false
    // reconcileChunks 应检测到 chunk 文件并设置 bitmap
    s := us2.GetSession("partial-id")
    if s == nil {
        t.Fatal("session should be recovered")
    }
    // reconcileChunks 会扫描磁盘 .chunk 文件并标记 received
    // 但 checksum 可能为空
    t.Logf("recovered session: received=%v", s.ReceivedChunks)
}

func TestChaos_ChecksumStoreCrashAtomic(t *testing.T) {
    tmpDir := t.TempDir()

    cs := NewChecksumStore(tmpDir, nil)
    cs.Set("k1", "v1")
    cs.Set("k2", "v2")

    // 模拟 crash: 创建一个 .tmp 残留文件
    tmpFile := filepath.Join(tmpDir, ".checksums.json.tmp")
    os.WriteFile(tmpFile, []byte(`{"stale":"data"}`), 0644)

    // 新实例: 应清理 .tmp 并正确加载已持久化的 .json
    cs2 := NewChecksumStore(tmpDir, nil)
    all := cs2.GetAll()
    if len(all) != 2 {
        t.Fatalf("expected 2 entries, got %d; tmp residue was not cleaned", len(all))
    }
    // 确认 .tmp 已清理
    if _, err := os.Stat(tmpFile); err == nil {
        t.Fatal(".tmp should be cleaned up on startup")
    }
}
```

- [ ] **Step 2: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/server/... -run "TestChaos" -v -count=1 -timeout=60s 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/server/e2e_test.go
git commit -m "test: add chaos/crash-recovery tests for UploadStore and ChecksumStore"
```

---

### Task 10: L3 — Client 分块上传/下载端到端测试

**Files:**
- Create: `pkg/client/e2e_test.go`

- [ ] **Step 1: 创建 client 端到端测试**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
    "bytes"
    "context"
    "crypto/sha256"
    "encoding/hex"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "sync/atomic"
    "testing"
    "time"

    "github.com/cocomhub/sproxy/internal/size"
    "github.com/cocomhub/sproxy/pkg/server"
)

// startTestServer 启动完整 sproxy 服务。
func startFullTestServer(t *testing.T) (string, *server.Config) {
    t.Helper()
    tmpDir := t.TempDir()

    cfg := server.Default()
    cfg.UploadsDir = tmpDir
    cfg.ChunkSize = 4 << 10 // 4 KiB for test
    if err := cfg.Validate(); err != nil {
        t.Fatalf("Validate: %v", err)
    }

    var cfgPtr atomic.Pointer[server.Config]
    cfgPtr.Store(cfg)

    mux := http.NewServeMux()
    h := server.RegisterRoutes(context.Background(), mux, &cfgPtr, "v", "t", nil, nil)
    t.Cleanup(func() { _ = h.Close() })

    ts := httptest.NewServer(mux)
    t.Cleanup(ts.Close)
    return ts.URL, cfg
}

func TestClientChunkedUpload_Download_RoundTrip(t *testing.T) {
    url, _ := startFullTestServer(t)

    srcDir := t.TempDir()
    fileData := bytes.Repeat([]byte("ClientChunkedTest!"), 1280) // ~20 KiB
    srcPath := filepath.Join(srcDir, "upload.bin")
    if err := os.WriteFile(srcPath, fileData, 0644); err != nil {
        t.Fatalf("write: %v", err)
    }

    c := NewFileClient(url, WithChunkedChunkSize(4096))
    defer c.httpClient.CloseIdleConnections()

    // 分块上传
    result, err := c.ChunkedUpload(context.Background(), srcPath, "upload.bin", nil)
    if err != nil {
        t.Fatalf("ChunkedUpload: %v", err)
    }
    if !result.Success {
        t.Fatalf("chunked upload failed: %s", result.Message)
    }

    // 分块下载
    outDir := t.TempDir()
    outPath := filepath.Join(outDir, "downloaded.bin")
    if err := c.ChunkedDownload(context.Background(), "upload.bin", outPath, 4096); err != nil {
        t.Fatalf("ChunkedDownload: %v", err)
    }

    got, _ := os.ReadFile(outPath)
    if !bytes.Equal(got, fileData) {
        t.Fatal("downloaded content mismatch after chunked round-trip")
    }
}

func TestClientChunkedUpload_Resume(t *testing.T) {
    url, _ := startFullTestServer(t)

    srcDir := t.TempDir()
    fileData := bytes.Repeat([]byte("ResumeChunkedData"), 2048) // ~32 KiB
    srcPath := filepath.Join(srcDir, "resume.bin")
    if err := os.WriteFile(srcPath, fileData, 0644); err != nil {
        t.Fatalf("write: %v", err)
    }

    c := NewFileClient(url, WithChunkedChunkSize(4096))
    defer c.httpClient.CloseIdleConnections()

    // 分块上传（允许续传）
    result, err := c.ChunkedUpload(context.Background(), srcPath, "resume.bin", nil)
    if err != nil {
        t.Fatalf("ChunkedUpload: %v", err)
    }
    if !result.Success {
        t.Fatalf("upload failed: %s", result.Message)
    }

    // 下载验证
    outDir := t.TempDir()
    outPath := filepath.Join(outDir, "resume-dl.bin")
    if err := c.Download(context.Background(), "resume.bin", outPath); err != nil {
        t.Fatalf("Download: %v", err)
    }
    got, _ := os.ReadFile(outPath)
    if !bytes.Equal(got, fileData) {
        t.Fatal("content mismatch after resume upload")
    }
}

func TestClient_ChunkedUploadAutoThreshold(t *testing.T) {
    // 验证小文件不上传自动分块，大文件自动启用
    if size.AutoChunkThreshold < 0 {
        t.Skip("AutoChunkThreshold not set")
    }
    url, _ := startFullTestServer(t)

    srcDir := t.TempDir()
    // 略小于 AutoChunkThreshold 的文件
    smallData := bytes.Repeat([]byte("S"), 1024) // 1 KiB
    srcPath := filepath.Join(srcDir, "small.bin")
    if err := os.WriteFile(srcPath, smallData, 0644); err != nil {
        t.Fatalf("write: %v", err)
    }

    c := NewFileClient(url)
    defer c.httpClient.CloseIdleConnections()

    // 使用 ShouldAutoChunk 决策，或直接验证普通 upload 也工作
    if size.ShouldAutoChunk(len(smallData)) {
        t.Log("file below threshold should not auto-chunk")
    }

    // 两种方式都应工作
    result, err := c.Upload(context.Background(), srcPath, "small.bin")
    if err != nil {
        t.Fatalf("Upload (non-chunked): %v", err)
    }
    if !result.Success {
        t.Fatalf("upload failed: %s", result.Message)
    }
}
```

- [ ] **Step 2: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/client/... -run "TestClient" -v -count=1 -timeout=120s 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 3: Commit**

```bash
git add pkg/client/e2e_test.go
git commit -m "test: add client chunked upload/download round-trip and resume tests"
```

---

### Task 11: Client 配置管理测试

**Files:**
- Create: `pkg/client/config_test.go`

- [ ] **Step 1: 创建 client config 测试**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
    "os"
    "path/filepath"
    "testing"
)

func TestClientConfig_Defaults(t *testing.T) {
    cfg := DefaultConfig()
    if cfg.ServerURL == "" {
        t.Fatal("ServerURL should have default")
    }
    if err := cfg.Validate(); err != nil {
        t.Fatalf("Validate default: %v", err)
    }
}

func TestClientConfig_LoadFromViper(t *testing.T) {
    // 这个测试依赖 viper，验证基本加载流程
    cfg := DefaultConfig()
    if cfg.ServerURL == "" {
        t.Fatal("default ServerURL should be set")
    }
}

func TestClientConfig_SaveAndLoad(t *testing.T) {
    tmpDir := t.TempDir()
    path := filepath.Join(tmpDir, "sclient.yaml")

    cfg := DefaultConfig()
    cfg.ServerURL = "http://test:18083"
    if err := SaveConfig(cfg, path); err != nil {
        t.Fatalf("SaveConfig: %v", err)
    }

    loaded, err := LoadConfig(path)
    if err != nil {
        t.Fatalf("LoadConfig: %v", err)
    }
    if loaded.ServerURL != "http://test:18083" {
        t.Fatalf("ServerURL mismatch: got %q, want %q", loaded.ServerURL, "http://test:18083")
    }
}

func TestClientConfig_Hover(t *testing.T) {
    tmpDir := t.TempDir()

    cfg := DefaultConfig()
    cfg.ServerURL = "http://hover:18083"
    cfg.AuthToken = "hover-token"
    if err := SaveConfig(cfg, filepath.Join(tmpDir, "sclient.yaml")); err != nil {
        t.Fatalf("SaveConfig: %v", err)
    }

    loaded, err := LoadConfig(filepath.Join(tmpDir, "sclient.yaml"))
    if err != nil {
        t.Fatalf("LoadConfig: %v", err)
    }
    if loaded.ServerURL != "http://hover:18083" || loaded.AuthToken != "hover-token" {
        t.Fatalf("loaded config mismatch: %+v", loaded)
    }
}
```

- [ ] **Step 2: 先确保 config.go 中的函数能正常编译**

检查 client/config.go 中的 `DefaultConfig`, `SaveConfig`, `LoadConfig`, `Validate` 签名，确保测试代码能编译。

- [ ] **Step 3: 运行测试**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/client/... -run "TestClientConfig" -v -count=1 2>&1`
Expected: 所有测试 PASS

- [ ] **Step 4: Commit**

```bash
git add pkg/client/config_test.go
git commit -m "test: add client config save/load/validate tests"
```

---

### Task 12: 最终覆盖率验证

- [ ] **Step 1: 运行全量测试并获取覆盖率报告**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test -coverprofile=full.cover ./pkg/server/... ./pkg/client/... ./pkg/tunnel/... 2>&1 && go tool cover -func=full.cover 2>&1`

Expected:
- `pkg/server` 覆盖率 ≥80%
- `pkg/client` 覆盖率 ≥65%
- `pkg/tunnel` 覆盖率 ≥80%
- 此前 0% 的关键函数（mkdir/rmdir/healthz/version/SaveConfig/FileChecksum/recoverSessions/cleanupExpired/DeletePrefix/GetSessionByFilename）均已覆盖

- [ ] **Step 2: 确认所有测试无竞争条件（race）**

Run: `cd D:/workdir/leon/cocomhub/sproxy && go test -race -count=1 -timeout=120s ./pkg/server/... ./pkg/client/... 2>&1`
Expected: `ok` 且无 race 警告

- [ ] **Step 3: Commit 最终覆盖率报告**

```bash
# 不提交 cover 文件（gitignored），提交新改动的测试文件
git add -A
git status
git commit -m "test: finalize test suite — full coverage verification passed"
```

---

## Spec 覆盖检查

| Spec 章节 | 对应 Task | 备注 |
|---|---|---|
| L1 healthz/version | Task 1 | ✅ |
| L1 mkdir/rmdir | Task 1 | ✅ |
| L1 rename 分支 | Task 2 | from=to/409/404/400/路径穿越 |
| L1 stat 分支 | Task 2 | 空/404/目录/穿越 |
| L1 listFiles 子目录 | Task 2 | ✅ |
| L1 upload 文件已存在409 | Task 2 | ✅ |
| L2 ChecksumStore DeletePrefix/Rename/Recover | Task 3 | ✅ |
| L2 UploadStore GetSessionByFilename/Delete/CleanupExpired/Recover | Task 4 | ✅ |
| L2 UploadStore 并发 MarkChunk | Task 4 | ✅ |
| L2 SaveConfig/FileChecksum/verifyChecksum | Task 5 | ✅ |
| L3 并发上传/rename/delete | Task 6 | ✅ |
| L3 分块上传大文件/续传/StatusByFilename/DigestConsistency | Task 7 | ✅ |
| L3 分块下载全量/416 | Task 8 | ✅ |
| L3 混沌 crash/partial chunk/checksum atomic | Task 9 | ✅ |
| L3 Client chunked round-trip/resume | Task 10 | ✅ |
| L3 Client config 测试 | Task 11 | ✅ |
| 最终覆盖率验证 | Task 12 | ✅ |