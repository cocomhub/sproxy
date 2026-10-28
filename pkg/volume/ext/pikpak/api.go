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
	// UserHost 是 OAuth token 端点域名（默认 https://user.mypikpak.com）——access_token
	// 过期时用 refresh_token 续期；测试可注入本地 httptest。
	UserHost string
	// RefreshSkew 是 access_token 临期续期提前量；<=0 → 默认 5min。
	RefreshSkew time.Duration
	// MaxRetries 是幂等请求（GET/HEAD）的瞬断重试次数；<=0 → 默认（3）。
	// 背景：取 FETCH 直链的控制面请求单次 TLS 握手超时会让整个多 GB 任务失败。
	MaxRetries int
	// RetryBackoff 是重试退避基数（指数增长 + 抖动）；<=0 → 默认（400ms）。
	RetryBackoff time.Duration
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
	// userHost 是 OAuth token 端点域名（续期用）。
	userHost string
	// refreshSkew 是临期续期提前量。
	refreshSkew time.Duration
	// expiry 是当前 token 的过期时间（零值 = 未知，不主动续期）。
	expiry time.Time
	// lastRefresh 是上次续期时间（节流：避免并发 401 触发续期风暴）。
	lastRefresh time.Time
	log         *slog.Logger
	// retry 是幂等请求（GET/HEAD）的退避重试策略。
	retry retryPolicy

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
	userHost := strings.TrimRight(cfg.UserHost, "/")
	if userHost == "" {
		userHost = defaultUserHost
	}
	skew := cfg.RefreshSkew
	if skew <= 0 {
		skew = defaultRefreshSkew
	}
	return &API{host: strings.TrimRight(host, "/"), client: client, cli: cli, cfgToken: cfg.AccessToken, token: cfg.AccessToken,
		credPath: credPath, userHost: userHost, refreshSkew: skew, log: log,
		retry: retryPolicy{MaxRetries: cfg.MaxRetries, Backoff: cfg.RetryBackoff}}
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
	// 源文件时间（官方 REST 原样字段，RFC3339 带时区偏移——如 "2026-10-28T03:11:17.777+08:00"）。
	// 用户裁定 2026-10-28：转存产物的 mtime/ctime 必须来自源文件，不得用下载完成时刻顶替。
	CreatedTime      string `json:"created_time"`
	ModifiedTime     string `json:"modified_time"`
	UserModifiedTime string `json:"user_modified_time"`
}

// SourceTimes 解析源文件时间（created/modified，UTC）。缺失/格式非法 → 零值（未知）。
// 优先 modified_time（无则 user_modified_time），created 用 created_time。
func (m FileMeta) SourceTimes() (created, modified time.Time) {
	created = parseAPITime(m.CreatedTime)
	modified = parseAPITime(m.ModifiedTime)
	if modified.IsZero() {
		modified = parseAPITime(m.UserModifiedTime)
	}
	return created, modified
}

// parseAPITime 解析 PikPak REST 时间（RFC3339 带偏移，可能带毫秒）；失败返回零值。
func parseAPITime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
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
	Files         []FileMeta `json:"files"`
	NextPageToken string     `json:"next_page_token"`
}

// pikpakMaxListPages 是 List 分页安全上限（防服务端 page_token 回环导致死循环）。
const pikpakMaxListPages = 1000

// pikpakMaxWalkDepth 是分享目录递归深度上限（防服务端目录自环导致栈溢出/死循环）。
const pikpakMaxWalkDepth = 64

// doJSON 执行带鉴权的 API 请求并解析 JSON。
// 鉴权：优先显式 AccessToken（cfg.AccessToken）；未配置时经 CLI `auth token`
// 惰性导出并缓存（CLI 登录态 → REST 的官方通道）。两者皆无 → ErrNotLoggedIn。
//
// **幂等重试**：GET/HEAD 遇传输层瞬断（TLS 握手超时/连接重置/EOF）或 429/5xx
// 退避重试（默认 3 次）；POST 等非幂等请求单次（避免重放副作用）。
// 背景（2026-10-11 实测）：DownloadLink 单次握手超时曾让 1.3GB 混合下载整任务失败。
func (a *API) doJSON(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	u := a.host + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	if method != http.MethodGet && method != http.MethodHead {
		_, err := a.doJSONOnce(ctx, method, path, u, body, out)
		return err
	}
	return doWithRetry(ctx, a.retry, a.log, method+" "+path, func() (bool, error) {
		return a.doJSONOnce(ctx, method, path, u, body, out)
	})
}

