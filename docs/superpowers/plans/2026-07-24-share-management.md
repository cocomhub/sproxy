# 第一期：分享管理模块实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 的分享功能增加管理能力：Web UI 实现分享列表/撤销界面，CLI 新增 `share create`/`list`/`revoke` 命令，后端新增 `GET /api/shares` 和 `DELETE /api/shares/{token}` 端点。

**架构：** 后端扩展 `ShareStore` 增加 `List()`/`Revoke()` 方法 + 两个新 handler → 客户端 SDK 新增三个方法 → CLI 新增三个子命令（参考 `version` 命令的嵌套模式）→ Web UI 升级分享弹窗为双标签页。

**技术栈：** Go 1.26, stdlib-only (net/http, slog), cobra+viper, vanilla JS, 纯标准库测试

---

## 文件结构

### 新增文件
- `pkg/client/share.go` — 客户端 SDK 分享方法
- `pkg/client/share_test.go` — 客户端 SDK 测试
- `cmd/sclient/share.go` — CLI share 命令及子命令
- `cmd/sclient/share_test.go` — CLI share 命令测试

### 修改文件
- `pkg/server/share.go` — 新增 `CreatedAt` 字段、`List()`/`Revoke()` 方法、两个 handler
- `pkg/server/share_test.go` — 新增 List/Revoke 测试用例
- `pkg/server/handlers.go` — 注册 `GET /api/shares`、`DELETE /api/shares/{token}`（localMux + 主 mux）
- `cmd/sclient/root.go` — 在 `init()` 中注册 `shareCmd`
- `web/static/index.html` — 新增分享管理弹窗（或升级现有分享弹窗为双标签页）
- `web/static/app.js` — 分享弹窗双标签页逻辑 + 管理功能

---

## 任务 1：后端数据模型扩展

**文件：**
- 修改：`pkg/server/share.go:26-34`

### 步骤 1.1：ShareLink 增加 CreatedAt 字段

在 `ShareLink` 结构体中增加 `CreatedAt` 字段：

```go
// ShareLink 表示一个文件分享链接。
type ShareLink struct {
	Token        string    `json:"token"`
	Filename     string    `json:"filename"`
	AbsPath      string    `json:"-"` // 创建时解析的绝对路径
	CreatedAt    time.Time `json:"created_at"`    // 新增
	ExpiresAt    time.Time `json:"expires_at"`
	MaxDownloads int       `json:"max_downloads"` // 0 = 不限
	Downloads    int       `json:"downloads"`
	OneTime      bool      `json:"one_time"`
}
```

### 步骤 1.2：Create 方法设置 CreatedAt

在 `Create()` 方法中初始化 `CreatedAt`（第 58 行后增加）：

```go
link := &ShareLink{
	Token:        token,
	Filename:     filename,
	AbsPath:      absPath,
	CreatedAt:    time.Now(),       // 新增
	ExpiresAt:    time.Now().Add(ttl),
	MaxDownloads: maxDownloads,
	OneTime:      oneTime,
}
```

### 步骤 1.3：新增 List() 方法

在 `Consume()` 方法之后增加：

```go
// List 返回所有分享链接，标记已过期/已消耗的链接为 expired=true。
func (s *ShareStore) List() []*ShareLink {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*ShareLink, 0, len(s.links))
	for _, link := range s.links {
		// 复制一份，避免外部修改内部状态
		cp := *link
		result = append(result, &cp)
	}
	return result
}
```

### 步骤 1.4：新增 Revoke() 方法

```go
// Revoke 删除指定 token 的分享链接。链接不存在时返回 error。
func (s *ShareStore) Revoke(token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.links[token]; !ok {
		return fmt.Errorf("分享链接不存在: %s", token)
	}
	delete(s.links, token)
	return nil
}
```

