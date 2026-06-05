# Phase 5 实现计划 — 前瞻性功能

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。

**目标：** 为 sproxy 实现 6 个前瞻性功能：文件归档下载、文件版本管理、Prometheus Metrics、统计监控页面、文件分享链接、多用户权限系统。

**架构：** 每个功能独立实现，按依赖关系排序（P5-6 → P5-1 → P5-4 → P5-2 → P5-3 → P5-5）。严格 stdlib 政策不变，不引入第三方依赖。

**技术栈：** Go 1.26（archive/tar, compress/gzip 皆为标准库），Web UI 纯原生 JS，无前端框架。

---

## 文件索引

| 文件 | 涉及功能 | 职责 |
|------|----------|------|
| `pkg/server/handlers.go` | P5-6/P5-1/P5-4/P5-2/P5-3/P5-5 | 路由注册、handler 方法 |
| `pkg/server/archive.go` | P5-6 | 归档下载 handler + tar.gz 流式打包 |
| `pkg/server/version.go` | P5-1 | 版本管理 handler + 版本存储 |
| `pkg/server/metrics.go` | P5-4 | Prometheus Metrics 采集与暴露 |
| `pkg/server/stats.go` | P5-2 | 统计 API handler |
| `pkg/server/share.go` | P5-3 | 分享链接 handler + 内存存储 |
| `pkg/server/auth.go` | P5-5 | 多用户权限系统 + API Key 管理 |
| `pkg/server/config.go` | 全部 | 配置结构体扩展 |
| `pkg/server/response.go` | 全部 | 响应类型扩展 |
| `pkg/client/client.go` | P5-6/P5-1/P5-3 | 客户端方法扩展 |
| `pkg/client/archive.go` | P5-6 | 客户端归档下载 |
| `cmd/sclient/archive.go` | P5-6 | sclient archive 命令 |
| `cmd/sclient/version.go` | P5-1 | sclient version 命令 |
| `web/static/index.html` | P5-6/P5-2/P5-3 | Web UI 扩展 |
| `config.example.yaml` | 全部 | 配置示例更新 |
| `pkg/server/integration_test.go` | 全部 | 集成测试 |
| `pkg/server/archive_test.go` | P5-6 | 归档测试 |
| `pkg/server/version_test.go` | P5-1 | 版本管理测试 |
| `pkg/server/share_test.go` | P5-3 | 分享链接测试 |

---

## 任务 1: P5-6 文件归档下载

**目标版本：** v0.8.0

**文件：**
- 新增：`pkg/server/archive.go`
- 新增：`pkg/client/archive.go`
- 新增：`cmd/sclient/archive.go`
- 新增：`pkg/server/archive_test.go`
- 修改：`pkg/server/handlers.go`（注册路由、Handlers 结构体扩展）
- 修改：`pkg/server/response.go`（ArchiveRequest 类型）
- 修改：`pkg/client/client.go`（Archive 方法）
- 修改：`cmd/sclient/root.go`（注册 archive 子命令）
- 修改：`web/static/index.html`（批量下载、目录下载 UI）

### 1.1 服务端归档 handler

**文件：**
- 新增：`pkg/server/archive.go`

```go
package server

import (
    "archive/tar"
    "compress/gzip"
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "os"
    "path/filepath"
    "strings"
)

// ArchiveRequest 是 POST /api/archive 的请求体。
type ArchiveRequest struct {
    Files   []string `json:"files"`
}

// archiveHandler 处理 POST /api/archive。
// 接收 JSON {"files": ["file1.txt", "dir/file2.txt"]}，
// 返回 application/tar+gzip 流式归档文件。
// 使用 io.Pipe 实现流式打包，不占用额外磁盘空间。
func (h *Handlers) archiveHandler(w http.ResponseWriter, r *http.Request) {
    r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
    var req ArchiveRequest
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体: " + err.Error()}, http.StatusBadRequest)
        return
    }
    if len(req.Files) == 0 {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
        return
    }
    
    cfg := h.cfgPtr.Load()
    logger := h.logger.With("archive", "create")
    
    // 验证所有文件路径，收集绝对路径
    validated := make([]string, 0, len(req.Files))
    for _, f := range req.Files {
        relPath, err := ValidateFilePath(f)
        if err != nil {
            sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件路径: " + f}, http.StatusBadRequest)
            return
        }
        validated = append(validated, relPath)
    }
    
    w.Header().Set("Content-Type", "application/gzip")
    w.Header().Set("Content-Disposition", "attachment; filename=\"archive.tar.gz\"")
    w.WriteHeader(http.StatusOK)
    
    // 流式打包：io.Pipe 中 tar + gzip
    pr, pw := io.Pipe()
    go func() {
        gw := gzip.NewWriter(pw)
        tw := tar.NewWriter(gw)
        
        for _, relPath := range validated {
            fullPath := filepath.Join(cfg.UploadsDir, relPath)
            if err := addFileToTar(tw, fullPath, relPath, logger); err != nil {
                logger.Error("归档添加文件失败", "path", relPath, "error", err)
            }
        }
        
        // 按序关闭
        if err := tw.Close(); err != nil {
            logger.Error("tar writer 关闭失败", "error", err)
        }
        if err := gw.Close(); err != nil {
            logger.Error("gzip writer 关闭失败", "error", err)
        }
        pw.Close()
    }()
    
    io.Copy(w, pr)
}

// addFileToTar 将单个文件（或目录）添加到 tar writer 中。
// 如果是目录则递归添加。
func addFileToTar(tw *tar.Writer, fullPath, relPath string, logger *slog.Logger) error {
    info, err := os.Stat(fullPath)
    if err != nil {
        return fmt.Errorf("stat 失败: %w", err)
    }
    
    if info.IsDir() {
        // 递归添加目录内容
        entries, err := os.ReadDir(fullPath)
        if err != nil {
            return fmt.Errorf("读取目录失败: %w", err)
        }
        for _, entry := range entries {
            childRel := filepath.ToSlash(filepath.Join(relPath, entry.Name()))
            childFull := filepath.Join(fullPath, entry.Name())
            if err := addFileToTar(tw, childFull, childRel, logger); err != nil {
                logger.Warn("归档添加子文件失败", "path", childRel, "error", err)
            }
        }
        return nil
    }
    
    file, err := os.Open(fullPath)
    if err != nil {
        return fmt.Errorf("打开文件失败: %w", err)
    }
    defer file.Close()
    
    header, err := tar.FileInfoHeader(info, "")
    if err != nil {
        return fmt.Errorf("创建 tar header 失败: %w", err)
    }
    header.Name = filepath.ToSlash(relPath)
    
    if err := tw.WriteHeader(header); err != nil {
        return fmt.Errorf("写入 tar header 失败: %w", err)
    }
    if _, err := io.Copy(tw, file); err != nil {
        return fmt.Errorf("写入文件内容失败: %w", err)
    }
    return nil
}

// archiveDirHandler 处理 GET /api/archive-dir?dirname=xxx。
// 将指定目录及其内容打包下载。
func (h *Handlers) archiveDirHandler(w http.ResponseWriter, r *http.Request) {
    dirname := r.URL.Query().Get("dirname")
    if dirname == "" {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "dirname 不能为空"}, http.StatusBadRequest)
        return
    }
    relPath, err := ValidateFilePath(dirname)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录名: " + err.Error()}, http.StatusBadRequest)
        return
    }
    
    cfg := h.cfgPtr.Load()
    fullPath := filepath.Join(cfg.UploadsDir, relPath)
    info, err := os.Stat(fullPath)
    if err != nil {
        if os.IsNotExist(err) {
            sendJSONResponse(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
        } else {
            sendJSONResponse(w, UploadResponse{Success: false, Message: "访问目录失败"}, http.StatusInternalServerError)
        }
        return
    }
    if !info.IsDir() {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
        return
    }
    
    archiveName := filepath.Base(relPath) + ".tar.gz"
    w.Header().Set("Content-Type", "application/gzip")
    w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", archiveName))
    w.WriteHeader(http.StatusOK)
    
    pr, pw := io.Pipe()
    go func() {
        gw := gzip.NewWriter(pw)
        tw := tar.NewWriter(gw)
        addFileToTar(tw, fullPath, filepath.ToSlash(relPath), h.logger)
        tw.Close()
        gw.Close()
        pw.Close()
    }()
    io.Copy(w, pr)
}
```