// doJSONOnce 单次请求（返回是否可重试）。
func (a *API) doJSONOnce(ctx context.Context, method, path, u string, body any, out any) (bool, error) {
	token, err := a.ensureToken(ctx)
	if err != nil {
		return false, err
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
		return false, reqErr
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "Mozilla/5.0 Chrome/117")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Client-ID", "YUMx5nI8ZU8Ap8pm")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return retryableRequestErr(err), err
	}
	defer resp.Body.Close()
	// 401：token 已失效（本地 token_expiry 未到也可能发生，如服务端提前失效/改密）→
	// 续期一次（HTTP → CLI）并交由外层退避重试；不把 401 当普通 4xx 直接放弃。
	if resp.StatusCode == http.StatusUnauthorized {
		b, _ := ioReadAll(resp.Body)
		a.invalidateToken(ctx)
		return true, fmt.Errorf("pikpak api %s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(b), 200))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := ioReadAll(resp.Body)
		return retryableStatus(resp.StatusCode),
			fmt.Errorf("pikpak api %s %s: HTTP %d: %s", method, path, resp.StatusCode, truncate(string(b), 200))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return false, fmt.Errorf("pikpak api decode: %w", err)
		}
	}
	return false, nil
}

// ensureToken 返回**当前有效**的 access token：显式配置优先；否则读 CLI 会话凭据文件
// （.credentials.json），token 过期/临期时用 refresh_token 自动续期（HTTP 优先 → 官方 CLI
// 回落）并落盘。凭据文件缺失/无 access_token → ErrNotLoggedIn（fail-closed）。
// 并发安全：全部读写都在 tokenMu 保护下（含快速路径——锁外读 token 与锁内写之间
// 无 happens-before，多 goroutine 并发首个云下载会真实竞争）。
func (a *API) ensureToken(ctx context.Context) (string, error) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	// 显式 token（cfg.AccessToken）：无凭据文件可续期，按原样使用。
	if a.cfgToken != "" {
		a.token = a.cfgToken
		return a.token, nil
	}
	if a.token != "" && !a.needsRefreshLocked() {
		return a.token, nil
	}
	if a.credPath == "" {
		return "", fmt.Errorf("%w: 凭据文件路径不可用", ErrNotLoggedIn)
	}
	cred, err := readCredentials(a.credPath)
	if err != nil {
		if a.token != "" {
			return a.token, nil // 已有可用 token：凭据文件暂不可读不阻断
		}
		return "", fmt.Errorf("%w: 读凭据文件 %s: %v", ErrNotLoggedIn, a.credPath, err)
	}
	if cred.accessToken() == "" {
		return "", fmt.Errorf("%w: 凭据文件缺 access_token", ErrNotLoggedIn)
	}
	// 文件比内存新（其它进程/CLI 已续期）→ 直接采用文件里的 token。
	if cred.accessToken() != a.token {
		a.token, a.expiry = cred.accessToken(), cred.expiry()
	}
	if !a.needsRefreshLocked() {
		return a.token, nil
	}
	if rerr := a.refreshLocked(ctx, cred); rerr != nil {
		if a.expiredLocked() {
			return "", rerr // 确已过期且续期失败：如实报错（带续期/登录提示）
		}
		a.log.Warn("pikpak token refresh failed, continue with current token", "err", rerr)
	}
	return a.token, nil
}

// needsRefreshLocked 判断是否需续期：无 token 或（已知过期时间且临期）。
// 过期时间未知（老凭据/显式 token）→ 不主动续期（保持既有行为，失败由 401 路径兜底）。
func (a *API) needsRefreshLocked() bool {
	if a.token == "" {
		return true
	}
	if a.expiry.IsZero() {
		return false
	}
	return time.Now().Add(a.refreshSkew).After(a.expiry)
}

// expiredLocked 判断当前 token 是否已过期（过期时间未知 → false）。
func (a *API) expiredLocked() bool {
	return !a.expiry.IsZero() && time.Now().After(a.expiry)
}

