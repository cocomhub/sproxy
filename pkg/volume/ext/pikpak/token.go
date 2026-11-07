// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// token.go 负责凭据文件（~/.pikpak/.credentials.json）读写与 access_token 自动续期。
//
// 背景（用户实测 2026-10-11）：距上次下载隔一段时间后开始下载报「未登录」，
// 手工执行 `pikpak auth status` 后重试即可 —— 根因是 access_token 过期后客户端
// 既不检查 token_expiry、也不会用 refresh_token 续期，只能等外部 CLI 刷新凭据文件。
//
// 续期两条通道（优先 HTTP，回落官方 CLI）：
//  1. HTTP：`POST https://user.mypikpak.com/v1/auth/token`，头与官方 CLI 一致
//     （User-Agent: pikpak-cli/x.y.z + X-Client-Id/X-Device-Id/X-Client-Version），
//     体为 {client_id, client_secret, grant_type:"refresh_token", refresh_token}；
//     响应含**轮换后的 refresh_token**，必须落盘（否则下次续期可能 invalid_grant）。
//  2. CLI：`pikpak auth status`（官方 CLI 自行续期并写回凭据文件）→ 重读文件采用新 token。
//
// 凭据文件写回保持原字段顺序与 2 空格缩进（未识别字段原样保留，不丢数据）。
package pikpak

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// 官方 CLI 的 OAuth 客户端常量（凭据文件 client_id 同值；client_secret 取自官方 CLI
// 二进制中的公开常量，与既有 webClientID/webAlgorithms 同源——CLI 是公开桌面客户端，
// 其常量用于账号续期，且服务端要求 UA/头完全一致否则返回 403 permission_denied）。
const (
	cliClientID      = "aal7sToC85zMXEpJ"
	cliClientSecret  = "aapDEIgZwc9uGiT2"
	cliClientVersion = "0.5.2"
	// defaultUserHost 是 OAuth token 端点域名（与官方 CLI 一致）。
	defaultUserHost = "https://user.mypikpak.com"
	// authTokenPath 是 refresh_token 换 access_token 的端点路径。
	authTokenPath = "/v1/auth/token"
	// defaultRefreshSkew 是「临期即先续期」的提前量（token 12h 有效，5min 余量足够）。
	defaultRefreshSkew = 5 * time.Minute
	// refreshThrottle 是续期节流窗口：并发 401 只续期一次（避免 refresh_token 轮换风暴）。
	refreshThrottle = 10 * time.Second
)

// 凭据文件字段名（读写共用，避免重复字面量）。
const (
	credKeyClientID     = "client_id"
	credKeyDeviceID     = "device_id"
	credKeyAccessToken  = "access_token"
	credKeyRefreshToken = "refresh_token"
	credKeyTokenExpiry  = "token_expiry"
	credKeyLastUpdated  = "last_updated"
)

// tokenResponse 是 token 端点的响应（OAuth refresh_token grant）。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Sub          string `json:"sub"`
}

// credentials 是凭据文件的读写视图：values 保留全部原始字段（含未识别字段），
// order 记录顶层键顺序（写回时保持 CLI 原有排版，不改变键序/缩进）。
type credentials struct {
	values map[string]json.RawMessage
	order  []string
}

// strField 取字符串字段（缺失/类型不符 → 空串）。
func (c *credentials) strField(key string) string {
	raw, ok := c.values[key]
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s
}

// accessToken 返回凭据文件里的 access_token。
func (c *credentials) accessToken() string { return c.strField(credKeyAccessToken) }

// refreshToken 返回凭据文件里的 refresh_token。
func (c *credentials) refreshToken() string { return c.strField(credKeyRefreshToken) }

// clientID 返回凭据文件里的 OAuth client_id。
func (c *credentials) clientID() string { return c.strField(credKeyClientID) }

// deviceID 返回凭据文件里的 device_id（续期请求头需要）。
func (c *credentials) deviceID() string { return c.strField(credKeyDeviceID) }

