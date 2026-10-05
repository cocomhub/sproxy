// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"crypto/md5" // #nosec G501 -- PikPak 分享签名协议强制 MD5（与 gopeed 扩展算法一致，非安全用途）
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
)

// 匿名分享解析常量（与 gopeed 扩展 TeamBreakerr@gopeed-extension-pikpak 的
// WEB_CLIENT_ID / WEB_CLIENT_VERSION / WEB_PACKAGE_NAME / WEB_ALGORITHMS 完全一致，
// 保证同一分享 URL 产出的 captcha 签名/直链与扩展等价）。
const (
	webClientID      = "YUMx5nI8ZU8Ap8pm"
	webClientVersion = "2.0.0"
	webPackageName   = "mypikpak.com"
)

// webAlgorithms 是 captcha 签名链（15 个盐，逐轮 MD5 拼接）。
// 来源：gopeed 扩展 index.js WEB_ALGORITHMS（原样保留，勿改——算法变更会失效）。
var webAlgorithms = []string{
	"C9qPpZLN8ucRTaTiUMWYS9cQvWOE",
	"+r6CQVxjzJV6LCV",
	"F",
	"pFJRC",
	"9WXYIDGrwTCz2OiVlgZa90qpECPD6olt",
	"/750aCr4lm/Sly/c",
	"RB+DT/gZCrbV",
	"",
	"CyLsf7hdkIRxRm215hl",
	"7xHvLi2tOYP0Y92b",
	"ZGTXXxu8E/MIWaEDB+Sm/",
	"1UI3",
	"E7fP5Pfijd+7K+t6Tg/NhuLq0eEUVChpJSkrKxpO",
	"ihtqpG6FMt65+Xk+tWUH2",
	"NhXXU9rg4XXdzo7u5o",
}

// defaultShareUA 是匿名请求 UA（与扩展 DEFAULT_UA 一致）。
const defaultShareUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/117.0.0.0 Safari/537.36"

// captchaSign 计算 captcha 签名：WEB_CLIENT_ID+VERSION+PACKAGE+deviceId+ts 逐轮 MD5。
// 与扩展 captchaSign 语义一致（"1." + 32hex）。
func captchaSign(deviceID, ts string) string {
	s := webClientID + webClientVersion + webPackageName + deviceID + ts
	for _, algo := range webAlgorithms {
		//nolint:gosec // G401 PikPak 分享签名协议强制 MD5（与 gopeed 扩展一致，非安全用途）
		h := md5.Sum([]byte(s + algo)) // NOSONAR: S4790 — PikPak 分享签名协议强制 MD5（与 gopeed 扩展一致）
		s = hex.EncodeToString(h[:])
	}
	return "1." + s
}

// genDeviceID 生成 32 位小写 hex 设备 ID（同扩展 genDeviceId）。
func genDeviceID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 极低概率：fallback 到时间戳哈希（仅影响签名随机性，不阻断）。
		//nolint:gosec // G401 协议签名 fallback（非安全用途，仅影响签名随机性）
		h := md5.Sum([]byte(time.Now().String())) // NOSONAR: S4790 — 签名 fallback（非安全用途）
		return hex.EncodeToString(h[:])
	}
	return hex.EncodeToString(buf)
}

// ShareResolverConfig 是匿名分享解析配置。
type ShareResolverConfig struct {
	// APIHost 是 drive/v1 API 域名（默认 https://api-drive.mypikpak.net）。
	APIHost string
	// UserHost 是 captcha 域名（默认 https://user.mypikpak.net）。
	UserHost string
	// HTTPClient 可选注入（测试用）。
	HTTPClient *http.Client
	// Logger 日志。
	Logger *slog.Logger
}

// ShareResolver 是纯 Go 匿名分享解析器：
// captcha/init（签名）→ share 列表 → file_info（拿匿名直链 web_content_link/medias）。
// 无需登录账号；与 gopeed pikpak 扩展同一签名算法（纯 Go 移植）。
type ShareResolver struct {
	apiHost  string
	userHost string
	client   *http.Client
	log      *slog.Logger

	// mu 保护 deviceID 与 captchaToken（并发 Resolve 共享同一匿名身份）。
	mu           sync.Mutex
	deviceID     string
	captchaToken string
	// captchaFetchedAt / captchaTTL：token 年龄追踪（round-8 Minor——长下载中途 re-resolve
	// 用陈旧 token 会不必要地降级；超过 2/3 TTL 主动刷新）。TTL 来自 captcha/init 的
	// expires_in（秒），缺省保守 1h。
	captchaFetchedAt time.Time
	captchaTTL       time.Duration
}