// invalidateToken 处理「服务端判 401（token 已失效）」：清缓存 + 尽力续期一次，
// 让外层重试用续期后的 token。续期失败只记日志（不把 401 掩盖成续期错误）。
func (a *API) invalidateToken(ctx context.Context) {
	a.tokenMu.Lock()
	defer a.tokenMu.Unlock()
	a.token = a.cfgToken
	a.expiry = time.Time{}
	if a.cfgToken != "" || a.credPath == "" {
		return
	}
	cred, err := readCredentials(a.credPath)
	if err != nil {
		a.log.Warn("pikpak 401: 读凭据文件失败", "err", err)
		return
	}
	if rerr := a.refreshLocked(ctx, cred); rerr != nil {
		a.log.Warn("pikpak 401: token 续期失败", "err", rerr)
	}
}

// List 列出目录（parentID 空 = 根）下文件。**带 page_token 循环取全量**（EXT-6：
// 此前只取第一页 500 条，>500 条目目录的后续文件对 Stat/Get/List 变成「不存在」）。
func (a *API) List(ctx context.Context, parentID string) ([]FileMeta, error) {
	var out []FileMeta
	pageToken := ""
	for range pikpakMaxListPages {
		q := url.Values{}
		q.Set("parent_id", parentID)
		q.Set("page_size", "500")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		var resp fileListResp
		if err := a.doJSON(ctx, http.MethodGet, "/drive/v1/files", q, nil, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Files...)
		if resp.NextPageToken == "" {
			return out, nil
		}
		pageToken = resp.NextPageToken
	}
	return out, nil
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
// 返回 (转存任务/文件 ID, owned)——owned=true 表示该文件是**用户自己网盘已有源文件**
// （file_restore_own，错误码 9），非本次 restore 的副本（NH-P1：AutoDelete 不得删源文件）。

// isStorageFullError 判断错误是否为账号空间不足（drive 响应含 storage/quota/space 满）。
func isStorageFullError(err error) bool {
	if err == nil {
		return false
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "storage") && (strings.Contains(low, "full") || strings.Contains(low, "exceed") || strings.Contains(low, "space")) ||
		strings.Contains(low, "quota exceeded") || strings.Contains(low, "no space") ||
		strings.Contains(low, "存储空间")
}
func (a *API) RestoreShare(ctx context.Context, shareID string, fileIDs []string, parentID string) (string, bool, error) {
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
		// 返回**源文件 ID**（fileIDs[0]）+ owned=true——调用方直接定位下载且
		// AutoDelete 跳过（源文件非 restore 副本，NH-P1 防数据丢失）。
		if strings.Contains(err.Error(), "file_restore_own") && len(fileIDs) > 0 {
			return fileIDs[0], true, nil
		}
		// 空间不足（drive 返回 storage/quota/space 满错误）：立即映射为 ErrStorageFull
		// 哨兵——调用方直接失败，不再重试/降级拖沓（用户明示：保存账号空间失败立刻报错）。
		if isStorageFullError(err) {
			return "", false, fmt.Errorf("%w: %v", ErrStorageFull, err)
		}
		return "", false, err
	}
	return resp.FileID, false, nil
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
	return a.walkShareFolderDepth(ctx, shareID, pid, out, 0)
}

// walkShareFolderDepth 带**深度上限**的递归（P2：原无限递归在服务端目录自环时会栈溢出/
// 死循环占死任务；List 已有 pikpakMaxListPages 但 walk 无）。
func (a *API) walkShareFolderDepth(ctx context.Context, shareID, pid string, out *[]FileMeta, depth int) error {
	if depth > pikpakMaxWalkDepth {
		return fmt.Errorf("pikpak: 分享目录递归超过上限 %d（疑似自环）", pikpakMaxWalkDepth)
	}
	pageToken := ""
	for range pikpakMaxListPages {
		files, next, err := a.ShareDetail(ctx, shareID, pid, pageToken)
		if err != nil {
			return err
		}
		if aerr := a.appendShareFiles(ctx, shareID, files, out, depth); aerr != nil {
			return aerr
		}
		if next == "" {
			return nil
		}
		pageToken = next
	}
	return fmt.Errorf("pikpak: 分享目录分页超过上限 %d（疑似 page_token 回环）", pikpakMaxListPages)
}

