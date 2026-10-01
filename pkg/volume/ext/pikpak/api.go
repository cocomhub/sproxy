// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// APIConfig 是官方 REST API 配置。
type APIConfig struct {
	// Host 是 API 域名（默认 https://api-drive.mypikpak.com）。
	Host string
	// AccessToken 是 OAuth access token（可空 = 走 CLI 会话）。
	AccessToken string
	// HTTPClient 可选注入（测试用）。
	HTTPClient *http.Client
	// Logger 日志。
	Logger *slog.Logger
}

// API 是 PikPak 官方 REST API 客户端（drive/v1 系列）。
// 通过 CLI 会话（auth status 登录态）或显式 AccessToken 鉴权。
type API struct {
	host   string
	client *http.Client
	cli    *Cli
	token  string
	log    *slog.Logger
}

// NewAPI 创建 API 客户端。
// cli 非空时优先走 CLI 会话鉴权（推荐：OAuth 无密码）；否则用 AccessToken。
func NewAPI(cfg APIConfig, cli *Cli) *API {
	host := cfg.Host
	if host == "" {
		host = "https://api-drive.mypikpak.com"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: netutil.IsolatedTransport()}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &API{host: strings.TrimRight(host, "/"), client: client, cli: cli, token: cfg.AccessToken, log: log}
}

// FileMeta 是 PikPak 文件元数据（files 列表/搜索返回）。
type FileMeta struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // drive#file / drive#folder
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type"`
	Phase    string `json:"phase"` // PHASE_TYPE_COMPLETE 等
	Hash     string `json:"hash"`
}

// fileListResp 是 /drive/v1/files 的响应。
type fileListResp struct {
	Files []FileMeta `json:"files"`
}

// doJSON 执行带鉴权的 API 请求并解析 JSON。
// 鉴权：优先 CLI（通过 pikpak 命令拿 token 太绕，直接用 REST 需 access_token；
// CLI 的会话由 CLI 内部管理，REST 调用用 CLI 不可行——除非 CLI 提供 token 导出）。
// 简化：API 用显式 AccessToken（由 CLI 登录态导出或配置注入）；cli 仅用于安装/登录。
func (a *API) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u := a.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var req *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		req, err = http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(b)))
	} else {
		req, err = http.NewRequestWithContext(ctx, method, u, nil)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 Chrome/117")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("X-Client-ID", "YUMx5nI8ZU8Ap8pm")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := ioReadAll(resp.Body)
		return fmt.Errorf("pikpak api %s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(b), 200))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("pikpak api decode: %w", err)
		}
	}
	return nil
}