// NewShareResolver 创建匿名分享解析器。
func NewShareResolver(cfg ShareResolverConfig) *ShareResolver {
	apiHost := cfg.APIHost
	if apiHost == "" {
		apiHost = "https://api-drive.mypikpak.net"
	}
	userHost := cfg.UserHost
	if userHost == "" {
		userHost = "https://user.mypikpak.net"
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: netutil.IsolatedTransport()}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &ShareResolver{
		apiHost:  strings.TrimRight(apiHost, "/"),
		userHost: strings.TrimRight(userHost, "/"),
		client:   client,
		log:      log,
		deviceID: genDeviceID(),
	}
}

// ShareFile 是分享里的一个文件（匿名解析结果，含直链）。
type ShareFile struct {
	ID         string
	Name       string
	Kind       string // drive#file / drive#folder（目标选择用：筛视频文件）
	Size       int64
	Hash       string // 文件内容哈希（幂等校验用：转存命中时比对）
	DirectLink string // 匿名分享直链（web_content_link 或 medias[0].link.url）
}

// ShareMeta 是分享解析结果。
type ShareMeta struct {
	ShareID string
	Files   []ShareFile
}

// Resolve 解析分享 URL：captcha init → share 列表 → 每个文件 file_info 拿直链。
// 返回文件列表（含匿名直链）与 shareID。
func (r *ShareResolver) Resolve(ctx context.Context, shareURL string) (*ShareMeta, error) {
	shareID, shareToken := parseShareIDWithToken(shareURL)
	if shareID == "" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupported, shareURL)
	}
	r.mu.Lock()
	if r.captchaToken == "" || time.Since(r.captchaFetchedAt) > r.captchaTTL*2/3 {
		if capErr := r.refreshCaptchaLocked(ctx); capErr != nil {
			r.mu.Unlock()
			return nil, capErr
		}
	}
	tok := r.captchaToken
	dev := r.deviceID
	r.mu.Unlock()

	// 1. 分享详情（列文件，递归子目录文件夹）
	files, err := r.listShareRecursive(ctx, shareID, "", shareToken, tok, dev)
	if err != nil {
		return nil, err
	}
	// 2. 每个文件拿直链
	out := make([]ShareFile, 0, len(files))
	for _, f := range files {
		link, hash, ferr := r.fileInfo(ctx, shareID, f.ID, tok, dev)
		if ferr != nil {
			r.log.Warn("share file_info failed", "file", f.ID, "err", ferr)
			continue
		}
		out = append(out, ShareFile{ID: f.ID, Name: f.Name, Kind: f.Kind, Size: int64(f.Size), Hash: hash, DirectLink: link})
	}
	return &ShareMeta{ShareID: shareID, Files: out}, nil
}

// listShareRecursive 递归列分享文件（根 + 所有子目录；folder 记录但无直链）。
// 沿 folder 递归，避免 API 忽略 parent_id 时死循环（seen 去重）。
func (r *ShareResolver) listShareRecursive(ctx context.Context, shareID, parentID, shareToken, captchaTok, dev string) ([]struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	Size sizex.ByteSize `json:"size"`
}, error) {
	return r.listShareRecursiveSeen(ctx, shareID, parentID, shareToken, captchaTok, dev, make(map[string]bool))
}