// expiry 解析 token_expiry（RFC3339 带偏移）；缺失/解析失败 → 零值（= 过期信息未知）。
func (c *credentials) expiry() time.Time {
	s := c.strField(credKeyTokenExpiry)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// setString 写入字符串字段（新键追加到顺序末尾）。
func (c *credentials) setString(key, val string) {
	if _, ok := c.values[key]; !ok {
		c.order = append(c.order, key)
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return
	}
	c.values[key] = raw
}

// orderedKeys 返回写回顺序：原顺序优先 + 新增键补在后（排序稳定）。
func (c *credentials) orderedKeys() []string {
	seen := make(map[string]bool, len(c.values))
	out := make([]string, 0, len(c.values))
	for _, k := range c.order {
		if _, ok := c.values[k]; ok && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	var rest []string
	for k := range c.values {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(out, rest...)
}

// marshalIndent 按原键序输出（2 空格缩进，与官方 CLI 的凭据文件格式一致）。
func (c *credentials) marshalIndent() []byte {
	var b bytes.Buffer
	b.WriteString("{\n")
	keys := c.orderedKeys()
	for i, k := range keys {
		fmt.Fprintf(&b, "  %q: %s", k, compactJSON(c.values[k]))
		if i < len(keys)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// compactJSON 把原始 JSON 值压成单行（凭据字段均为标量；异常时原样返回）。
func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

// readCredentials 读凭据文件。
func readCredentials(path string) (*credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var vals map[string]json.RawMessage
	if err := json.Unmarshal(data, &vals); err != nil {
		return nil, fmt.Errorf("解析凭据文件: %w", err)
	}
	return &credentials{values: vals, order: topLevelKeys(data)}, nil
}

// jsonKeyScanner 扫描顶层 JSON 对象的键（按键值交替判定，忽略嵌套结构内部）。
type jsonKeyScanner struct {
	keys      []string
	depth     int
	expectKey bool
	done      bool
}

// feed 消费一个 JSON token。
func (s *jsonKeyScanner) feed(tok json.Token) {
	if delim, ok := tok.(json.Delim); ok {
		s.feedDelim(delim)
		return
	}
	if s.depth != 1 {
		return
	}
	if s.expectKey {
		if name, ok := tok.(string); ok {
			s.keys = append(s.keys, name)
		}
		s.expectKey = false
		return
	}
	s.expectKey = true // 值已消费 → 下一个是键
}

// feedDelim 处理结构定界符（维护深度与「下一个是键」状态）。
func (s *jsonKeyScanner) feedDelim(delim json.Delim) {
	switch delim {
	case '{', '[':
		s.depth++
		if s.depth == 1 {
			s.expectKey = true
		}
	case '}', ']':
		s.depth--
		if s.depth <= 0 {
			s.done = true
			return
		}
		if s.depth == 1 {
			s.expectKey = true // 嵌套值结束 → 下一个是键
		}
	}
}

// topLevelKeys 提取 JSON 顶层对象键顺序（凭据文件是扁平对象；保留 CLI 原有排版）。
// 注意：json.Decoder.Token() 同时返回**键与值**，必须按键值交替判定（否则字符串值被误当键）。
func topLevelKeys(data []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(data))
	s := &jsonKeyScanner{}
	for !s.done {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		s.feed(tok)
	}
	return s.keys
}

// persist 把续期结果原子写回凭据文件（新 access/refresh + token_expiry/last_updated）。
// 写入前先写同目录临时文件再 rename（避免半截文件让 CLI/其它进程读到坏凭据）。
func (c *credentials) persist(path string, resp *tokenResponse) error {
	now := time.Now()
	c.setString(credKeyAccessToken, resp.AccessToken)
	if resp.RefreshToken != "" {
		c.setString(credKeyRefreshToken, resp.RefreshToken)
	}
	if resp.ExpiresIn > 0 {
		c.setString(credKeyTokenExpiry, now.Add(time.Duration(resp.ExpiresIn)*time.Second).Format(time.RFC3339Nano))
	}
	c.setString(credKeyLastUpdated, now.Format(time.RFC3339Nano))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, c.marshalIndent(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// clientSecretFor 返回该 client_id 对应的 client_secret（仅官方 CLI 客户端已知；
// 未知 client → 空串 = 不做 HTTP 续期，直接回落 CLI）。
func clientSecretFor(clientID string) string {
	if clientID == cliClientID {
		return cliClientSecret
	}
	return ""
}

// httpRefreshToken 用 refresh_token 换新 access_token（官方 CLI 同款端点/头/体）。
func (a *API) httpRefreshToken(ctx context.Context, cred *credentials, secret string) (*tokenResponse, error) {
	payload, err := json.Marshal(map[string]string{
		"client_id":     cred.clientID(),
		"client_secret": secret,
		"grant_type":    "refresh_token",
		"refresh_token": cred.refreshToken(),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.userHost+authTokenPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pikpak-cli/"+cliClientVersion)
	req.Header.Set("X-Client-Id", cred.clientID())
	req.Header.Set("X-Client-Version", cliClientVersion)
	if dev := cred.deviceID(); dev != "" {
		req.Header.Set("X-Device-Id", dev)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := ioReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("token 续期 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("token 续期响应解析: %w", err)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return nil, errors.New("token 续期响应缺 access_token")
	}
	return &tr, nil
}

// refreshLocked 续期一次（调用方须持有 tokenMu）：HTTP 优先 → 官方 CLI 回落 → 明确错误。
// 节流：refreshThrottle 内已续期过 → 视为无需再续（并发 401 只续期一次）。
func (a *API) refreshLocked(ctx context.Context, cred *credentials) error {
	if time.Since(a.lastRefresh) < refreshThrottle {
		return nil
	}
	var lastErr error
	if secret := clientSecretFor(cred.clientID()); secret != "" && cred.refreshToken() != "" {
		tr, err := a.httpRefreshToken(ctx, cred, secret)
		if err == nil {
			a.applyToken(tr, cred)
			return nil
		}
		lastErr = err
		a.log.Warn("pikpak http token refresh failed", "err", err)
	}
	if err := a.refreshViaCLI(ctx, cred); err == nil {
		return nil
	} else if lastErr == nil {
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("凭据文件缺少 refresh_token（无法自动续期）")
	}
	return fmt.Errorf("%w: access_token 已过期且自动续期失败: %v（请运行 `pikpak auth status` 或重新登录）", ErrNotLoggedIn, lastErr)
}

// refreshViaCLI 回落官方 CLI：`pikpak auth status` 会自行续期并写回凭据文件，
// 随后重读文件采用新 token（token 未变但仍在有效期内也接受——CLI 判定为已登录）。
func (a *API) refreshViaCLI(ctx context.Context, cred *credentials) error {
	if a.cli == nil {
		return errors.New("未配置 pikpak CLI（无法回落续期）")
	}
	var st AuthStatus
	if err := a.cli.RunJSON(ctx, &st, "auth", "status"); err != nil {
		return fmt.Errorf("pikpak auth status: %w", err)
	}
	fresh, err := readCredentials(a.credPath)
	if err != nil {
		return fmt.Errorf("重读凭据文件: %w", err)
	}
	if fresh.accessToken() == "" {
		return errors.New("CLI 续期后凭据文件仍无 access_token")
	}
	exp := fresh.expiry()
	if fresh.accessToken() == cred.accessToken() && !exp.IsZero() && !time.Now().Before(exp) {
		return errors.New("CLI 未刷新出有效凭据（仍为过期 token）")
	}
	a.token = fresh.accessToken()
	a.expiry = exp
	a.lastRefresh = time.Now()
	a.log.Info("pikpak token refreshed via cli")
	return nil
}

// applyToken 采用续期结果（内存 + 落盘）。
func (a *API) applyToken(tr *tokenResponse, cred *credentials) {
	a.token = tr.AccessToken
	if tr.ExpiresIn > 0 {
		a.expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	} else {
		a.expiry = time.Time{}
	}
	a.lastRefresh = time.Now()
	if err := cred.persist(a.credPath, tr); err != nil {
		a.log.Warn("pikpak credentials persist failed", "err", err, "path", a.credPath)
	}
	a.log.Info("pikpak token refreshed", "expires_in", tr.ExpiresIn)
}
