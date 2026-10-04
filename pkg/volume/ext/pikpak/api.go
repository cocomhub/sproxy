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
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

// APIConfig 是官方 REST API 配置。
type APIConfig struct {
	// Host 是 API 域名（默认 https://api-drive.mypikpak.com）。
	Host string
	// AccessToken 是 OAuth access token（可空 = 走 CLI 会话）。
	AccessToken string
	// CredentialPath 是 CLI 会话凭据文件（.credentials.json）路径。空 = 默认
	// ~/.pikpak/.credentials.json。ensureToken 从该文件读 access_token（CLI v0.5.2
	// 的 `auth token` 已变保存命令，不再导出；凭据文件是唯一可靠 token 来源）。
	CredentialPath string
	// HTTPClient 可选注入（测试用）。
	HTTPClient *http.Client
	// Logger 日志。
	Logger *slog.Logger
}

// API 是 PikPak 官方 REST API 客户端（drive/v1 系列）。
// 鉴权：显式 AccessToken（cfg.AccessToken）优先；否则从 CLI 会话凭据文件
// （.credentials.json）读 access_token（CLI 每次操作前自动 refresh 会话）。
type API struct {
	host   string
	client *http.Client
	cli    *Cli
	// cfgToken 是构造时的显式 AccessToken（通常为空 = 走 CLI 会话）；ResetToken 恢复用。
	cfgToken string
	// token 是当前生效 token：初值 = cfgToken；会话凭据路径下惰性读入并缓存。
	token string
	// credPath 是 CLI 会话凭据文件路径（.credentials.json）。
	credPath string
	log      *slog.Logger

	// tokenMu 保护 token 的惰性初始化（多 goroutine 并发首次请求时只取一次）。
	tokenMu sync.Mutex
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
	credPath := cfg.CredentialPath
	if credPath == "" {
		if home, err := os.UserHomeDir(); err == nil {
			credPath = filepath.Join(home, ".pikpak", ".credentials.json")
		}
	}
	return &API{host: strings.TrimRight(host, "/"), client: client, cli: cli, cfgToken: cfg.AccessToken, token: cfg.AccessToken, credPath: credPath, log: log}
}

// ResetToken 清除会话导出的 token（恢复为构造时的显式 token，通常为空）——供账号池
// Use 切换 CLI 会话后调用，使后续 REST 重新从**新会话凭据文件**读 token（否则缓存旧
// 会话 token，转存/定位/删除走错账号——C1 关键）。
func (a *API) ResetToken() {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	a.token = a.cfgToken
}

// FileMeta 是 PikPak 文件元数据（files 列表/搜索返回）。
type FileMeta struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // drive#file / drive#folder
	Size     int64  `json:"-"`
	MimeType string `json:"mime_type"`
	Phase    string `json:"phase"` // PHASE_TYPE_COMPLETE 等
	Hash     string `json:"hash"`
}

// UnmarshalJSON 兼容真实 API 的 size 形态：**string 数字**（"12893054"）或人类可读
// （"12.30 MB"，share get 展示层）；也接受 JSON number。解析失败留 0（不阻断列表，
// pickLargestVideo 仅比较大小，0 不影响正确挑选唯一文件）。复用 sizex.ParseSize
// （仓库已有 ByteSize 解析语义：纯数字/带单位/小数均支持）。
func (m *FileMeta) UnmarshalJSON(data []byte) error {
	type alias FileMeta
	var a struct {
		alias
		SizeRaw json.RawMessage `json:"size"`
	}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = FileMeta(a.alias)
	if len(a.SizeRaw) == 0 {
		return nil
	}
	var n int64
	if err := json.Unmarshal(a.SizeRaw, &n); err == nil {
		m.Size = n
		return nil
	}
	var s string
	if err := json.Unmarshal(a.SizeRaw, &s); err == nil {
		if v, perr := sizex.ParseSize(s); perr == nil {
			m.Size = v
		}
	}
	return nil
}

// fileListResp 是 /drive/v1/files 的响应。
type fileListResp struct {
	Files []FileMeta `json:"files"`
}