// listShareRecursiveSeen 递归列分享文件；seen 去重已访问 folder（API 可能忽略 parent_id
// 返回重复结构 → 无去重会死循环）。
func (r *ShareResolver) listShareRecursiveSeen(ctx context.Context, shareID, parentID, shareToken, captchaTok, dev string, seen map[string]bool) ([]struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	Size sizex.ByteSize `json:"size"`
}, error) {
	files, err := r.shareDetail(ctx, shareID, parentID, shareToken, captchaTok, dev)
	if err != nil {
		return nil, err
	}
	var out []struct {
		ID   string         `json:"id"`
		Name string         `json:"name"`
		Kind string         `json:"kind"`
		Size sizex.ByteSize `json:"size"`
	}
	for _, f := range files {
		if f.Kind == "drive#folder" && seen[f.ID] {
			continue // 已遍历过的 folder（API 重复返回）——跳过重复条目（round-11 Minor）
		}
		if parentID != "" {
			f.Name = parentID + "/" + f.Name // 保留相对路径
		}
		out = append(out, f)
		if f.Kind == "drive#folder" {
			seen[f.ID] = true
			sub, err := r.listShareRecursiveSeen(ctx, shareID, f.ID, shareToken, captchaTok, dev, seen)
			if err != nil {
				r.log.Warn("share folder recurse failed", "folder", f.ID, "err", err)
				continue
			}
			out = append(out, sub...)
		}
	}
	return out, nil
}

// refreshCaptchaLocked 刷新 captcha token（持 r.mu）。签名 = captchaSign(deviceID, ts)。
func (r *ShareResolver) refreshCaptchaLocked(ctx context.Context) error {
	ts := time.Now().UnixMilli()
	sign := captchaSign(r.deviceID, fmt.Sprint(ts)) // nolint:contextcheck
	body := map[string]any{
		"action":        "GET:/drive/v1/share",
		"captcha_token": "",
		"client_id":     webClientID,
		"device_id":     r.deviceID,
		"meta": map[string]any{
			"captcha_sign":   sign,
			"client_version": webClientVersion,
			"package_name":   webPackageName,
			"timestamp":      fmt.Sprint(ts),
			"user_id":        "",
		},
		"redirect_uri": "",
	}
	var out struct {
		CaptchaToken string `json:"captcha_token"`
		ErrorCode    int    `json:"error_code"`
		ErrorDesc    string `json:"error_description"`
		ExpiresIn    int64  `json:"expires_in"` // 秒；缺省 0 → 保守 1h
	}
	if err := r.doJSONPost(ctx, r.userHost+"/v1/shield/captcha/init", body, &out); err != nil {
		return fmt.Errorf("pikpak captcha init: %w", err)
	}
	if out.ErrorCode != 0 || out.CaptchaToken == "" {
		return fmt.Errorf("pikpak captcha init: code=%d desc=%q", out.ErrorCode, out.ErrorDesc)
	}
	r.captchaToken = out.CaptchaToken
	r.captchaFetchedAt = time.Now()
	r.captchaTTL = time.Duration(out.ExpiresIn) * time.Second
	if r.captchaTTL <= 0 {
		r.captchaTTL = time.Hour // 缺省保守 1h（与分享直链 expire 实测同量级）
	}
	r.log.Debug("pikpak captcha refreshed", "device", r.deviceID[:8], "ttl", r.captchaTTL)
	return nil
}

// RefreshLink 轻量重取单个分享文件的直链（round-8 Minor：reResolve 不再全量重列）。
// 校验分享仍存在（shareDetail 一次，O(1) 列表）+ 单文件 file_info；分享被换（fileID 消失）
// 或 token 陈旧时返回错误/自动刷新。返回 (新直链, hash, err)。
func (r *ShareResolver) RefreshLink(ctx context.Context, shareID, fileID, shareToken string) (string, string, error) {
	r.mu.Lock()
	if r.captchaToken == "" || time.Since(r.captchaFetchedAt) > r.captchaTTL*2/3 {
		if capErr := r.refreshCaptchaLocked(ctx); capErr != nil {
			r.mu.Unlock()
			return "", "", capErr
		}
	}
	tok, dev := r.captchaToken, r.deviceID
	r.mu.Unlock()

	files, err := r.shareDetail(ctx, shareID, "", shareToken, tok, dev)
	if err != nil {
		return "", "", err
	}
	found := false
	for _, f := range files {
		if f.ID == fileID {
			found = true
			break
		}
	}
	if !found {
		return "", "", fmt.Errorf("pikpak share file %s not found (share changed?)", fileID)
	}
	link, hash, err := r.fileInfo(ctx, shareID, fileID, tok, dev)
	if err != nil {
		return "", "", err
	}
	return link, hash, nil
}