### 1.2 注册路由

**文件：**
- 修改：`pkg/server/handlers.go`（RegisterRoutes 函数）

在 `RegisterRoutes` 的 `mux.HandleFunc(...)` 注册段添加以下路由：

```go
// 归档下载
mux.HandleFunc("POST /api/archive", h.authMiddleware(h.archiveHandler))
mux.HandleFunc("GET /api/archive-dir", h.authMiddleware(h.archiveDirHandler))
```

同时在 `localMux.HandleFunc(...)` 段也添加对应的本地路由（无 auth）。

### 1.3 客户端 Archive 方法

**文件：**
- 新增：`pkg/client/archive.go`

```go
package client

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "os"
)

// Archive 将服务器端指定的文件列表打包下载到本地文件。
// files: 服务端文件路径列表；outputPath: 本地目标 .tar.gz 文件路径。
func (c *FileClient) Archive(ctx context.Context, files []string, outputPath string) error {
    body, _ := json.Marshal(map[string]any{"files": files})
    
    req, err := http.NewRequestWithContext(ctx, "POST", c.serverURL+"/api/archive", bytes.NewReader(body))
    if err != nil {
        return fmt.Errorf("创建请求失败: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    
    resp, err := c.doRequest(ctx, req)
    if err != nil {
        return fmt.Errorf("归档请求失败: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("归档失败 (HTTP %d): %s", resp.StatusCode, string(body))
    }
    
    out, err := os.Create(outputPath)
    if err != nil {
        return fmt.Errorf("创建输出文件失败: %w", err)
    }
    defer out.Close()
    
    _, err = io.Copy(out, resp.Body)
    return err
}

// ArchiveDir 将服务器端指定目录打包下载到本地文件。
func (c *FileClient) ArchiveDir(ctx context.Context, dirname, outputPath string) error {
    req, err := http.NewRequestWithContext(ctx, "GET", 
        c.serverURL+"/api/archive-dir?dirname="+url.QueryEscape(dirname), nil)
    if err != nil {
        return fmt.Errorf("创建请求失败: %w", err)
    }
    
    resp, err := c.doRequest(ctx, req)
    if err != nil {
        return fmt.Errorf("归档请求失败: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        body, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("归档失败 (HTTP %d): %s", resp.StatusCode, string(body))
    }
    
    out, err := os.Create(outputPath)
    if err != nil {
        return fmt.Errorf("创建输出文件失败: %w", err)
    }
    defer out.Close()
    
    _, err = io.Copy(out, resp.Body)
    return err
}
```

### 1.4 sclient archive 命令

**文件：**
- 新增：`cmd/sclient/archive.go`

```go
package main

import (
    "fmt"
    "strings"
    "github.com/spf13/cobra"
)

var archiveCmd = &cobra.Command{
    Use:   "archive [flags] <file...>",
    Short: "将服务端文件打包下载为 tar.gz",
    Long:  `将指定文件列表打包下载为 tar.gz 归档文件。支持目录。`,
    RunE: func(cmd *cobra.Command, args []string) error {
        if len(args) < 1 {
            return fmt.Errorf("至少需要一个文件名")
        }
        output, _ := cmd.Flags().GetString("output")
        client := newFileClient()
        files := args
        if strings.HasPrefix(files[0], "@") {
            // @filename 格式：从文件中读取文件列表
            return archiveFromFile(client, files[0][1:], output)
        }
        return client.Archive(cmd.Context(), files, output)
    },
}

var archiveDirCmd = &cobra.Command{
    Use:   "archive-dir [flags] <dirname>",
    Short: "将服务端目录打包下载为 tar.gz",
    RunE: func(cmd *cobra.Command, args []string) error {
        if len(args) < 1 {
            return fmt.Errorf("需要指定目录名")
        }
        output, _ := cmd.Flags().GetString("output")
        client := newFileClient()
        return client.ArchiveDir(cmd.Context(), args[0], output)
    },
}

func init() {
    archiveCmd.Flags().StringP("output", "o", "archive.tar.gz", "输出文件路径")
    archiveDirCmd.Flags().StringP("output", "o", "archive.tar.gz", "输出文件路径")
    rootCmd.AddCommand(archiveCmd)
    rootCmd.AddCommand(archiveDirCmd)
}
```

### 1.5 Web UI 归档按钮

**文件：**
- 修改：`web/static/index.html`

在 `#batch-toolbar` 中添加一个"下载归档"按钮：
```html
<button class="btn btn-primary btn-sm" onclick="batchDownloadArchive()">下载归档</button>
```

