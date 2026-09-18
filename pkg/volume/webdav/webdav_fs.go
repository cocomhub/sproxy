// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package webdav 提供 WebDAV 存储后端客户端：把任意 WebDAV 服务（Nextcloud /
// 坚果云 / OwnCloud 等，RFC 4918）适配为 `pkg/sync.FS`（7 方法），作为 sproxy
// 外部卷（V3 通用卷模型，RegisterBackend("webdav") 接入系统盘/用户卷）。
//
// 与 pkg/gateway/webdav（服务端网关：sync.FS → WebDAV 协议暴露）互为反向：
// 本包是客户端（WebDAV 服务 → sync.FS）。
//
// 中间态约束（用户硬规则）：WriteFile 先落本地 staging 再 PUT；OpenRead GET 后
// 落本地临时文件——本包不持有中间态（由 backend 构造器配 local_root 落本地）。
package webdav

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/sync"
)

// ClientConfig 是 WebDAV 客户端配置。
type ClientConfig struct {
	// RootURL 是 WebDAV 根 URL（http(s)://host[:port][/webdav-root]；必填）。
	RootURL string
	// Username/Password 触发 Basic Auth（两者非空时启用）。
	Username string
	Password string
	// Token 触发 Bearer 认证（非空时优先于 Basic）。
	Token string
	// HTTPClient 自定义 HTTP 客户端（测试注入；nil = 自建独立 Transport）。
	HTTPClient *http.Client
}

// ClientOption 是构造选项（函数式）。
type ClientOption func(*ClientConfig)

// WithBasicAuth 配置 Basic 认证。
func WithBasicAuth(username, password string) ClientOption {
	return func(c *ClientConfig) { c.Username, c.Password = username, password }
}

// WithBearerToken 配置 Bearer 认证（优先于 Basic）。
func WithBearerToken(token string) ClientOption {
	return func(c *ClientConfig) { c.Token = token }
}

// WithHTTPClient 注入自定义 HTTP 客户端（测试用）。
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *ClientConfig) { c.HTTPClient = hc }
}

// WebDAVFS 是 WebDAV 存储后端的 sync.FS 实现。
//
// 并发安全：每实例独立 http.Client（仓库硬规则 17：禁共享 DefaultTransport），
// http.Client 自身并发安全；本结构无可变共享状态（rootURL/auth 只读）。
type WebDAVFS struct {
	rootURL *url.URL // 归一后的 WebDAV 根（去尾斜杠）
	client  *http.Client
	auth    authProvider
}

// authProvider 是认证头提供者（Basic 或 Bearer）。
type authProvider func(*http.Request)