// shareDetail 列分享文件（/drive/v1/share/detail?share_id=...）。
// parentID 非空时列该文件夹子项（分享子目录遍历，与 pikpak.exe --parent 同端点/同参数）。
func (r *ShareResolver) shareDetail(ctx context.Context, shareID, parentID, shareToken, captchaTok, dev string) ([]struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Kind string         `json:"kind"`
	Size sizex.ByteSize `json:"size"`
}, error) {
	q := url.Values{}
	q.Set("share_id", shareID)
	q.Set("client_id", webClientID)
	q.Set("device_id", dev)
	if parentID != "" {
		q.Set("parent_id", parentID)
	}
	if shareToken != "" {
		q.Set("share_token", shareToken) // 分享子路径 token（/s/<id>/<token>）定位具体内容
	}
	var out struct {
		ShareStatus string `json:"share_status"`
		Files       []struct {
			ID   string         `json:"id"`
			Name string         `json:"name"`
			Kind string         `json:"kind"`
			Size sizex.ByteSize `json:"size"`
		} `json:"files"`
	}
	if err := r.doJSONGet(ctx, r.apiHost+"/drive/v1/share/detail", q, captchaTok, dev, &out); err != nil {
		return nil, err
	}
	if out.ShareStatus != "" && out.ShareStatus != "OK" {
		return nil, fmt.Errorf("pikpak share status %q", out.ShareStatus)
	}
	return out.Files, nil
}

// fileInfo 拿分享文件的匿名直链 + hash（/drive/v1/share/file_info）。
func (r *ShareResolver) fileInfo(ctx context.Context, shareID, fileID, captchaTok, dev string) (string, string, error) {
	q := url.Values{}
	q.Set("share_id", shareID)
	q.Set("file_id", fileID)
	q.Set("pass_code_token", "")
	var out struct {
		FileInfo struct {
			Hash           string `json:"hash"`
			WebContentLink string `json:"web_content_link"`
			Medias         []struct {
				Link struct {
					URL string `json:"url"`
				} `json:"link"`
			} `json:"medias"`
		} `json:"file_info"`
	}
	if err := r.doJSONGet(ctx, r.apiHost+"/drive/v1/share/file_info", q, captchaTok, dev, &out); err != nil {
		return "", "", err
	}
	if out.FileInfo.WebContentLink != "" {
		return out.FileInfo.WebContentLink, out.FileInfo.Hash, nil
	}
	for _, m := range out.FileInfo.Medias {
		if m.Link.URL != "" {
			return m.Link.URL, out.FileInfo.Hash, nil
		}
	}
	return "", "", nil
}

// doJSONGet 发起 GET 请求（匿名鉴权：X-Client-ID/X-Device-ID/X-Captcha-Token）。
func (r *ShareResolver) doJSONGet(ctx context.Context, u string, q url.Values, captchaTok, dev string, out any) error {
	full := u
	if len(q) > 0 {
		full += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return err
	}
	r.setAnonHeaders(req, captchaTok, dev)
	return r.doJSON(req, out)
}

// doJSON 执行请求并解析 JSON（统一错误处理）。
func (r *ShareResolver) doJSON(req *http.Request, out any) error {
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("pikpak api %s: HTTP %d: %s", req.URL.Path, resp.StatusCode, truncate(string(b), 200))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("pikpak api decode: %w", err)
	}
	return nil
}

// doJSONPost 是 POST 版（captcha init 用）。
func (r *ShareResolver) doJSONPost(ctx context.Context, u string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", defaultShareUA)
	req.Header.Set("X-Client-ID", webClientID)
	req.Header.Set("X-Device-ID", r.deviceID)
	return r.doJSON(req, out)
}

// setAnonHeaders 设置匿名 GET 请求头。
func (r *ShareResolver) setAnonHeaders(req *http.Request, captchaTok, dev string) {
	req.Header.Set("User-Agent", defaultShareUA)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Client-ID", webClientID)
	req.Header.Set("X-Device-ID", dev)
	if captchaTok != "" {
		req.Header.Set("X-Captcha-Token", captchaTok)
	}
}