// doJSON 执行带鉴权的 API 请求并解析 JSON。
// 鉴权：优先显式 AccessToken（cfg.AccessToken）；未配置时经 CLI `auth token`
// 惰性导出并缓存（CLI 登录态 → REST 的官方通道）。两者皆无 → ErrNotLoggedIn。
func (a *API) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	token, err := a.ensureToken(ctx)
	if err != nil {
		return err
	}
	u := a.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var req *http.Request
	var reqErr error
	if body != nil {
		b, _ := json.Marshal(body)
		req, reqErr = http.NewRequestWithContext(ctx, method, u, strings.NewReader(string(b)))
	} else {
		req, reqErr = http.NewRequestWithContext(ctx, method, u, nil)
	}
	if reqErr != nil {
		return reqErr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 Chrome/117")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
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

// ensureToken 返回当前 access token：显式配置优先；否则从 CLI 会话凭据文件
// （.credentials.json）读 access_token 并缓存。CLI v0.5.2 的 `auth token` 是【保存】
// 命令不再导出——凭据文件是唯一可靠来源（CLI 每次操作前自动 refresh 会话，故文件内
// access_token 恒为最新）。凭据文件缺失/无 access_token → ErrNotLoggedIn（fail-closed）。
// 并发安全：全部读写都在 tokenMu 保护下（含快速路径——锁外读 token 与锁内写之间
// 无 happens-before，多 goroutine 并发首个云下载会真实竞争）。
func (a *API) ensureToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	if a.token != "" {
		return a.token, nil
	}
	if a.credPath == "" {
		return "", fmt.Errorf("%w: 凭据文件路径不可用", ErrNotLoggedIn)
	}
	data, err := os.ReadFile(a.credPath)
	if err != nil {
		return "", fmt.Errorf("%w: 读凭据文件: %v", ErrNotLoggedIn, err)
	}
	var cred struct {
		AccessToken string `json:"access_token"`
	}
	if jerr := json.Unmarshal(data, &cred); jerr != nil {
		return "", fmt.Errorf("%w: 凭据文件解析失败: %v", ErrNotLoggedIn, jerr)
	}
	if cred.AccessToken == "" {
		return "", fmt.Errorf("%w: 凭据文件缺 access_token", ErrNotLoggedIn)
	}
	a.token = cred.AccessToken
	return a.token, nil
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
		// 自己分享的文件（file_restore_own，错误码 9）：已在个人网盘，无需转存。
		// 返回**源文件 ID**（fileIDs[0]）供调用方直接定位下载（locateRestoredFile
		// FindByID 命中自己网盘文件）。这是真实 API 行为——测试 fake server 未建模
		// 导致此前单测全绿（测试掩盖真实行为）。
		if strings.Contains(err.Error(), "file_restore_own") && len(fileIDs) > 0 {
			return fileIDs[0], nil
		}
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
	if err := a.walkShareFolder(ctx, shareID, "", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walkShareFolder 递归展开分享目录树：分页拉取 + 子目录递归，结果追加到 out。
func (a *API) walkShareFolder(ctx context.Context, shareID, pid string, out *[]FileMeta) error {
	pageToken := ""
	for {
		files, next, err := a.ShareDetail(ctx, shareID, pid, pageToken)
		if err != nil {
			return err
		}
		for i := range files {
			*out = append(*out, files[i])
			if files[i].Kind == "drive#folder" {
				if err := a.walkShareFolder(ctx, shareID, files[i].ID, out); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		pageToken = next
	}
}

// FindInDrive 在网盘（递归）里找匹配名字/大小的文件，返回 FileMeta。
// 匹配优先级：size 精确匹配 > name 子串匹配（大小优先，降低同子串不同文件误命中）。
func (a *API) FindInDrive(ctx context.Context, wantName string, wantSize int64) (*FileMeta, error) {
	all, err := a.ListRecursive(ctx, "")
	if err != nil {
		return nil, err
	}
	// size 精确匹配（强判据）优先；无命中再走 name 子串（弱判据）。
	if f := findFileBySize(all, wantSize); f != nil {
		return f, nil
	}
	if f := findFileByName(all, wantName); f != nil {
		return f, nil
	}
	return nil, ErrFileNotFound
}

// findFileBySize 在文件列表中按 size 精确匹配（0 表示不参与匹配）。
func findFileBySize(all []FileMeta, wantSize int64) *FileMeta {
	if wantSize <= 0 {
		return nil
	}
	for i := range all {
		f := &all[i]
		if f.Kind == "drive#file" && f.Size == wantSize {
			return f
		}
	}
	return nil
}

// findFileByName 在文件列表中按 name 子串匹配（空表示不参与匹配）。
func findFileByName(all []FileMeta, wantName string) *FileMeta {
	if wantName == "" {
		return nil
	}
	for i := range all {
		f := &all[i]
		if f.Kind == "drive#file" && strings.Contains(f.Name, wantName) {
			return f
		}
	}
	return nil
}

// FindByID 在网盘（递归）里按文件 ID 精确定位（restore 返回的 fileID 首选路径）。
func (a *API) FindByID(ctx context.Context, fileID string) (*FileMeta, error) {
	if fileID == "" {
		return nil, fmt.Errorf("%w: empty file id", ErrFileNotFound)
	}
	all, err := a.ListRecursive(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].ID == fileID {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("%w: file id %s", ErrFileNotFound, fileID)
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

// parseShareID 从 mypikpak.com/s/<share_id>（或 mypikpak.net / keepshare）URL 提取 share id。
// 校验 Host 属于支持域名（与 Supports 同一判据，避免伪冒域名 /s/<id> 被误转发到官方 REST）。
func parseShareID(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	if !supportedShareHost(host) {
		return "", fmt.Errorf("%w: host %s", ErrUnsupported, u.Host)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "s" {
		return "", fmt.Errorf("%w: %s", ErrUnsupported, raw)
	}
	return parts[1], nil
}

// supportedShareHost 判断 host 是否属于支持的分享域名。
func supportedShareHost(host string) bool {
	switch host {
	case "mypikpak.com", "www.mypikpak.com",
		"mypikpak.net", "www.mypikpak.net",
		"keepshare.org", "www.keepshare.org":
		return true
	}
	return false
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