### 步骤 1.5：验证编译

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./pkg/server/
```

---

## 任务 2：后端新增 handler（listSharesHandler + revokeShareHandler）

**文件：**
- 修改：`pkg/server/share.go`（handler 追加在文件末尾）
- 修改：`pkg/server/handlers.go`（路由注册）

### 步骤 2.1：编写 listSharesHandler

在 `accessShareHandler` 之后追加：

```go
// listSharesHandler 处理 GET /api/shares，返回所有活跃分享链接列表。
func (h *Handlers) listSharesHandler(w http.ResponseWriter, r *http.Request) {
	links := h.shareStore.List()

	// 构建响应：标记每个链接是否已过期/已消耗
	type shareItem struct {
		Token        string `json:"token"`
		Filename     string `json:"filename"`
		CreatedAt    string `json:"created_at"`
		ExpiresAt    string `json:"expires_at"`
		MaxDownloads int    `json:"max_downloads"`
		Downloads    int    `json:"downloads"`
		OneTime      bool   `json:"one_time"`
		Expired      bool   `json:"expired"`
	}

	now := time.Now()
	items := make([]shareItem, 0, len(links))
	for _, l := range links {
		expired := now.After(l.ExpiresAt) || (l.MaxDownloads > 0 && l.Downloads >= l.MaxDownloads)
		items = append(items, shareItem{
			Token:        l.Token,
			Filename:     l.Filename,
			CreatedAt:    l.CreatedAt.Format(time.RFC3339),
			ExpiresAt:    l.ExpiresAt.Format(time.RFC3339),
			MaxDownloads: l.MaxDownloads,
			Downloads:    l.Downloads,
			OneTime:      l.OneTime,
			Expired:      expired,
		})
	}

	sendJSONResponse(w, map[string]any{"shares": items}, http.StatusOK)
}
```

### 步骤 2.2：编写 revokeShareHandler

```go
// revokeShareHandler 处理 DELETE /api/shares/{token}，撤销指定分享链接。
func (h *Handlers) revokeShareHandler(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "token 不能为空"}, http.StatusBadRequest)
		return
	}

	if err := h.shareStore.Revoke(token); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusNotFound)
		return
	}

	sendJSONResponse(w, UploadResponse{Success: true, Message: "分享链接已撤销"}, http.StatusOK)
}
```

### 步骤 2.3：注册路由

在 `pkg/server/handlers.go` 中找到 share 相关路由（第 157-158 行），在其后增加：

```go
// 在 localMux 中注册（隧道内部使用）
localMux.HandleFunc("GET /api/shares", h.listSharesHandler)
localMux.HandleFunc("DELETE /api/shares/{token}", h.revokeShareHandler)

// 在主 mux 中注册（带 Bearer auth）
mux.HandleFunc("GET /api/shares", h.authMiddleware(h.listSharesHandler))
mux.HandleFunc("DELETE /api/shares/{token}", h.authMiddleware(h.revokeShareHandler))
```

### 步骤 2.4：验证编译

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./pkg/server/ && go build ./cmd/sproxy/
```

---

## 任务 3：后端测试

**文件：**
- 修改：`pkg/server/share_test.go`

### 步骤 3.1：编写 TestShare_List 测试

追加到 `share_test.go` 末尾：

```go
func TestShare_List(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 上传文件
	body := []byte("list test content")
	uploadFile(t, url, "list_test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 创建分享链接
	reqBody := `{"filename":"list_test.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// 列出分享
	resp2, err := http.Get(url + "/api/shares")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}

	var result struct {
		Shares []struct {
			Token        string `json:"token"`
			Filename     string `json:"filename"`
			CreatedAt    string `json:"created_at"`
			ExpiresAt    string `json:"expires_at"`
			MaxDownloads int    `json:"max_downloads"`
			Downloads    int    `json:"downloads"`
			OneTime      bool   `json:"one_time"`
			Expired      bool   `json:"expired"`
		} `json:"shares"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if len(result.Shares) == 0 {
		t.Fatal("expected at least 1 share")
	}
	found := false
	for _, s := range result.Shares {
		if s.Filename == "list_test.txt" {
			found = true
			if s.Token == "" {
				t.Error("expected non-empty token")
			}
			if s.CreatedAt == "" {
				t.Error("expected non-empty created_at")
			}
			if s.Expired {
				t.Error("expected expired=false for a valid share")
			}
			break
		}
	}
	if !found {
		t.Error("share for list_test.txt not found in list")
	}
}
```

### 步骤 3.2：编写 TestShare_Revoke 测试

```go
func TestShare_Revoke(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	body := []byte("revoke test content")
	uploadFile(t, url, "revoke_test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 创建分享
	reqBody := `{"filename":"revoke_test.txt","ttl":"1h"}`
	resp, err := http.Post(url+"/api/share", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}

	var shareResp map[string]any
	json.NewDecoder(resp.Body).Decode(&shareResp)
	resp.Body.Close()
	token := shareResp["token"].(string)

	// 撤销分享
	req2, err := http.NewRequest(http.MethodDelete, url+"/api/shares/"+token, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 revoking share, got %d", resp2.StatusCode)
	}

	// 确认访问返回 404
	resp3, err := http.Get(url + "/s/" + token)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after revoke, got %d", resp3.StatusCode)
	}
}

func TestShare_RevokeNotFound(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest(http.MethodDelete, url+"/api/shares/nonexistent_token", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for non-existent token, got %d", resp.StatusCode)
	}
}
```

### 步骤 3.3：运行测试验证

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestShare_List|TestShare_Revoke|TestShare_RevokeNotFound' ./pkg/server/ -v
```

预期：全部 PASS

---

## 任务 4：客户端 SDK 新增分享方法

**文件：**
- 创建：`pkg/client/share.go`
- 创建：`pkg/client/share_test.go`

### 步骤 4.1：编写 ShareLink 结构体和 SDK 方法

创建 `pkg/client/share.go`：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ShareLink 表示服务端返回的分享链接信息。
type ShareLink struct {
	Token        string `json:"token"`
	Filename     string `json:"filename"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
	MaxDownloads int    `json:"max_downloads"`
	Downloads    int    `json:"downloads"`
	OneTime      bool   `json:"one_time"`
	Expired      bool   `json:"expired"`
}