在 JS 中添加函数：
```javascript
let selectedForArchive = [];

function batchDownloadArchive() {
    const selected = getSelectedFiles();
    if (selected.length === 0) { showToast('请选择文件', 'warning'); return; }
    
    const files = selected.map(f => {
        const path = currentDir ? currentDir + '/' + f.name : f.name;
        return path;
    });
    
    // 使用 fetch POST /api/archive 流式下载
    const headersObj = headers();
    headersObj['Content-Type'] = 'application/json';
    
    fetch(BASE + '/api/archive', {
        method: 'POST',
        headers: headersObj,
        body: JSON.stringify({ files: files })
    }).then(resp => {
        if (!resp.ok) return resp.text().then(t => { throw new Error(t); });
        const disposition = resp.headers.get('Content-Disposition') || '';
        const match = disposition.match(/filename="?(.+?)"?$/);
        const filename = match ? match[1] : 'archive.tar.gz';
        return resp.blob().then(blob => {
            const url = URL.createObjectURL(blob);
            const a = document.createElement('a');
            a.href = url; a.download = filename;
            a.click();
            URL.revokeObjectURL(url);
            showToast('归档下载完成: ' + filename, 'success');
        });
    }).catch(err => showToast('归档失败: ' + err.message, 'error'));
}
```

### 1.6 测试

**文件：**
- 新增：`pkg/server/archive_test.go`

```go
package server

import (
    "archive/tar"
    "compress/gzip"
    "encoding/json"
    "io"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "testing"
)

func TestArchive_SingleFile(t *testing.T) {
    ts, cleanup := newTestServerWithAllRoutes(t)
    defer cleanup()
    
    // 上传测试文件
    uploadFile(t, ts, "test.txt", "hello world", sha256hex("hello world"))
    
    // 归档
    body := `{"files":["test.txt"]}`
    resp, err := ts.Client().Post(ts.URL+"/api/archive", "application/json", strings.NewReader(body))
    if err != nil { t.Fatal(err) }
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
        t.Fatalf("expected application/gzip, got %s", ct)
    }
    
    // 解压验证
    gr, err := gzip.NewReader(resp.Body)
    if err != nil { t.Fatal(err) }
    defer gr.Close()
    
    tr := tar.NewReader(gr)
    header, err := tr.Next()
    if err != nil { t.Fatal(err) }
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
    ts, cleanup := newTestServerWithAllRoutes(t)
    defer cleanup()
    
    uploadFile(t, ts, "a.txt", "aaa", sha256hex("aaa"))
    uploadFile(t, ts, "b.txt", "bbb", sha256hex("bbb"))
    uploadFile(t, ts, "sub/c.txt", "ccc", sha256hex("ccc"))
    
    body := `{"files":["a.txt","b.txt","sub/c.txt"]}`
    resp, _ := ts.Client().Post(ts.URL+"/api/archive", "application/json", strings.NewReader(body))
    defer resp.Body.Close()
    
    gr, _ := gzip.NewReader(resp.Body)
    tr := tar.NewReader(gr)
    
    names := make(map[string]string)
    for {
        h, err := tr.Next()
        if err == io.EOF { break }
        if err != nil { t.Fatal(err) }
        content, _ := io.ReadAll(tr)
        names[h.Name] = string(content)
    }
    
    if names["a.txt"] != "aaa" { t.Errorf("a.txt: expected aaa, got %s", names["a.txt"]) }
    if names["b.txt"] != "bbb" { t.Errorf("b.txt: expected bbb, got %s", names["b.txt"]) }
    if names["sub/c.txt"] != "ccc" { t.Errorf("sub/c.txt: expected ccc, got %s", names["sub/c.txt"]) }
}

func TestArchive_InvalidPath(t *testing.T) {
    ts, cleanup := newTestServerWithAllRoutes(t)
    defer cleanup()
    
    body := `{"files":["../etc/passwd"]}`
    resp, _ := ts.Client().Post(ts.URL+"/api/archive", "application/json", strings.NewReader(body))
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400 for path traversal, got %d", resp.StatusCode)
    }
}

func TestArchive_EmptyFiles(t *testing.T) {
    ts, cleanup := newTestServerWithAllRoutes(t)
    defer cleanup()
    
    body := `{"files":[]}`
    resp, _ := ts.Client().Post(ts.URL+"/api/archive", "application/json", strings.NewReader(body))
    if resp.StatusCode != http.StatusBadRequest {
        t.Fatalf("expected 400 for empty files, got %d", resp.StatusCode)
    }
}

func TestArchiveDir_Success(t *testing.T) {
    ts, cleanup := newTestServerWithAllRoutes(t)
    defer cleanup()
    
    uploadFile(t, ts, "mydir/a.txt", "aaa", sha256hex("aaa"))
    uploadFile(t, ts, "mydir/sub/b.txt", "bbb", sha256hex("bbb"))
    
    resp, _ := ts.Client().Get(ts.URL + "/api/archive-dir?dirname=mydir")
    defer resp.Body.Close()
    
    if resp.StatusCode != http.StatusOK {
        t.Fatalf("expected 200, got %d", resp.StatusCode)
    }
    
    gr, _ := gzip.NewReader(resp.Body)
    tr := tar.NewReader(gr)
    
    names := make(map[string]string)
    for {
        h, err := tr.Next()
        if err == io.EOF { break }
        if err != nil { t.Fatal(err) }
        content, _ := io.ReadAll(tr)
        names[h.Name] = string(content)
    }
    
    if !strings.HasPrefix(names["mydir/a.txt"], "aaa") { t.Error("mydir/a.txt missing or wrong content") }
}
```

### 1.7 验证

- [ ] `go build ./cmd/sproxy/` 编译成功
- [ ] `go test -run TestArchive ./pkg/server/...` 全部通过
- [ ] `go test -run TestArchiveDir ./pkg/server/...` 全部通过
- [ ] `go build ./cmd/sclient/` 编译成功

---

## 任务 2: P5-1 文件版本管理

**目标版本：** v0.8.0

**文件：**
- 新增：`pkg/server/version.go`
- 新增：`pkg/server/version_test.go`
- 新增：`cmd/sclient/version.go`
- 修改：`pkg/server/handlers.go`（路由注册、upload handler 版本化改造）
- 修改：`pkg/server/config.go`（VersioningConfig）
- 修改：`pkg/server/response.go`（版本相关响应类型）
- 修改：`config.example.yaml`
- 修改：`web/static/index.html`（版本浏览 UI）

### 版本存储约定

版本文件存储在 `uploads_dir/.__versions__/<filename>/<timestamp>` 目录下。每次 upload 覆盖前，将现有文件复制到版本目录。