// NewWebDAVFS 构造 WebDAV 客户端。RootURL 必填且须为 http(s) 合法 URL。
// HTTPClient nil 时自建独立 Transport（每实例连接池，禁共享 DefaultTransport）。
func NewWebDAVFS(cfg ClientConfig) (*WebDAVFS, error) {
	if strings.TrimSpace(cfg.RootURL) == "" {
		return nil, fmt.Errorf("webdav: root url 为空")
	}
	u, err := url.Parse(strings.TrimSpace(cfg.RootURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("webdav: root url 非法: %q", cfg.RootURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/")

	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{}} // 独立 Transport（硬规则 17）
	}

	var auth authProvider
	switch {
	case cfg.Token != "":
		token := cfg.Token
		auth = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	case cfg.Username != "" && cfg.Password != "":
		username, password := cfg.Username, cfg.Password
		auth = func(r *http.Request) { r.SetBasicAuth(username, password) }
	}

	return &WebDAVFS{rootURL: u, client: client, auth: auth}, nil
}

// Close 关闭底层 HTTP 连接（幂等安全）。
func (f *WebDAVFS) Close() error {
	if tr, ok := f.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

// _ 编译期断言：WebDAVFS 实现 sync.FS。
var _ sync.FS = (*WebDAVFS)(nil)

// urlFor 把 FS 相对路径映射为 WebDAV 绝对 URL（根 + 相对路径拼接 + 转义）。
// 相对路径用 / 分隔；空/"/" → 根。
func (f *WebDAVFS) urlFor(rel string) string {
	rel = strings.TrimPrefix(rel, "/")
	u := *f.rootURL // 值拷贝（浅拷贝 URL 结构，字段只读不改）
	if rel != "" {
		// 逐段转义（保留 / 分隔），仿 url.JoinPath 语义但显式控制。
		segs := strings.Split(rel, "/")
		for i, s := range segs {
			segs[i] = url.PathEscape(s)
		}
		u.Path = u.Path + "/" + strings.Join(segs, "/")
	}
	return u.String()
}

// newRequest 构造带认证的请求（ctx + method + url + body）。
func (f *WebDAVFS) newRequest(ctx context.Context, method, rel string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, f.urlFor(rel), body)
	if err != nil {
		return nil, fmt.Errorf("webdav: 构造 %s 请求 %q 失败: %w", method, rel, err)
	}
	if f.auth != nil {
		f.auth(req)
	}
	return req, nil
}

// ListDir 列出 path 的直接子条目（PROPFIND depth=1，单层不递归）。
// 条目 Path 为 FS 根相对完整路径（正斜杠）；目录条目标 IsDir。
func (f *WebDAVFS) ListDir(ctx context.Context, relPath string) ([]sync.Entry, error) {
	req, err := f.newRequest(ctx, "PROPFIND", relPath, strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav: PROPFIND %q: %w", relPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // 目录不存在 → 空列表（引擎视为空目录）
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("webdav: PROPFIND %q: 状态 %d", relPath, resp.StatusCode)
	}
	responses, err := parseMultistatus(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("webdav: 解析 PROPFIND %q 响应: %w", relPath, err)
	}
	base := strings.TrimPrefix(cleanPath(relPath), "/")
	var out []sync.Entry
	for _, r := range responses {
		href := strings.TrimPrefix(r.Href, "/")
		href = path.Clean(href)
		if href == "." || href == "" {
			continue
		}
		// 跳过自身（depth=1 含自身 response）。
		if base != "" && (href == base || strings.HasPrefix(href, base+"/")) {
			if href == base {
				continue
			}
		}
		// 条目 Path 必须为**完整相对路径**（FS 根基准，正斜杠）——引擎 walkDir 递归依赖
		// 子目录条目的 Path 含全前缀（stripRootPrefix 用根裁剪）；只给目录内相对会导致
		// 递归丢失层级（e.Path="b.txt" 而非 "sub/b.txt"）。
		// href 是绝对路径（含 WebDAV 根 URL 路径前缀），归一为相对 FS 根：去掉根前缀后
		// 保留完整相对路径；根 URL 带路径前缀（如 /remote.php/webdav）时按 href 的
		// path.Clean 结果直接截取（服务端返回的 href 已含根 URL 的 path 部分）。
		rel := href
		if base != "" {
			rel = strings.TrimPrefix(href, base+"/")
		}
		// 完整相对路径 = base + "/" + 子名（base 空 = 根）。
		// 完整相对路径 = base + "/" + 子名（base 空 = 根）。
		full := rel
		if base != "" {
			full = base + "/" + rel
		}
		full = strings.TrimPrefix(full, "/")
		if full == "" {
			continue
		}
		out = append(out, entryFromProp(full, r.Prop))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Stat 返回条目；不存在返回 (nil, nil)（PROPFIND depth=0）。
func (f *WebDAVFS) Stat(ctx context.Context, relPath string) (*sync.Entry, error) {
	req, err := f.newRequest(ctx, "PROPFIND", relPath, strings.NewReader(propfindBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "0")
	req.Header.Set("Content-Type", "application/xml")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav: PROPFIND %q: %w", relPath, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("webdav: PROPFIND %q: 状态 %d", relPath, resp.StatusCode)
	}
	responses, err := parseMultistatus(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("webdav: 解析 PROPFIND %q 响应: %w", relPath, err)
	}
	if len(responses) == 0 {
		return nil, nil
	}
	rel := strings.TrimPrefix(cleanPath(relPath), "/")
	e := entryFromProp(rel, responses[0].Prop)
	return &e, nil
}

// OpenRead 读取文件（GET）→ io.ReadCloser。
func (f *WebDAVFS) OpenRead(ctx context.Context, relPath string) (io.ReadCloser, error) {
	req, err := f.newRequest(ctx, http.MethodGet, relPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("webdav: GET %q: %w", relPath, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("webdav: GET %q: 文件不存在", relPath)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("webdav: GET %q: 状态 %d", relPath, resp.StatusCode)
	}
	return resp.Body, nil
}

// WriteFile 全量覆盖写入（PUT）。mtime 由服务端决定（WebDAV 无标准 PROPPATCH
// lastmodified；需要时 backend 层可扩展）。
//
// 写前确保父目录（逐级 MKCOL，幂等）：真实 WebDAV 服务端（Nextcloud 等）PUT 到
// 不存在的父目录会 409；引擎 push 时目录条目在 SyncEmptyDirs=false 下不建目录（ActionSkipped），
// 故此处按需逐级建父目录（目录已存在 → MKCOL 405 → 幂等忽略）。
func (f *WebDAVFS) WriteFile(ctx context.Context, relPath string, r io.Reader, size, mtime int64) error {
	if err := f.ensureParentDirs(ctx, relPath); err != nil {
		return err
	}
	req, err := f.newRequest(ctx, http.MethodPut, relPath, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: PUT %q: %w", relPath, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusNoContent, http.StatusOK:
		return nil
	default:
		return fmt.Errorf("webdav: PUT %q: 状态 %d", relPath, resp.StatusCode)
	}
}

// ensureParentDirs 逐级确保 relPath 的父目录存在（MKCOL，幂等）。
// 父目录链中已存在的目录：MKCOL → 405 → 幂等忽略；不存在 → 创建。
func (f *WebDAVFS) ensureParentDirs(ctx context.Context, relPath string) error {
	parent := path.Dir(strings.TrimPrefix(relPath, "/"))
	if parent == "." || parent == "/" {
		return nil
	}
	// 逐级（从最浅到最深）：dir1 → dir1/dir2 → ...
	var segs []string
	for _, s := range strings.Split(parent, "/") {
		if s == "" {
			continue
		}
		segs = append(segs, s)
		if err := f.MakeDir(ctx, strings.Join(segs, "/")); err != nil {
			// MKCOL 失败（非 405 已存在）→ 继续尝试上级（服务端不一致时尽量写文件）；
			// 写文件本身会再报错，这里不阻塞整链。
			continue
		}
	}
	return nil
}

// Rename 移动（MOVE，Destination 头为目标绝对 URL）。
func (f *WebDAVFS) Rename(ctx context.Context, from, to string) error {
	req, err := f.newRequest(ctx, "MOVE", from, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Destination", f.urlFor(to))
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: MOVE %q → %q: %w", from, to, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("webdav: MOVE %q → %q: 状态 %d", from, to, resp.StatusCode)
	}
}

// Delete 删除（DELETE；404 → 幂等 nil）。
func (f *WebDAVFS) Delete(ctx context.Context, relPath string) error {
	req, err := f.newRequest(ctx, http.MethodDelete, relPath, nil)
	if err != nil {
		return err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: DELETE %q: %w", relPath, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK, http.StatusNotFound:
		return nil // 404 幂等（已不存在）
	default:
		return fmt.Errorf("webdav: DELETE %q: 状态 %d", relPath, resp.StatusCode)
	}
}

// MakeDir 创建目录（MKCOL；已存在 → 幂等 nil——服务端 405 视为已存在）。
func (f *WebDAVFS) MakeDir(ctx context.Context, relPath string) error {
	req, err := f.newRequest(ctx, "MKCOL", relPath, nil)
	if err != nil {
		return err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("webdav: MKCOL %q: %w", relPath, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusMethodNotAllowed: // 405 = 已存在（RFC 4918）
		return nil
	default:
		return fmt.Errorf("webdav: MKCOL %q: 状态 %d", relPath, resp.StatusCode)
	}
}

// propfindBody 是 PROPFIND 请求体（allprop）。
const propfindBody = `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:allprop/></d:propfind>`

// ---- PROPFIND 响应 XML 解析（RFC 4918 multistatus）----

// multistatus 是 PROPFIND 响应的根元素。
type multistatus struct {
	XMLName  xml.Name   `xml:"DAV: multistatus"`
	Response []response `xml:"response"`
}

type response struct {
	Href string `xml:"href"`
	// Propstat 的 prop 直接嵌入（简化：取第一个 200 propstat）。
	Prop prop `xml:"propstat>prop"`
}

// prop 是 WebDAV 属性集合（RFC 4918 常用属性）。
type prop struct {
	// ResourceType 捕获 <resourcetype><collection/> 子元素（nil = 非目录）。
	// 用 *struct{} 而非 string：collection 是空元素，string 字段捕获不到其文本。
	ResourceType  *struct{} `xml:"resourcetype>collection"`
	DisplayName   string    `xml:"displayname"`
	ContentLength string    `xml:"getcontentlength"`
	LastModified  string    `xml:"getlastmodified"`
	IsHidden      string    `xml:"ishidden"`
}

// parseMultistatus 解析 PROPFIND 响应体。
func parseMultistatus(r io.Reader) ([]response, error) {
	dec := xml.NewDecoder(r)
	var ms multistatus
	if err := dec.Decode(&ms); err != nil {
		return nil, err
	}
	return ms.Response, nil
}

// entryFromProp 把 WebDAV 属性映射为 sync.Entry。
// rel 为 FS 根相对路径（正斜杠）；Name = 最后一段。
func entryFromProp(rel string, p prop) sync.Entry {
	e := sync.Entry{
		Path:  rel,
		Name:  path.Base(rel),
		IsDir: p.ResourceType != nil, // <collection/> 子元素存在 = 目录
	}
	if e.Name == "." || e.Name == "/" {
		e.Name = ""
	}
	if p.ContentLength != "" {
		_, _ = fmt.Sscanf(p.ContentLength, "%d", &e.Size)
	}
	if p.LastModified != "" {
		if t, err := time.Parse(http.TimeFormat, p.LastModified); err == nil {
			e.MTime = t.UnixNano()
		}
	}
	return e
}

// cleanPath 归一相对路径（防路径穿越：拒绝 ..）。
func cleanPath(rel string) string {
	rel = strings.TrimPrefix(rel, "/")
	clean := path.Clean("/" + rel)
	return strings.TrimPrefix(clean, "/")
}