// CreateShare 创建文件分享链接，返回分享链接信息。
// ttl 为 0 时使用服务端默认值（24h），maxDownloads 为 0 表示不限次数。
func (c *FileClient) CreateShare(ctx context.Context, filename string, ttl time.Duration, maxDownloads int, oneTime bool) (*ShareLink, error) {
	body := map[string]any{
		"filename": filename,
		"ttl":      ttl.String(),
		"max_downloads": maxDownloads,
		"one_time": oneTime,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resp, err := c.doRequest(ctx, "POST", "/api/share", bytes.NewReader(jsonBody), headers)
	if err != nil {
		return nil, fmt.Errorf("创建分享链接失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var result UploadResult
		if json.Unmarshal(respBody, &result) == nil && result.Message != "" {
			return nil, fmt.Errorf("创建分享链接失败: %s", result.Message)
		}
		return nil, fmt.Errorf("创建分享链接失败 (HTTP %d)", resp.StatusCode)
	}

	var link ShareLink
	if err := json.Unmarshal(respBody, &link); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return &link, nil
}

// ListShares 列出当前所有活跃的分享链接。
func (c *FileClient) ListShares(ctx context.Context) ([]*ShareLink, error) {
	resp, err := c.doRequest(ctx, "GET", "/api/shares", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("获取分享列表失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("获取分享列表失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Shares []*ShareLink `json:"shares"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return result.Shares, nil
}

// RevokeShare 撤销指定 token 的分享链接。
func (c *FileClient) RevokeShare(ctx context.Context, token string) error {
	apiPath := "/api/shares/" + url.PathEscape(token)
	resp, err := c.doRequest(ctx, "DELETE", apiPath, nil, nil)
	if err != nil {
		return fmt.Errorf("撤销分享链接失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var result UploadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("解析响应失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	if !result.Success {
		return fmt.Errorf("撤销失败: %s", result.Message)
	}
	return nil
}
```

### 步骤 4.2：编写客户端 SDK 测试

创建 `pkg/client/share_test.go`：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCreateShare(t *testing.T) {
	t.Parallel()

	// mock 服务端：处理 POST /api/share
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/share" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"abc123","filename":"test.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	link, err := c.CreateShare(context.Background(), "test.txt", time.Hour, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if link.Token != "abc123" {
		t.Errorf("expected token abc123, got %s", link.Token)
	}
	if link.Filename != "test.txt" {
		t.Errorf("expected filename test.txt, got %s", link.Filename)
	}
}

func TestListShares(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api/shares" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"shares":[{"token":"abc","filename":"a.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false,"expired":false}]}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	shares, err := c.ListShares(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 {
		t.Fatalf("expected 1 share, got %d", len(shares))
	}
	if shares[0].Token != "abc" {
		t.Errorf("expected token abc, got %s", shares[0].Token)
	}
}

func TestRevokeShare(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/api/shares/test_token" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"success":true,"message":"分享链接已撤销"}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(context.Background(), "test_token"); err != nil {
		t.Fatal(err)
	}
}

func TestRevokeShareNotFound(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"message":"分享链接不存在"}`))
	}))
	defer ts.Close()

	c := NewFileClient(ts.URL)
	if err := c.RevokeShare(context.Background(), "nonexistent"); err == nil {
		t.Fatal("expected error for non-existent token")
	}
}
```

### 步骤 4.3：运行测试验证

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestCreateShare|TestListShares|TestRevokeShare' ./pkg/client/ -v
```

预期：全部 PASS

---

## 任务 5：CLI 新增 share 子命令

**文件：**
- 创建：`cmd/sclient/share.go`
- 创建：`cmd/sclient/share_test.go`
- 修改：`cmd/sclient/root.go`

### 步骤 5.1：编写 share 命令

创建 `cmd/sclient/share.go`，参照 `cmd/sclient/version.go` 的嵌套子命令模式：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var shareCmd = &cobra.Command{
	Use:   "share",
	Short: "文件分享管理",
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
}

var shareCreateCmd = &cobra.Command{
	Use:   "create <filename>",
	Short: "创建文件分享链接",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		ttlStr, _ := cmd.Flags().GetString("ttl")
		ttl := 24 * time.Hour
		if ttlStr != "" {
			d, parseErr := time.ParseDuration(ttlStr)
			if parseErr == nil && d > 0 {
				ttl = d
			}
		}
		maxDownloads, _ := cmd.Flags().GetInt("max-downloads")
		oneTime, _ := cmd.Flags().GetBool("one-time")

		link, err := cli.CreateShare(cmd.Context(), args[0], ttl, maxDownloads, oneTime)
		if err != nil {
			fmt.Fprintf(os.Stderr, "创建分享链接失败: %v\n", err)
			return fmt.Errorf("创建分享链接失败: %w", err)
		}

		// 获取服务器地址用于构造完整 URL
		serverURL, _ := cmd.Flags().GetString("server")
		if serverURL == "" && cfgProvider != nil {
			cfg, cfgErr := client.LoadFromProvider(cfgProvider)
			if cfgErr == nil {
				serverURL = cfg.ServerURL
			}
		}
		shareURL := serverURL + "/s/" + link.Token

		fmt.Printf("分享链接: %s\n", shareURL)
		fmt.Printf("Token: %s\n", link.Token)
		fmt.Printf("有效期至: %s\n", link.ExpiresAt)
		fmt.Printf("最大下载次数: %d\n", link.MaxDownloads)
		fmt.Printf("一次性: %v\n", link.OneTime)
		return nil
	},
}

var shareListCmd = &cobra.Command{
	Use:   "list",
	Short: "列出所有分享链接",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		shares, err := cli.ListShares(cmd.Context())
		if err != nil {
			fmt.Fprintf(os.Stderr, "获取分享列表失败: %v\n", err)
			return fmt.Errorf("获取分享列表失败: %w", err)
		}

		if len(shares) == 0 {
			fmt.Println("暂无分享链接")
			return nil
		}

		// 输出表格
		fmt.Printf("%-36s  %-40s  %-10s  %s\n", "TOKEN", "FILENAME", "STATUS", "DOWNLOADS")
		for _, s := range shares {
			status := "活跃"
			if s.Expired {
				status = "已过期"
			}
			downloads := fmt.Sprintf("%d/%d", s.Downloads, s.MaxDownloads)
			if s.MaxDownloads == 0 {
				downloads = fmt.Sprintf("%d/∞", s.Downloads)
			}
			// 缩短 token 显示
			shortToken := s.Token
			if len(shortToken) > 36 {
				shortToken = shortToken[:16] + "..." + shortToken[len(shortToken)-16:]
			}
			fmt.Printf("%-36s  %-40s  %-10s  %s\n", shortToken, s.Filename, status, downloads)
		}
		return nil
	},
}

var shareRevokeCmd = &cobra.Command{
	Use:   "revoke <token>",
	Short: "撤销分享链接",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cli, err := buildFileClient(cmd)
		if err != nil {
			return err
		}

		if err := cli.RevokeShare(cmd.Context(), args[0]); err != nil {
			fmt.Fprintf(os.Stderr, "撤销分享链接失败: %v\n", err)
			return fmt.Errorf("撤销分享链接失败: %w", err)
		}

		fmt.Printf("已撤销分享: %s\n", args[0])
		return nil
	},
}

func init() {
	shareCreateCmd.Flags().String("ttl", "24h", "有效期（例如 1h, 24h, 7d, 30d）")
	shareCreateCmd.Flags().Int("max-downloads", 0, "最大下载次数（0=不限）")
	shareCreateCmd.Flags().Bool("one-time", false, "一次性分享（下载一次后自动失效）")

	shareCmd.AddCommand(shareCreateCmd)
	shareCmd.AddCommand(shareListCmd)
	shareCmd.AddCommand(shareRevokeCmd)
}
```

### 步骤 5.2：注册 shareCmd 到 rootCmd

在 `cmd/sclient/root.go` 的 `init()` 函数中（第 84-96 行），增加：

```go
rootCmd.AddCommand(shareCmd)
```

### 步骤 5.3：编写 CLI 测试

创建 `cmd/sclient/share_test.go`：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func TestShareCmd_Usage(t *testing.T) {
	t.Parallel()
	cmd := shareCmd
	if cmd.Use != "share" {
		t.Errorf("expected Use=share, got %s", cmd.Use)
	}
	if cmd.Short != "文件分享管理" {
		t.Errorf("expected Short=文件分享管理, got %s", cmd.Short)
	}
}

func TestShareCmd_HasSubcommands(t *testing.T) {
	t.Parallel()
	cmds := shareCmd.Commands()
	names := make(map[string]bool)
	for _, c := range cmds {
		names[c.Name()] = true
	}
	for _, name := range []string{"create", "list", "revoke"} {
		if !names[name] {
			t.Errorf("expected subcommand %s, not found", name)
		}
	}
}

func TestShareCreateCmd_Flags(t *testing.T) {
	t.Parallel()
	f := shareCreateCmd.Flags()
	ttl, err := f.GetString("ttl")
	if err != nil || ttl != "24h" {
		t.Errorf("expected --ttl default 24h, got %v", ttl)
	}
	maxDL, _ := f.GetInt("max-downloads")
	if maxDL != 0 {
		t.Errorf("expected --max-downloads default 0, got %d", maxDL)
	}
	oneTime, _ := f.GetBool("one-time")
	if oneTime {
		t.Errorf("expected --one-time default false")
	}
}

func TestShareListCmd_NoArgs(t *testing.T) {
	t.Parallel()
	if !shareListCmd.Args(cobra.Command{}, nil) {
		t.Error("expected NoArgs to accept nil")
	}
}

func TestShareCmd_Integration(t *testing.T) {
	t.Parallel()

	// 创建 mock 服务端
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/share":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"token":"test123","filename":"file.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false}`))
		case "/api/shares":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"shares":[{"token":"test123","filename":"file.txt","created_at":"2026-07-24T12:00:00Z","expires_at":"2026-07-25T12:00:00Z","max_downloads":0,"downloads":0,"one_time":false,"expired":false}]}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer ts.Close()

	// 测试 share create
	output := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "share", "create", "file.txt", "--ttl", "24h"})
		_ = rootCmd.Execute()
	})

	if !strings.Contains(output, "test123") {
		t.Errorf("expected output to contain token, got: %s", output)
	}

	// 测试 share list
	output2 := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "share", "list"})
		_ = rootCmd.Execute()
	})
	if !strings.Contains(output2, "file.txt") {
		t.Errorf("expected output to contain filename, got: %s", output2)
	}
}

func TestShareCmd_Revoke(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" && r.URL.Path == "/api/shares/test123" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"success":true,"message":"分享链接已撤销"}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	output := testutil.CaptureStdout(func() {
		rootCmd.SetArgs([]string{"--server", ts.URL, "share", "revoke", "test123"})
		_ = rootCmd.Execute()
	})

	if !strings.Contains(output, "test123") {
		t.Errorf("expected output to contain token, got: %s", output)
	}
}
```

### 步骤 5.4：验证编译 + 测试

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./cmd/sclient/
go test -count=1 -run 'TestShareCmd' ./cmd/sclient/ -v
```

预期：全部 PASS

---

## 任务 6：Web UI 升级分享弹窗

**文件：**
- 修改：`web/static/index.html`
- 修改：`web/static/app.js`

### 步骤 6.1：在 index.html 中新增分享管理弹窗

在 `index.html` 中，在云端下载弹窗（`cloud-modal`）之后、版本管理弹窗（`version-modal`）之前，插入分享管理弹窗：

```html
<!-- 分享管理弹窗 -->
<div id="share-modal" style="display:none;position:fixed;inset:0;background:rgba(0,0,0,.45);z-index:1000;align-items:center;justify-content:center;">
  <div style="background:#fff;border-radius:8px;padding:24px;width:600px;max-width:92vw;max-height:80vh;overflow-y:auto;box-shadow:0 8px 32px rgba(0,0,0,.2);">
    <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px;">
      <h3 style="margin:0;font-size:16px;">分享管理</h3>
      <button type="button" id="share-close-btn" style="background:none;border:none;font-size:20px;cursor:pointer;color:#888;line-height:1;">&times;</button>
    </div>
    <div id="share-tab-bar" style="display:flex;gap:0;margin-bottom:12px;border-bottom:2px solid #e0e0e0;">
      <button type="button" id="share-create-tab" class="share-tab active" style="padding:8px 16px;border:none;background:#fff;cursor:pointer;border-bottom:2px solid #4a90d9;margin-bottom:-2px;font-size:14px;">创建分享</button>
      <button type="button" id="share-list-tab" class="share-tab" style="padding:8px 16px;border:none;background:#fff;cursor:pointer;border-bottom:2px solid transparent;font-size:14px;color:#666;">管理分享</button>
    </div>
    <div id="share-create-panel">
      <div style="margin-bottom:12px;">
        <label style="display:block;margin-bottom:4px;font-size:13px;color:#555;">文件名</label>
        <input type="text" id="share-filename" placeholder="输入文件名..." style="width:100%;padding:8px;border:1px solid #ccc;border-radius:4px;font-size:14px;box-sizing:border-box;">
      </div>
      <div style="display:flex;gap:8px;margin-bottom:12px;">
        <div style="flex:1;">
          <label style="display:block;margin-bottom:4px;font-size:13px;color:#555;">有效期</label>
          <input type="text" id="share-ttl" value="24h" placeholder="如 1h, 24h, 7d" style="width:100%;padding:8px;border:1px solid #ccc;border-radius:4px;font-size:14px;box-sizing:border-box;">
        </div>
        <div style="flex:1;">
          <label style="display:block;margin-bottom:4px;font-size:13px;color:#555;">最大下载次数</label>
          <input type="number" id="share-max-downloads" value="0" min="0" placeholder="0=不限" style="width:100%;padding:8px;border:1px solid #ccc;border-radius:4px;font-size:14px;box-sizing:border-box;">
        </div>
      </div>
      <div style="margin-bottom:12px;">
        <label><input type="checkbox" id="share-one-time"> 一次性分享（下载一次后自动失效）</label>
      </div>
      <button type="button" id="share-create-btn" class="btn btn-primary" style="width:100%;">创建分享链接</button>
    </div>
    <div id="share-list-panel" style="display:none;">
      <div id="share-list-body" style="max-height:400px;overflow-y:auto;">
        <div class="empty-msg">暂无分享链接</div>
      </div>
      <div style="margin-top:12px;text-align:right;">
        <button type="button" id="share-list-refresh-btn" class="btn btn-secondary" style="margin-right:8px;">刷新</button>
      </div>
    </div>
  </div>
</div>
```

### 步骤 6.2：在 app.js 中新增分享管理逻辑

在 `app.js` 末尾追加分享管理功能：

```javascript
// --- 分享管理 ---
let _shareModalVisible = false;

function showShareModal(filename) {
  _shareModalVisible = true;
  document.getElementById('share-modal').style.display = 'flex';
  if (filename) {
    document.getElementById('share-filename').value = filename;
  } else {
    document.getElementById('share-filename').value = '';
  }
  document.getElementById('share-ttl').value = '24h';
  document.getElementById('share-max-downloads').value = '0';
  document.getElementById('share-one-time').checked = false;
  switchShareTab('create');
  refreshShareList();
}

function hideShareModal() {
  _shareModalVisible = false;
  document.getElementById('share-modal').style.display = 'none';
}

function switchShareTab(tab) {
  document.getElementById('share-create-panel').style.display = tab === 'create' ? 'block' : 'none';
  document.getElementById('share-list-panel').style.display = tab === 'list' ? 'block' : 'none';
  document.querySelectorAll('.share-tab').forEach(function(el) {
    el.style.borderBottomColor = el.id === 'share-' + tab + '-tab' ? '#4a90d9' : 'transparent';
    el.style.color = el.id === 'share-' + tab + '-tab' ? '#333' : '#666';
  });
}

async function createShare() {
  var filename = document.getElementById('share-filename').value.trim();
  if (!filename) { showToast('请输入文件名', 'error'); return; }
  var ttl = document.getElementById('share-ttl').value.trim() || '24h';
  var maxDownloads = Number.parseInt(document.getElementById('share-max-downloads').value) || 0;
  var oneTime = document.getElementById('share-one-time').checked;

  var body = JSON.stringify({
    filename: filename,
    ttl: ttl,
    max_downloads: maxDownloads,
    one_time: oneTime
  });

  try {
    var data;
    if (tunnelHexKey) {
      var result = await tunnelRequest('POST', '/api/share', { 'Content-Type': 'application/json' }, new TextEncoder().encode(body));
      data = JSON.parse(new TextDecoder().decode(result.body));
    } else {
      var resp = await fetch(BASE + '/api/share', {
        method: 'POST', headers: headers({ 'Content-Type': 'application/json' }), body: body
      });
      data = await resp.json();
      if (!resp.ok) { showToast('创建分享失败: ' + (data.message || resp.status), 'error'); return; }
    }
    var shareUrl = location.origin + '/s/' + data.token;
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(shareUrl);
      showToast('分享链接已复制到剪贴板: ' + shareUrl, 'success');
    } else {
      showToast('分享链接: ' + shareUrl, 'success');
    }
    refreshShareList();
  } catch (e) { showToast('创建分享失败: ' + e.message, 'error'); }
}

async function refreshShareList() {
  if (!_shareModalVisible) return;
  var body = document.getElementById('share-list-body');
  try {
    var shares;
    if (tunnelHexKey) {
      var result = await tunnelRequest('GET', '/api/shares', {}, null);
      shares = (JSON.parse(new TextDecoder().decode(result.body))).shares || [];
    } else {
      var resp = await fetch(BASE + '/api/shares', { headers: headers() });
      if (!resp.ok) { body.innerHTML = '<div class="empty-msg">请求失败: ' + resp.status + '</div>'; return; }
      shares = (await resp.json()).shares || [];
    }

    if (shares.length === 0) {
      body.innerHTML = '<div class="empty-msg">暂无分享链接</div>';
      return;
    }

    // 构建表格
    var html = '<table style="width:100%;border-collapse:collapse;font-size:13px;">';
    html += '<thead><tr style="background:#f5f5f5;"><th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">文件名</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">状态</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">下载次数</th>';
    html += '<th style="padding:6px 8px;text-align:left;border-bottom:1px solid #ddd;">剩余时间</th>';
    html += '<th style="padding:6px 8px;text-align:center;border-bottom:1px solid #ddd;">操作</th></tr></thead><tbody>';

    for (var i = 0; i < shares.length; i++) {
      var s = shares[i];
      var statusText = s.expired ? '已过期' : (s.one_time ? '一次性' : '活跃');
      var statusColor = s.expired ? '#999' : (s.one_time ? '#e67e22' : '#27ae60');
      var downloads = s.max_downloads > 0 ? s.downloads + '/' + s.max_downloads : s.downloads + '/∞';
      var expiresLabel = s.expired ? '-' : (s.expires_at ? new Date(s.expires_at).toLocaleString() : '-');

      html += '<tr><td style="padding:6px 8px;border-bottom:1px solid #eee;max-width:200px;overflow:hidden;text-overflow:ellipsis;" title="' + escHtml(s.filename) + '">' + escHtml(s.filename) + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;color:' + statusColor + ';">' + statusText + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;">' + downloads + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;font-size:12px;">' + expiresLabel + '</td>';
      html += '<td style="padding:6px 8px;border-bottom:1px solid #eee;text-align:center;">';
      if (!s.expired) {
        html += '<button class="btn btn-danger btn-sm share-revoke-btn" data-token="' + escHtml(s.token) + '">撤销</button>';
      }
      // 复制链接按钮
      html += '<button class="btn btn-sm btn-secondary share-copy-btn" data-token="' + escHtml(s.token) + '" style="margin-left:4px;">复制</button>';
      html += '</td></tr>';
    }

    html += '</tbody></table>';
    body.innerHTML = html;
  } catch (e) {
    body.innerHTML = '<div class="empty-msg">请求失败: ' + e.message + '</div>';
  }
}

async function revokeShare(token) {
  if (!confirm('确定撤销此分享链接？撤销后链接将立即失效。')) return;
  try {
    if (tunnelHexKey) {
      await tunnelRequest('DELETE', '/api/shares/' + token, {}, null);
    } else {
      var resp = await fetch(BASE + '/api/shares/' + token, { method: 'DELETE', headers: headers() });
      if (!resp.ok) {
        var data = await resp.json().catch(function() { return {}; });
        showToast('撤销失败: ' + (data.message || resp.status), 'error');
        return;
      }
    }
    showToast('分享链接已撤销', 'success');
    refreshShareList();
  } catch (e) { showToast('撤销失败: ' + e.message, 'error'); }
}

function copyShareLink(token) {
  var url = location.origin + '/s/' + token;
  if (navigator.clipboard) {
    navigator.clipboard.writeText(url).then(function() {
      showToast('链接已复制到剪贴板', 'success');
    }).catch(function() {
      showToast('复制失败', 'error');
    });
  } else {
    showToast(url, 'success');
  }
}
```

### 步骤 6.3：修改现有分享按钮事件

将原来 `shareFile()` 的 `prompt` 链替换为调用 `showShareModal(filename)`：

在 `app.js` 的分享按钮事件绑定处，将：

```javascript
// 原来：直接调用 shareFile(name)
// 改为：打开分享管理弹窗
showShareModal(name);
```

同时修改文件分享按钮的事件绑定，在 `DOMContentLoaded` 初始化部分找到 `file-share-btn` 的点击事件，改为：

```javascript
// 当点击文件行的"分享"按钮时：
'click .file-share-btn': function(e) {
  var name = e.target.getAttribute('data-filename');
  showShareModal(name);
}
```

### 步骤 6.4：添加弹窗事件绑定

在 `DOMContentLoaded` 中增加弹窗事件绑定：

```javascript
// 分享弹窗
document.getElementById('share-close-btn').addEventListener('click', hideShareModal);
document.getElementById('share-create-tab').addEventListener('click', function() { switchShareTab('create'); });
document.getElementById('share-list-tab').addEventListener('click', function() { switchShareTab('list'); });
document.getElementById('share-create-btn').addEventListener('click', createShare);
document.getElementById('share-list-refresh-btn').addEventListener('click', refreshShareList);

// 事件委托：分享管理弹窗中的撤销和复制按钮
document.getElementById('share-list-body').addEventListener('click', function(e) {
  if (e.target.classList.contains('share-revoke-btn')) {
    revokeShare(e.target.getAttribute('data-token'));
  } else if (e.target.classList.contains('share-copy-btn')) {
    copyShareLink(e.target.getAttribute('data-token'));
  }
});
```

### 步骤 6.5：验证嵌入编译

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./cmd/sproxy/
```

---

## 任务 7：最终验证

### 步骤 7.1：运行所有受影响包的测试

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -race ./pkg/server/... -run 'TestShare' -v
go test -count=1 -race ./pkg/client/... -run 'TestCreateShare|TestListShares|TestRevokeShare' -v
go test -count=1 -race ./cmd/sclient/... -run 'TestShareCmd' -v
```

### 步骤 7.2：运行 lint 检查

```bash
cd D:\workdir\leon\cocomhub\sproxy
golangci-lint run ./pkg/server/... ./pkg/client/... ./cmd/sclient/...
```

### 步骤 7.3：更新前端文件嵌入

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./cmd/sproxy/
```

### 步骤 7.4：手动 E2E 验证（可选）

```bash
# 启动服务端
cd D:\workdir\leon\cocomhub\sproxy
make run &

# 上传文件
./build/bin/sclient --server http://localhost:18083 upload test.txt

# 创建分享
./build/bin/sclient --server http://localhost:18083 share create test.txt

# 列出分享
./build/bin/sclient --server http://localhost:18083 share list

# 撤销分享
./build/bin/sclient --server http://localhost:18083 share revoke <token>

# 打开浏览器访问 http://localhost:18083/ui/ 验证分享管理弹窗
```