```go
package server

import (
    "encoding/json"
    "fmt"
    "io"
    "log/slog"
    "net/http"
    "os"
    "path/filepath"
    "time"
)

const versionsDirName = ".__versions__"

type VersionConfig struct {
    Enabled     bool  `yaml:"enabled" mapstructure:"enabled"`
    MaxVersions int   `yaml:"max_versions" mapstructure:"max_versions"`
}

type VersionInfo struct {
    Filename   string `json:"filename"`
    VersionID  int64  `json:"version_id"` // UnixNano timestamp
    Size       int64  `json:"size"`
    Checksum   string `json:"checksum"`
    CreatedAt  string `json:"created_at"`
}

// saveVersion 在上传覆盖前保存当前文件版本。
// 返回保存的版本 ID（UnixNano），如果没有旧文件则返回 0。
func (h *Handlers) saveVersion(remotePath, uploadsDir string) (int64, error) {
    fullPath := filepath.Join(uploadsDir, remotePath)
    if _, err := os.Stat(fullPath); os.IsNotExist(err) {
        return 0, nil // 新文件，无需保存版本
    }
    
    versionID := time.Now().UnixNano()
    verDir := filepath.Join(uploadsDir, versionsDirName, remotePath)
    if err := os.MkdirAll(verDir, 0755); err != nil {
        return 0, fmt.Errorf("创建版本目录失败: %w", err)
    }
    
    verPath := filepath.Join(verDir, fmt.Sprintf("%d", versionID))
    
    src, err := os.Open(fullPath)
    if err != nil {
        return 0, fmt.Errorf("打开源文件失败: %w", err)
    }
    defer src.Close()
    
    dst, err := os.Create(verPath)
    if err != nil {
        return 0, fmt.Errorf("创建版本文件失败: %w", err)
    }
    defer dst.Close()
    
    if _, err := io.Copy(dst, src); err != nil {
        os.Remove(verPath)
        return 0, fmt.Errorf("复制版本文件失败: %w", err)
    }
    
    // 清理超出上限的旧版本
    h.cleanupOldVersions(remotePath, uploadsDir)
    
    h.logger.Info("文件版本已保存", "file_name", remotePath, "version_id", versionID)
    return versionID, nil
}

// cleanupOldVersions 删除超出 max_versions 的旧版本。
func (h *Handlers) cleanupOldVersions(remotePath, uploadsDir string) {
    cfg := h.cfgPtr.Load()
    if cfg.Versioning.MaxVersions <= 0 {
        return
    }
    
    verDir := filepath.Join(uploadsDir, versionsDirName, remotePath)
    entries, err := os.ReadDir(verDir)
    if err != nil {
        return
    }
    
    if len(entries) <= cfg.Versioning.MaxVersions {
        return
    }
    
    // 按文件名（时间戳）排序，删除最旧的
    excess := len(entries) - cfg.Versioning.MaxVersions
    for i := 0; i < excess; i++ {
        os.Remove(filepath.Join(verDir, entries[i].Name()))
    }
}

// listVersionsHandler 处理 GET /api/versions?filename=xxx。
func (h *Handlers) listVersionsHandler(w http.ResponseWriter, r *http.Request) {
    filename := r.URL.Query().Get("filename")
    if filename == "" {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 不能为空"}, http.StatusBadRequest)
        return
    }
    remotePath, err := ValidateFilePath(filename)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名: " + err.Error()}, http.StatusBadRequest)
        return
    }
    
    cfg := h.cfgPtr.Load()
    if !cfg.Versioning.Enabled {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "版本管理未启用"}, http.StatusNotImplemented)
        return
    }
    
    verDir := filepath.Join(cfg.UploadsDir, versionsDirName, remotePath)
    entries, err := os.ReadDir(verDir)
    if os.IsNotExist(err) {
        sendJSONResponse(w, map[string]any{"versions": []VersionInfo{}}, http.StatusOK)
        return
    }
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "读取版本目录失败"}, http.StatusInternalServerError)
        return
    }
    
    versions := make([]VersionInfo, 0, len(entries))
    for _, e := range entries {
        info, err := e.Info()
        if err != nil { continue }
        var versionID int64
        fmt.Sscanf(e.Name(), "%d", &versionID)
        
        fi := VersionInfo{
            Filename:  filepath.ToSlash(remotePath),
            VersionID: versionID,
            Size:      info.Size(),
            CreatedAt: time.Unix(0, versionID).Format(time.RFC3339),
        }
        // 尝试获取 checksum
        if cs, ok := h.checksumStore.Get(fmt.Sprintf("__version__/%s/%d", remotePath, versionID)); ok {
            fi.Checksum = cs
        }
        versions = append(versions, fi)
    }
    
    sendJSONResponse(w, map[string]any{"versions": versions}, http.StatusOK)
}

// restoreVersionHandler 处理 POST /api/versions/restore?filename=xxx&version_id=xxx。
func (h *Handlers) restoreVersionHandler(w http.ResponseWriter, r *http.Request) {
    filename := r.URL.Query().Get("filename")
    versionIDStr := r.URL.Query().Get("version_id")
    if filename == "" || versionIDStr == "" {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
        return
    }
    
    remotePath, err := ValidateFilePath(filename)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名: " + err.Error()}, http.StatusBadRequest)
        return
    }
    
    cfg := h.cfgPtr.Load()
    if !cfg.Versioning.Enabled {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "版本管理未启用"}, http.StatusNotImplemented)
        return
    }
    
    verFile := filepath.Join(cfg.UploadsDir, versionsDirName, remotePath, versionIDStr)
    if _, err := os.Stat(verFile); os.IsNotExist(err) {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
        return
    }
    
    targetPath := filepath.Join(cfg.UploadsDir, remotePath)
    
    // 先保存当前版本（回滚前备份）
    h.saveVersion(remotePath, cfg.UploadsDir)
    
    // 拷贝版本文件到目标位置
    src, err := os.Open(verFile)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "打开版本文件失败"}, http.StatusInternalServerError)
        return
    }
    defer src.Close()
    
    dst, err := os.Create(targetPath)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目标文件失败"}, http.StatusInternalServerError)
        return
    }
    defer dst.Close()
    
    if _, err := io.Copy(dst, src); err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复文件失败"}, http.StatusInternalServerError)
        return
    }
    
    // 更新 checksum
    checksum, _ := FileChecksum(targetPath)
    h.checksumStore.Set(remotePath, checksum)
    
    h.logger.Info("文件版本已恢复", "file_name", remotePath, "version_id", versionIDStr)
    sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("已恢复版本 %s", versionIDStr), Checksum: checksum}, http.StatusOK)
}

// deleteVersionHandler 处理 DELETE /api/versions?filename=xxx&version_id=xxx。
func (h *Handlers) deleteVersionHandler(w http.ResponseWriter, r *http.Request) {
    filename := r.URL.Query().Get("filename")
    versionIDStr := r.URL.Query().Get("version_id")
    if filename == "" || versionIDStr == "" {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "filename 和 version_id 不能为空"}, http.StatusBadRequest)
        return
    }
    
    remotePath, err := ValidateFilePath(filename)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名: " + err.Error()}, http.StatusBadRequest)
        return
    }
    
    cfg := h.cfgPtr.Load()
    verFile := filepath.Join(cfg.UploadsDir, versionsDirName, remotePath, versionIDStr)
    if err := os.Remove(verFile); err != nil {
        if os.IsNotExist(err) {
            sendJSONResponse(w, UploadResponse{Success: false, Message: "版本文件不存在"}, http.StatusNotFound)
        } else {
            sendJSONResponse(w, UploadResponse{Success: false, Message: "删除版本文件失败"}, http.StatusInternalServerError)
        }
        return
    }
    
    sendJSONResponse(w, UploadResponse{Success: true, Message: "版本已删除"}, http.StatusOK)
}
```