// List 列出目录（parentID 空 = 根）下文件。
func (a *API) List(ctx context.Context, parentID string) ([]FileMeta, error) {
	q := url.Values{}
	q.Set("parent_id", parentID)
	q.Set("page_size", "500")
	var resp fileListResp
	if err := a.doJSON(ctx, http.MethodGet, "/drive/v1/files", q, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

// ListRecursive 递归列出（用于找分享转存后的文件）。
func (a *API) ListRecursive(ctx context.Context, parentID string) ([]FileMeta, error) {
	var out []FileMeta
	var walk func(pid string) error
	walk = func(pid string) error {
		files, err := a.List(ctx, pid)
		if err != nil {
			return err
		}
		for i := range files {
			out = append(out, files[i])
			if files[i].Kind == "drive#folder" {
				if err := walk(files[i].ID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(parentID); err != nil {
		return nil, err
	}
	return out, nil
}

// RestoreShare 转存分享文件到个人网盘根目录（或指定 parentID）。
// 返回转存任务/文件 ID（RESTORE_START 时文件异步进入网盘）。
func (a *API) RestoreShare(ctx context.Context, shareID string, fileIDs []string, parentID string) (string, error) {
	body := map[string]any{
		"share_id":    shareID,
		"pass_code":   "",
		"parent_id":   parentID,
		"file_ids":    fileIDs,
		"folder_type": "",
	}
	var resp struct {
		RestoreStatus string `json:"restore_status"`
		FileID        string `json:"file_id"`
	}
	if err := a.doJSON(ctx, http.MethodPost, "/drive/v1/share/restore", nil, body, &resp); err != nil {
		return "", err
	}
	return resp.FileID, nil
}

// ShareDetail 列出分享内容（share/detail 分页）。
func (a *API) ShareDetail(ctx context.Context, shareID, parentID string, pageToken string) ([]FileMeta, string, error) {
	q := url.Values{}
	q.Set("share_id", shareID)
	q.Set("parent_id", parentID)
	q.Set("limit", "200")
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	var resp struct {
		Files         []FileMeta `json:"files"`
		NextPageToken string     `json:"next_page_token"`
	}
	if err := a.doJSON(ctx, http.MethodGet, "/drive/v1/share/detail", q, nil, &resp); err != nil {
		return nil, "", err
	}
	return resp.Files, resp.NextPageToken, nil
}

// ListShareRecursive 递归列出分享内容（找目标 mp4）。
func (a *API) ListShareRecursive(ctx context.Context, shareID string) ([]FileMeta, error) {
	var out []FileMeta
	var walk func(pid string) error
	walk = func(pid string) error {
		pageToken := ""
		for {
			files, next, err := a.ShareDetail(ctx, shareID, pid, pageToken)
			if err != nil {
				return err
			}
			for i := range files {
				out = append(out, files[i])
				if files[i].Kind == "drive#folder" {
					if err := walk(files[i].ID); err != nil {
						return err
					}
				}
			}
			if next == "" {
				break
			}
			pageToken = next
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return out, nil
}

// FindInDrive 在网盘（递归）里找匹配名字/大小的文件，返回 FileMeta。
func (a *API) FindInDrive(ctx context.Context, wantName string, wantSize int64) (*FileMeta, error) {
	all, err := a.ListRecursive(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range all {
		f := &all[i]
		if f.Kind != "drive#file" {
			continue
		}
		if wantSize > 0 && f.Size == wantSize {
			return f, nil
		}
		if wantName != "" && strings.Contains(f.Name, wantName) {
			return f, nil
		}
	}
	return nil, ErrFileNotFound
}

// Delete 删除网盘文件/文件夹（移到回收站）。
func (a *API) Delete(ctx context.Context, fileIDs []string) error {
	body := map[string]any{"ids": fileIDs}
	var out map[string]any
	if err := a.doJSON(ctx, http.MethodPost, "/drive/v1/files:batchTrash", nil, body, &out); err != nil {
		return err
	}
	return nil
}

// DownloadLink 是个人网盘文件的官方下载链（经 /drive/v1/files/{id}?usage=FETCH）。
func (a *API) DownloadLink(ctx context.Context, fileID string) (string, error) {
	var resp struct {
		WebContentLink string `json:"web_content_link"`
	}
	q := url.Values{}
	q.Set("usage", "FETCH")
	if err := a.doJSON(ctx, http.MethodGet, "/drive/v1/files/"+fileID, q, nil, &resp); err != nil {
		return "", err
	}
	if resp.WebContentLink == "" {
		return "", fmt.Errorf("%w: no download link for %s", ErrFileNotFound, fileID)
	}
	return resp.WebContentLink, nil
}

// parseShareID 从 mypikpak.com/s/<share_id> URL 提取 share id。
func parseShareID(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "s" {
		return "", fmt.Errorf("%w: %s", ErrUnsupported, raw)
	}
	return parts[1], nil
}

// ioReadAll 读全部（薄封装）。
func ioReadAll(r io.Reader) ([]byte, error) {
	var b strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err == io.EOF {
			return []byte(b.String()), nil
		}
		if err != nil {
			return nil, err
		}
	}
}