// appendShareFiles 追加一页条目并对子目录递归（抽出以降低 walkShareFolderDepth 复杂度）。
func (a *API) appendShareFiles(ctx context.Context, shareID string, files []FileMeta, out *[]FileMeta, depth int) error {
	for i := range files {
		*out = append(*out, files[i])
		if files[i].Kind != "drive#folder" {
			continue
		}
		if err := a.walkShareFolderDepth(ctx, shareID, files[i].ID, out, depth+1); err != nil {
			return err
		}
	}
	return nil
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
	// 直接用 GET /drive/v1/files/{id}（轻量单查）——不再 ListRecursive 全盘遍历
	// （全盘 walk 在网盘大/空间满时慢/超时，是去重/定位失效根因之一）。
	var out struct {
		FileMeta
	}
	if err := a.doJSON(ctx, http.MethodGet, "/drive/v1/files/"+fileID, nil, nil, &out); err != nil {
		return nil, fmt.Errorf("%w: file id %s: %v", ErrFileNotFound, fileID, err)
	}
	if out.ID == "" {
		return nil, fmt.Errorf("%w: file id %s", ErrFileNotFound, fileID)
	}
	return &out.FileMeta, nil
}

// Delete 删除网盘文件/文件夹（移到回收站 batchTrash）。
// 注意：batchTrash 只移回收站，**不释放配额空间**（quota.usage 不降）——
// 免费账号 6GB 空间场景应优先用 DeletePermanent（永久删）释放空间。
func (a *API) Delete(ctx context.Context, fileIDs []string) error {
	body := map[string]any{"ids": fileIDs}
	var out map[string]any
	if err := a.doJSON(ctx, http.MethodPost, "/drive/v1/files:batchTrash", nil, body, &out); err != nil {
		return err
	}
	return nil
}

// DeletePermanent 永久删除网盘文件/文件夹（/drive/v1/files:batchDelete）。
// 与 Delete（batchTrash 移回收站）不同：batchDelete 直接释放配额空间
// （实测 quota.usage 归 0）——免费账号空间仅 6GB，转存后必须及时永久删。
// 注意：删除是**异步任务**（返回 task_id），PikPak 端清理需要数秒；
// 调用方可在需要时轮询 quota 确认释放。
func (a *API) DeletePermanent(ctx context.Context, fileIDs []string) error {
	body := map[string]any{"ids": fileIDs}
	var out struct {
		TaskID string `json:"task_id"`
	}
	if err := a.doJSON(ctx, http.MethodPost, "/drive/v1/files:batchDelete", nil, body, &out); err != nil {
		return err
	}
	a.log.Debug("pikpak permanent delete", "task_id", out.TaskID, "files", len(fileIDs))
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
// parseShareIDWithToken 解析分享 URL 并提取 share id 与子路径 token（/s/<id>/<token>）。
// 返回 (shareID, token)；无 token 段时 token 为空。
// 支持：mypikpak.com/s/<id>/[token]、keepshare.org/<id>/magnet:...（镜像）。
func parseShareIDWithToken(raw string) (string, string) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", ""
	}
	host := strings.ToLower(u.Hostname())
	if !supportedShareHost(host) {
		return "", ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	// mypikpak.com/s/<id>/<token>：parts[0]=s, parts[1]=id, parts[2]=token（可选）
	if parts[0] == "s" {
		if len(parts) < 2 || parts[1] == "" {
			return "", ""
		}
		tok := ""
		if len(parts) > 2 {
			tok = parts[2]
		}
		return parts[1], tok
	}
	// keepshare.org/<id>/magnet:...（镜像 301 → mypikpak）：parts[0] 直接是分享 id
	if strings.Contains(host, "keepshare") {
		if parts[0] == "" {
			return "", ""
		}
		return parts[0], ""
	}
	return "", ""
}

// parseShareID 从分享 URL 提取 share id（忽略子路径 token，维持旧调用语义）。
func parseShareID(raw string) (string, error) {
	id, _ := parseShareIDWithToken(raw)
	if id == "" {
		return "", fmt.Errorf("%w: %s", ErrUnsupported, raw)
	}
	return id, nil
}

// supportedShareHost 判断 host 是否属于支持的分享域名。
func supportedShareHost(host string) bool {
	switch host {
	case "mypikpak.com", "www.mypikpak.com",
		"mypikpak.net", "www.mypikpak.net",
		"keepshare.org", "www.keepshare.org",
		"keepshare.cc", "www.keepshare.cc": // 实测 keepshare 301 → keepshare.cc
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