### 修改 upload handler 保存版本

在 `handlers.go` 的 `upload` 方法中，在 `os.Stat` 检查文件已存在之后、创建临时文件之前，插入版本保存：

```go
// 在 upload 方法中，文件已存在且校验成功时：先保存版本
if stat, err := os.Stat(filePath); err == nil {
    if !verifyFileWithChecksum(filePath, expectedChecksum) {
        // ... 已有逻辑：校验失败返回 409
    }
    // 幂等返回前，如果版本管理启用则保存当前版本
    cfg := h.cfgPtr.Load()
    if cfg.Versioning.Enabled {
        if _, saveErr := h.saveVersion(remotePath, uploadDir); saveErr != nil {
            h.logger.Warn("保存文件版本失败", "file_name", remotePath, "error", saveErr)
        }
    }
    // ... 继续已有的幂等返回逻辑
}
```

以及在新文件上传且目标文件已存在时（非幂等覆盖场景），在 `os.Rename(tmpPath, filePath)` 之前保存版本。

### 配置扩展

**文件：**
- 修改：`pkg/server/config.go`

```go
type Config struct {
    // ... 现有字段
    
    Versioning VersionConfig `yaml:"versioning" mapstructure:"versioning"`
}

type VersionConfig struct {
    Enabled     bool `yaml:"enabled" mapstructure:"enabled"`
    MaxVersions int  `yaml:"max_versions" mapstructure:"max_versions"`
}
```

**文件：**
- 修改：`config.example.yaml`

```yaml
# 文件版本管理（默认关闭）
# versioning:
#   enabled: true
#   max_versions: 10
```

### 测试

**文件：**
- 新增：`pkg/server/version_test.go`

### 验证

- [ ] `go build ./cmd/sproxy/` 编译成功
- [ ] `go test -run TestVersions -v ./pkg/server/...` 全部通过

---

## 任务 3: P5-4 Prometheus Metrics

**目标版本：** v0.8.0

**文件：**
- 新增：`pkg/server/metrics.go`
- 修改：`pkg/server/handlers.go`（路由注册、metrics handler）
- 修改：`pkg/server/config.go`（MetricsConfig）
- 修改：`config.example.yaml`

### 纯标准库 Prometheus 文本格式

完全使用 `sync/atomic` 计数器，手动输出 Prometheus 文本格式。

```go
package server

import (
    "fmt"
    "sync/atomic"
)

// Metrics 使用 atomic 计数器收集请求统计数据。
// 所有字段对齐到 64-bit 边界，确保 32-bit 平台安全。
type Metrics struct {
    // 请求计数（按状态码分组）
    RequestsTotal      atomic.Int64
    Requests2xx        atomic.Int64
    Requests4xx        atomic.Int64
    Requests5xx        atomic.Int64
    
    // 字节计数
    BytesUploaded      atomic.Int64
    BytesDownloaded    atomic.Int64
    
    // 活跃连接数（通过 handler 进入/退出计数）
    ActiveConnections  atomic.Int64
    
    // 上传/下载文件数
    FilesUploaded      atomic.Int64
    FilesDownloaded    atomic.Int64
    FilesDeleted       atomic.Int64
}

// NewMetrics 创建并初始化 Metrics。
func NewMetrics() *Metrics {
    return &Metrics{}
}

// RecordRequest 根据状态码记录一次请求。
func (m *Metrics) RecordRequest(statusCode int) {
    m.RequestsTotal.Add(1)
    switch {
    case statusCode >= 200 && statusCode < 300:
        m.Requests2xx.Add(1)
    case statusCode >= 400 && statusCode < 500:
        m.Requests4xx.Add(1)
    case statusCode >= 500:
        m.Requests5xx.Add(1)
    }
}

// RecordUpload 记录上传字节数和文件数。
func (m *Metrics) RecordUpload(bytes int64) {
    m.BytesUploaded.Add(bytes)
    m.FilesUploaded.Add(1)
}

// RecordDownload 记录下载字节数和文件数。
func (m *Metrics) RecordDownload(bytes int64) {
    m.BytesDownloaded.Add(bytes)
    m.FilesDownloaded.Add(1)
}

// RecordDelete 记录删除。
func (m *Metrics) RecordDelete() {
    m.FilesDeleted.Add(1)
}

// MetricsHandler 返回 GET /metrics 的 HTTP handler。
// 使用 Prometheus 文本格式（仅标准库，无依赖）。
func (h *Handlers) MetricsHandler(w http.ResponseWriter, r *http.Request) {
    m := h.metrics
    if m == nil {
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        w.WriteHeader(http.StatusOK)
        w.Write([]byte("# No metrics collected\n"))
        return
    }
    
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.WriteHeader(http.StatusOK)
    
    fmt.Fprintf(w, "# HELP sproxy_requests_total Total HTTP requests\n")
    fmt.Fprintf(w, "# TYPE sproxy_requests_total counter\n")
    fmt.Fprintf(w, "sproxy_requests_total %d\n\n", m.RequestsTotal.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_requests_2xx HTTP 2xx requests\n")
    fmt.Fprintf(w, "# TYPE sproxy_requests_2xx counter\n")
    fmt.Fprintf(w, "sproxy_requests_2xx %d\n\n", m.Requests2xx.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_requests_4xx HTTP 4xx requests\n")
    fmt.Fprintf(w, "# TYPE sproxy_requests_4xx counter\n")
    fmt.Fprintf(w, "sproxy_requests_4xx %d\n\n", m.Requests4xx.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_requests_5xx HTTP 5xx requests\n")
    fmt.Fprintf(w, "# TYPE sproxy_requests_5xx counter\n")
    fmt.Fprintf(w, "sproxy_requests_5xx %d\n\n", m.Requests5xx.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_bytes_uploaded Total bytes uploaded\n")
    fmt.Fprintf(w, "# TYPE sproxy_bytes_uploaded counter\n")
    fmt.Fprintf(w, "sproxy_bytes_uploaded %d\n\n", m.BytesUploaded.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_bytes_downloaded Total bytes downloaded\n")
    fmt.Fprintf(w, "# TYPE sproxy_bytes_downloaded counter\n")
    fmt.Fprintf(w, "sproxy_bytes_downloaded %d\n\n", m.BytesDownloaded.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_active_connections Currently active connections\n")
    fmt.Fprintf(w, "# TYPE sproxy_active_connections gauge\n")
    fmt.Fprintf(w, "sproxy_active_connections %d\n\n", m.ActiveConnections.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_files_uploaded Total files uploaded\n")
    fmt.Fprintf(w, "# TYPE sproxy_files_uploaded counter\n")
    fmt.Fprintf(w, "sproxy_files_uploaded %d\n\n", m.FilesUploaded.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_files_downloaded Total files downloaded\n")
    fmt.Fprintf(w, "# TYPE sproxy_files_downloaded counter\n")
    fmt.Fprintf(w, "sproxy_files_downloaded %d\n\n", m.FilesDownloaded.Load())
    
    fmt.Fprintf(w, "# HELP sproxy_files_deleted Total files deleted\n")
    fmt.Fprintf(w, "# TYPE sproxy_files_deleted counter\n")
    fmt.Fprintf(w, "sproxy_files_deleted %d\n\n", m.FilesDeleted.Load())
}
```

### 注册路由

```go
// 在 RegisterRoutes 中添加
mux.HandleFunc("GET /metrics", h.MetricsHandler)
```

### Handlers 结构体扩展

```go
type Handlers struct {
    // ... 现有字段
    metrics       *Metrics
}
```

创建时初始化：
```go
h := &Handlers{
    // ... 现有字段
    metrics: NewMetrics(),
}
```

### 在 handler 中添加指标记录

在 upload、download、delete 等 handler 中调用 `h.metrics.RecordRequest(statusCode)`、`h.metrics.RecordUpload(size)` 等。通过创建 `metricsMiddleware` 自动记录请求状态码：

```go
func (h *Handlers) metricsMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        h.metrics.ActiveConnections.Add(1)
        defer h.metrics.ActiveConnections.Add(-1)
        next.ServeHTTP(w, r)
    })
}
```

### 配置扩展

```go
type Config struct {
    // ...
    MetricsEnabled bool `yaml:"metrics_enabled" mapstructure:"metrics_enabled"`
}
```

### 验证

- [ ] `go build ./cmd/sproxy/` 编译成功
- [ ] 启动服务后 `curl http://localhost:18083/metrics` 返回 Prometheus 文本格式

---

## 任务 4: P5-2 统计监控页面

**目标版本：** v0.8.0

**依赖：** P5-4 Metrics（已完成）

**文件：**
- 新增：`pkg/server/stats.go`
- 修改：`pkg/server/handlers.go`（路由注册）
- 修改：`web/static/index.html`（监控面板 UI）
- 修改：`pkg/server/integration_test.go`

### 统计 API

```go
package server

import (
    "encoding/json"
    "net/http"
    "os"
    "path/filepath"
)

// StatsResponse 是 GET /api/stats 的响应体。
type StatsResponse struct {
    DiskUsage      DiskUsageStats  `json:"disk_usage"`
    RequestCounts  RequestCounts   `json:"request_counts"`
    ActiveConns    int64           `json:"active_connections"`
    FilesUploaded  int64           `json:"files_uploaded"`
    FilesDownloaded int64          `json:"files_downloaded"`
    FilesDeleted   int64           `json:"files_deleted"`
    BytesUploaded  int64           `json:"bytes_uploaded"`
    BytesDownloaded int64          `json:"bytes_downloaded"`
}

type DiskUsageStats struct {
    UploadsDir    string `json:"uploads_dir"`
    TotalFiles    int    `json:"total_files"`
    TotalSize     int64  `json:"total_size"`
    FreeSpace     int64  `json:"free_space"`
}

type RequestCounts struct {
    Total int64 `json:"total"`
    _2xx  int64 `json:"2xx"`
    _4xx  int64 `json:"4xx"`
    _5xx  int64 `json:"5xx"`
}

// statsHandler 处理 GET /api/stats。
func (h *Handlers) statsHandler(w http.ResponseWriter, r *http.Request) {
    cfg := h.cfgPtr.Load()
    m := h.metrics
    
    // 文件统计（遍历目录）
    totalFiles := 0
    var totalSize int64
    filepath.WalkDir(cfg.UploadsDir, func(path string, d os.DirEntry, err error) error {
        if err != nil { return nil }
        if d.IsDir() { return nil }
        if d.Name() == ".checksums.json" { return nil }
        if strings.HasPrefix(path, filepath.Join(cfg.UploadsDir, chunkedDirName)) { return filepath.SkipDir }
        info, err := d.Info()
        if err != nil { return nil }
        totalFiles++
        totalSize += info.Size()
        return nil
    })
    
    // 磁盘剩余空间
    var freeSpace int64
    // 使用 os.Stat 所在文件系统信息
    // 跨平台兼容：Windows 上没有 syscall.Statfs_t，用简单方式
    // 这里先跳过，设为 -1 表示未知
    freeSpace = -1
    
    resp := StatsResponse{
        DiskUsage: DiskUsageStats{
            UploadsDir: cfg.UploadsDir,
            TotalFiles: totalFiles,
            TotalSize:  totalSize,
            FreeSpace:  freeSpace,
        },
        FilesUploaded:   m.FilesUploaded.Load(),
        FilesDownloaded: m.FilesDownloaded.Load(),
        FilesDeleted:    m.FilesDeleted.Load(),
        BytesUploaded:   m.BytesUploaded.Load(),
        BytesDownloaded: m.BytesDownloaded.Load(),
    }
    
    if m != nil {
        resp.ActiveConns = m.ActiveConnections.Load()
        resp.RequestCounts = RequestCounts{
            Total: m.RequestsTotal.Load(),
            _2xx:  m.Requests2xx.Load(),
            _4xx:  m.Requests4xx.Load(),
            _5xx:  m.Requests5xx.Load(),
        }
    }
    
    sendJSONResponse(w, resp, http.StatusOK)
}
```

### Web UI 监控面板

在 `web/static/index.html` 中添加一个监控页面入口和面板。作为 Web UI 的一个独立标签页，显示磁盘使用、请求统计、活跃连接等。

```html
<!-- 在 toolbar 中添加 -->
<button class="btn btn-secondary" onclick="showStats()">监控面板</button>

<!-- 监控面板 HTML（初始隐藏） -->
<div id="stats-panel" style="display:none;">
  <h2>服务器统计</h2>
  <div id="stats-content">加载中...</div>
  <button class="btn btn-secondary" onclick="hideStats()">关闭</button>
</div>
```

### 验证

- [ ] `go build ./cmd/sproxy/` 编译成功
- [ ] `curl http://localhost:18083/api/stats` 返回 JSON

---

## 任务 5: P5-3 文件分享链接

**目标版本：** v0.8.0

**文件：**
- 新增：`pkg/server/share.go`
- 新增：`pkg/server/share_test.go`
- 修改：`pkg/server/handlers.go`（路由注册）
- 修改：`pkg/server/config.go`（ShareConfig）
- 修改：`web/static/index.html`（分享 UI）

### 分享链接实现

使用 `crypto/rand` 生成安全随机 token，内存存储（`map[string]*ShareLink`）：

```go
package server

import (
    "crypto/rand"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "net/http"
    "path/filepath"
    "sync"
    "time"
)

type ShareConfig struct {
    Enabled   bool          `yaml:"enabled" mapstructure:"enabled"`
    MaxExpiry time.Duration `yaml:"max_expiry" mapstructure:"max_expiry"` // 最大有效期，如 "24h"
}

type ShareLink struct {
    Token      string    `json:"token"`
    Filename   string    `json:"filename"`
    Checksum   string    `json:"checksum"`
    CreatedAt  time.Time `json:"created_at"`
    ExpiresAt  time.Time `json:"expires_at"`
    MaxDownloads int     `json:"max_downloads"` // 0 = 无限
    Downloads  int       `json:"downloads"`
    OneTime    bool      `json:"one_time"` // 一次性：下载后自动删除
}

// ShareStore 是分享链接的内存存储。
type ShareStore struct {
    mu     sync.RWMutex
    links  map[string]*ShareLink
    logger *slog.Logger
}

func NewShareStore(logger *slog.Logger) *ShareStore {
    return &ShareStore{
        links:  make(map[string]*ShareLink),
        logger: logger,
    }
}

func (s *ShareStore) Create(filename, checksum string, expiresAt time.Time, maxDownloads int, oneTime bool) (*ShareLink, error) {
    tokenBytes := make([]byte, 16)
    if _, err := rand.Read(tokenBytes); err != nil {
        return nil, fmt.Errorf("生成 token 失败: %w", err)
    }
    token := hex.EncodeToString(tokenBytes)
    
    link := &ShareLink{
        Token:      token,
        Filename:   filename,
        Checksum:   checksum,
        CreatedAt:  time.Now(),
        ExpiresAt:  expiresAt,
        MaxDownloads: maxDownloads,
        OneTime:    oneTime,
    }
    
    s.mu.Lock()
    s.links[token] = link
    s.mu.Unlock()
    
    return link, nil
}

func (s *ShareStore) Get(token string) *ShareLink {
    s.mu.RLock()
    defer s.mu.RUnlock()
    link, ok := s.links[token]
    if !ok { return nil }
    return link
}

func (s *ShareStore) Delete(token string) {
    s.mu.Lock()
    delete(s.links, token)
    s.mu.Unlock()
}

// shareCreateHandler 处理 POST /api/share。
// 请求体 JSON：{"filename":"...", "checksum":"...", "expires_in":"1h", "max_downloads":0, "one_time":false}
func (h *Handlers) shareCreateHandler(w http.ResponseWriter, r *http.Request) {
    cfg := h.cfgPtr.Load()
    if !cfg.Share.Enabled {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "分享功能未启用"}, http.StatusNotImplemented)
        return
    }
    
    r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
    var req struct {
        Filename      string `json:"filename"`
        Checksum      string `json:"checksum"`
        ExpiresIn     string `json:"expires_in"`    // "1h", "24h", etc.
        MaxDownloads  int    `json:"max_downloads"`  // 0 = 无限
        OneTime       bool   `json:"one_time"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体: " + err.Error()}, http.StatusBadRequest)
        return
    }
    
    remotePath, err := ValidateFilePath(req.Filename)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名: " + err.Error()}, http.StatusBadRequest)
        return
    }
    
    // 检查文件存在
    fullPath := filepath.Join(cfg.UploadsDir, remotePath)
    if _, err := os.Stat(fullPath); os.IsNotExist(err) {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
        return
    }
    
    // 解析有效期
    expiresAt := time.Now().Add(24 * time.Hour) // 默认 24h
    if req.ExpiresIn != "" {
        d, err := time.ParseDuration(req.ExpiresIn)
        if err == nil && d > 0 {
            if d > cfg.Share.MaxExpiry {
                d = cfg.Share.MaxExpiry
            }
            expiresAt = time.Now().Add(d)
        }
    }
    
    link, err := h.shareStore.Create(remotePath, req.Checksum, expiresAt, req.MaxDownloads, req.OneTime)
    if err != nil {
        sendJSONResponse(w, UploadResponse{Success: false, Message: "创建分享链接失败"}, http.StatusInternalServerError)
        return
    }
    
    h.logger.Info("分享链接已创建", "token", link.Token[:8]+"...", "file_name", remotePath, "expires_at", expiresAt)
    sendJSONResponse(w, map[string]any{
        "success": true,
        "token":   link.Token,
        "url":     fmt.Sprintf("/share/%s", link.Token),
        "expires_at": expiresAt.Format(time.RFC3339),
    }, http.StatusOK)
}

// shareDownloadHandler 处理 GET /share/{token}。
// 验证 token 有效性后重定向到 /download?filename=xxx。
func (h *Handlers) shareDownloadHandler(w http.ResponseWriter, r *http.Request) {
    token := r.PathValue("token")
    if token == "" {
        http.Error(w, "missing token", http.StatusBadRequest)
        return
    }
    
    link := h.shareStore.Get(token)
    if link == nil {
        http.Error(w, "分享链接不存在或已失效", http.StatusNotFound)
        return
    }
    
    if time.Now().After(link.ExpiresAt) {
        h.shareStore.Delete(token)
        http.Error(w, "分享链接已过期", http.StatusGone)
        return
    }
    
    if link.MaxDownloads > 0 && link.Downloads >= link.MaxDownloads {
        h.shareStore.Delete(token)
        http.Error(w, "分享链接已达下载次数上限", http.StatusGone)
        return
    }
    
    // 递增下载次数
    link.Downloads++
    
    // 一次性分享：下载后删除
    if link.OneTime {
        defer h.shareStore.Delete(token)
    }
    
    // 重定向到下载
    http.Redirect(w, r, "/download?filename=" + url.QueryEscape(link.Filename), http.StatusFound)
}
```

### Web UI 分享按钮

在每个文件行添加"分享"按钮：

```javascript
// 在 render 函数中为每个文件行添加分享按钮
<button class="btn btn-sm btn-primary" onclick="shareFile('${f.name}', '${f.checksum}')">分享</button>

function shareFile(filename, checksum) {
    const duration = prompt('有效期（如 1h, 24h, 7d），留空默认 24h：');
    fetch(BASE + '/api/share', {
        method: 'POST',
        headers: Object.assign(headers(), {'Content-Type': 'application/json'}),
        body: JSON.stringify({
            filename: currentDir ? currentDir + '/' + filename : filename,
            checksum: checksum,
            expires_in: duration || '24h',
            one_time: false
        })
    }).then(r => r.json()).then(data => {
        if (data.success) {
            const shareUrl = window.location.origin + BASE + '/share/' + data.token;
            prompt('分享链接（复制到浏览器打开）：', shareUrl);
            showToast('分享链接已生成', 'success');
        } else {
            showToast('分享失败: ' + data.message, 'error');
        }
    });
}
```

### 路由注册

```go
mux.HandleFunc("POST /api/share", h.authMiddleware(h.shareCreateHandler))
mux.HandleFunc("GET /share/{token}", h.shareDownloadHandler) // 无需 auth
```

### 配置扩展

```go
type Config struct {
    // ...
    Share ShareConfig `yaml:"share" mapstructure:"share"`
}
```

### 测试

```go
func TestShareLink_CreateUse(t *testing.T) {
    ts, cleanup := newTestServerWithAllRoutes(t)
    defer cleanup()
    
    // 需要启用 share 功能，使用完整配置
    uploadFile(t, ts, "test.txt", "hello", sha256hex("hello"))
    
    body := `{"filename":"test.txt","checksum":"...","expires_in":"1h"}`
    resp, _ := ts.Client().Post(ts.URL+"/api/share", "application/json", strings.NewReader(body))
    // 如果 share.enabled 为 false，期望 501
    // ...
}
```

### 验证

- [ ] `go build ./cmd/sproxy/` 编译成功
- [ ] `go test -run TestShareLink ./pkg/server/...` 全部通过

---

## 任务 6: P5-5 多用户权限系统

**目标版本：** v0.8.0

**依赖：** 所有上述功能完成

**文件：**
- 新增：`pkg/server/auth.go`
- 修改：`pkg/server/config.go`（APIKeyConfig）
- 修改：`pkg/server/handlers.go`（authMiddleware 改造）
- 修改：`config.example.yaml`
- 修改：`web/static/index.html`

### API Key 管理

```go
package server

import (
    "crypto/rand"
    "crypto/subtle"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "log/slog"
    "net/http"
    "sync"
    "time"
)

// Permission 表示权限级别。
type Permission int

const (
    PermissionRead   Permission = iota // 只读：list, download, stat, search
    PermissionWrite                    // 读写：upload, create dir
    PermissionDelete                   // 删除：delete, rmdir
    PermissionAdmin                    // 管理：所有操作 + 管理 key
)

type APIKey struct {
    Key         string     `json:"key"`
    Name        string     `json:"name"`
    Permission  Permission `json:"permission"`
    CreatedAt   time.Time  `json:"created_at"`
    Enabled     bool       `json:"enabled"`
}

// KeyStore 管理 API Key 的内存存储。
type KeyStore struct {
    mu       sync.RWMutex
    keys     map[string]*APIKey
    logger   *slog.Logger
}

type AuthConfig struct {
    // 兼容旧的 AuthToken 字段
    // 新的多用户配置
    APIKeys []APIKeyConfig `yaml:"api_keys" mapstructure:"api_keys"`
}

type APIKeyConfig struct {
    Name       string `yaml:"name" mapstructure:"name"`
    Key        string `yaml:"key" mapstructure:"key"`       // 留空则自动生成
    Permission string `yaml:"permission" mapstructure:"permission"` // read, write, delete, admin
}
```

### authMiddleware 改造

```go
func (h *Handlers) authMiddleware(next http.HandlerFunc, required Permission) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        cfg := h.cfgPtr.Load()
        if cfg == nil || (cfg.AuthToken == "" && len(cfg.Auth.APIKeys) == 0) {
            next(w, r)
            return
        }
        
        auth := r.Header.Get("Authorization")
        if !strings.HasPrefix(auth, "Bearer ") {
            http.Error(w, "unauthorized", http.StatusUnauthorized)
            return
        }
        token := strings.TrimPrefix(auth, "Bearer ")
        
        // 先尝试旧式 AuthToken
        if cfg.AuthToken != "" {
            if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AuthToken)) == 1 {
                next(w, r)
                return
            }
        }
        
        // 尝试 API Key
        if key, ok := h.keyStore.Authenticate(token); ok {
            if key.Permission >= required {
                next(w, r)
                return
            }
            http.Error(w, "permission denied", http.StatusForbidden)
            return
        }
        
        http.Error(w, "unauthorized", http.StatusUnauthorized)
    }
}
```

由于 `authMiddleware` 签名变了（增加 `Permission` 参数），所有注册路由的地方需要相应更新。

### 配置

```yaml
# 多用户 API Key 管理（需 auth_token 启用）
# auth_token: "your-secret-token"
# auth:
#   api_keys:
#     - name: "alice"
#       key: "key_alice_read_xxx"  # 留空自动生成
#       permission: "read"
#     - name: "bob"
#       key: "key_bob_write_xxx"
#       permission: "write"
```

### 验证

- [ ] `go build ./cmd/sproxy/` 编译成功
- [ ] `curl -H "Authorization: Bearer <read_key>" http://localhost:18083/api/files` 正常
- [ ] `curl -H "Authorization: Bearer <read_key>" -X POST http://localhost:18083/delete?filename=x` 返回 403

---

## 版本标记

全部完成后：

```bash
export VERSION=v0.8.0
git tag -a $VERSION -m "Phase 5: archive, versioning, metrics, stats, share links, auth"
git push origin $VERSION
```

## 验证方式

全局验证命令（每次 commit 前执行）：
```bash
go build ./cmd/sproxy/
go build ./cmd/sclient/
go vet ./...
go test -race ./... 2>&1
```