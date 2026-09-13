// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package remote 是**跨节点卷访问**的 A 侧：把 `remote://<node>/<vol>/<path>` 句柄解析为
// 「节点 + 卷 + 卷内相对路径」，经 mesh 建立到对端的加密隧道，并在其上执行文件操作。
//
// # 定位（与 pkg/sync、pkg/client 的分工）
//
//	L2 远程访问  pkg/remote   传输（mesh 建链/缓存）+ 寻址（remote:// 句柄）+ 授权上下文
//	L3 编排      pkg/sync     纯逻辑（枚举/差异/冲突/编排）+ FS 接口
//	L2 传输      pkg/client   HTTP 客户端 / mesh 服务发现 / hub 中继
//
// 本包**不**做差异计算、冲突判定、过滤、配额结算或目录树编排——那些属 `pkg/sync`。
// 它提供两件东西：
//
//  1. 低层协议方法 `List`/`Stat`/`Open`（供 CLI 与按需调用）；
//  2. **`sync.FS` 实现**（`Client.FS`）：使 `pkg/sync` 的引擎可把远端当作一个文件系统，
//     从而「远程同步」与「本地同步」共用同一套编排逻辑，无需第二份读写实现。
//
// # 现状与写批次
//
// 当前对端（B 侧）只提供**只读面**（Y 一期，`/remote/list|stat|download`）。因此 `FS` 的
// 写方法（`WriteFile`/`Rename`/`Delete`/`MakeDir`）返回 `ErrUnsupported`——**接口形状已按
// 最终形态固化**（读写 7 方法），写批次（P3）只需填实现，不需要改任何已发布的接口。
//
// # 传输可替换（Dialer）
//
// mesh 建链细节不在本包硬编码：`Dialer` 是唯一注入点。默认实现 `RelayDialer` 只经 hub
// 中继（基于 `pkg/client`）；WebRTC 直连在 `pkg/tunnel/mesh`——那是**独立 module**，根
// module 的包不得导入它（实测：根 go.mod 无其 replace），故直连拨号由 `cmd/sclient` 侧
// 注入 Dialer 实现。
package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// ServiceName 是对端为跨节点卷访问宣告的 mesh 服务名（服务发现用）。
//
// 只读面与写面**分服务名**：写批次将引入独立的写服务名与独立路由白名单，使只读通路
// 物理上不含写 handler（Y 一期 AD-7「非黑名单法」的安全属性不被写批次削弱）。
const ServiceName = "volread"

// ErrUnsupported 表示该操作在当前对端面（只读）尚未提供。
//
// 写方法一律返回本错误而非 panic/静默成功：接口形状已按最终形态固化，调用方可以立即按
// 「远端写」编写上层逻辑，只在真正执行时得到明确的 unsupported。
var ErrUnsupported = errors.New("remote: 对端当前只提供只读面（写批次尚未实现）")

// 只读面的响应头名（与对端 pkg/files.Stat 的设置保持一致）。
const (
	headerFileSize     = "X-File-Size"
	headerFileMTime    = "X-File-MTime"
	headerFileChecksum = "X-File-Checksum"
	headerFileIsDir    = "X-File-IsDir"
)

// Dialer 建立到 mesh 节点的**已认证字节流连接**（封装后再叠 mux + Tunnel）。
//
// 它是本包唯一的传输注入点：默认 `RelayDialer`（经 hub 中继）；测试用直连 Dialer；
// WebRTC 直连由 `cmd/sclient`（可导入 `pkg/tunnel/mesh` 子 module）提供。
type Dialer interface {
	Dial(ctx context.Context, node string) (net.Conn, error)
}

// Client 是跨节点卷访问的 A 侧客户端：按节点缓存 mux + Tunnel（握手只跑一次）。
//
// 并发安全：linkFor 持锁；同一节点的并发首连只建立一条链路（后续等待并复用）。
type Client struct {
	dialer   Dialer
	identity *tunnel.Identity
	timeout  time.Duration
	logger   *slog.Logger

	mu     sync.Mutex
	links  map[string]*link
	pins   map[string][]string // node → 允许的对端指纹（空 = 拒连，不 TOFU）
	closed bool
}

// link 是一条已建立的节点链路（mux 上的 Tunnel）。
type link struct {
	conn net.Conn
	m    *mux.Mux
	tun  *tunnel.Tunnel
}

// Option 配置 Client。
type Option func(*Client)

// WithIdentity 设置本端 Ed25519 身份（必填：无身份无法完成双向 pin 握手）。
func WithIdentity(id *tunnel.Identity) Option {
	return func(c *Client) { c.identity = id }
}

// WithPeerPin 追加某节点允许的对端指纹（可多次调用）。**未配置 pin 的节点一律拒连**
// （fail-closed，不 TOFU）——这与 Y 一期 AD-2 的信任模型一致。
func WithPeerPin(node, fingerprint string) Option {
	return func(c *Client) {
		node = strings.TrimSpace(node)
		fp := strings.TrimSpace(fingerprint)
		if node == "" || fp == "" {
			return
		}
		c.pins[node] = append(c.pins[node], fp)
	}
}

// WithHandshakeTimeout 设置隧道握手超时（0 = 使用 pkg/tunnel 默认值）。
func WithHandshakeTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithLogger 设置日志器（nil 回落 slog.Default()）。
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// New 构造客户端。dialer 为 nil 或未配任何 pin 均可构造，但**调用时会 fail-closed**
// （前者无法建链、后者拒绝连未 pin 的节点）。
func New(dialer Dialer, opts ...Option) *Client {
	c := &Client{
		dialer: dialer,
		links:  map[string]*link{},
		pins:   map[string][]string{},
		logger: slog.Default(),
	}
	for _, o := range opts {
		if o != nil {
			o(c)
		}
	}
	return c
}

// Close 关闭全部缓存链路（幂等）。
func (c *Client) Close() error {
	c.mu.Lock()
	c.closed = true
	links := make([]*link, 0, len(c.links))
	for _, l := range c.links {
		links = append(links, l)
	}
	c.links = map[string]*link{}
	c.mu.Unlock()

	for _, l := range links {
		_ = l.m.Close()
		_ = l.conn.Close()
	}
	return nil
}

// pinFor 返回节点的允许指纹列表（空 = 拒连）。
func (c *Client) pinFor(node string) []string {
	pins := c.pins[node]
	out := make([]string, 0, len(pins))
	for _, p := range pins {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// linkFor 返回（必要时建立）到 node 的链路。
//
// fail-closed 顺序：先查 pin（无 pin 立即拒，**不发起连接**）→ 再拨号 → mux → Tunnel。
// 隧道静态密钥由**对端指纹**确定性派生（`tunnel.DeriveRemoteStaticKey`），故 pin 既是
// 授权输入也是密钥派生输入，二者不可分离。
func (c *Client) linkFor(ctx context.Context, node string) (*link, error) {
	if node == "" {
		return nil, errors.New("remote: 节点名为空")
	}
	pins := c.pinFor(node)
	if len(pins) == 0 {
		return nil, fmt.Errorf("remote: 节点 %q 未配置对端指纹 pin（fail-closed，拒绝连接）", node)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("remote: 客户端已关闭")
	}
	if l, ok := c.links[node]; ok {
		c.mu.Unlock()
		return l, nil
	}
	c.mu.Unlock()

	if c.dialer == nil {
		return nil, errors.New("remote: 未配置 Dialer")
	}
	if c.identity == nil {
		return nil, errors.New("remote: 未配置本端身份（WithIdentity）")
	}

	conn, err := c.dialer.Dial(ctx, node)
	if err != nil {
		return nil, fmt.Errorf("remote: 拨号节点 %q 失败: %w", node, err)
	}
	m := mux.New(builtin.FromNetConn(conn), mux.RoleDialer)
	opts := []tunnel.TunnelOption{
		tunnel.WithIdentity(c.identity),
		tunnel.WithPeerFingerprints(pins),
	}
	if c.timeout > 0 {
		opts = append(opts, tunnel.WithHandshakeTimeout(c.timeout))
	}
	// 静态密钥由对端指纹派生：pin 列表非空且已校验，取首个（多 pin 场景由握手时的
	// WithPeerFingerprints 全量校验）。
	tun := tunnel.NewTunnel(m, tunnel.DeriveRemoteStaticKey(pins[0]), opts...)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		_ = m.Close()
		_ = conn.Close()
		return nil, errors.New("remote: 客户端已关闭")
	}
	if l, ok := c.links[node]; ok { // 并发首连：保留先建立者
		_ = m.Close()
		_ = conn.Close()
		return l, nil
	}
	l := &link{conn: conn, m: m, tun: tun}
	c.links[node] = l
	return l, nil
}

// do 在节点链路上执行一次隧道请求（调用方负责关闭响应体）。
func (c *Client) do(ctx context.Context, ref Ref, method, path string, query map[string]string, opts ...OpenOption) (*http.Response, error) {
	l, err := c.linkFor(ctx, ref.Node)
	if err != nil {
		return nil, err
	}
	q := make([]string, 0, len(query)+2)
	q = append(q, "volume="+urlQueryEscape(ref.Volume))
	if ref.Path != "" {
		q = append(q, "path="+urlQueryEscape(ref.Path))
	}
	for k, v := range query {
		q = append(q, urlQueryEscape(k)+"="+urlQueryEscape(v))
	}
	sort.Strings(q)
	target := path + "?" + strings.Join(q, "&")

	req, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, fmt.Errorf("remote: 构造请求失败: %w", err)
	}
	for _, o := range opts {
		if o != nil {
			o(req)
		}
	}
	resp, err := l.tun.Do(req)
	if err != nil {
		// 链路可能已失效：从缓存移除，下次调用重建（不重试当前请求——语义由调用方决定）。
		c.dropLink(ref.Node, l)
		return nil, fmt.Errorf("remote: 隧道请求 %s %s 失败: %w", method, path, err)
	}
	return resp, nil
}

// dropLink 从缓存移除指定链路（若仍是当前缓存者）并关闭它。
func (c *Client) dropLink(node string, l *link) {
	c.mu.Lock()
	cur, ok := c.links[node]
	if ok && cur == l {
		delete(c.links, node)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if ok {
		_ = l.m.Close()
		_ = l.conn.Close()
	}
}

// OpenOption 修改下载请求（当前：Range 起点）。
type OpenOption func(*http.Request)

// WithOffset 请求从 offset 字节开始（HTTP Range；对端支持 Range）。
func WithOffset(offset int64) OpenOption {
	return func(r *http.Request) {
		if offset > 0 {
			r.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		}
	}
}

// List 列出 ref 目录下的条目（顶层，不递归）。
//
// 返回的 `files.FileInfo.Name` 是**短名**（不含目录前缀），与对端 `/api/files` 契约一致；
// 需要 FS 根相对路径时用 `Client.FS` 的 `ListDir`（它按 FS 契约拼接）。
func (c *Client) List(ctx context.Context, ref Ref) ([]files.FileInfo, error) {
	resp, err := c.do(ctx, ref, http.MethodGet, "/remote/list", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError("list", resp)
	}
	var lr files.ListResponse
	if err := jsonDecode(resp.Body, &lr); err != nil {
		return nil, fmt.Errorf("remote: 解析 list 响应失败: %w", err)
	}
	return lr.Files, nil
}

// Stat 返回 ref 的元信息；路径不存在返回 (nil, nil)（与 `sync.FS` 契约一致）。
func (c *Client) Stat(ctx context.Context, ref Ref) (*files.FileInfo, error) {
	resp, err := c.do(ctx, ref, http.MethodHead, "/remote/stat", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, responseError("stat", resp)
	}
	info := &files.FileInfo{
		Name:     baseName(ref.Path),
		Checksum: resp.Header.Get(headerFileChecksum),
		IsDir:    resp.Header.Get(headerFileIsDir) == "true",
	}
	if v := resp.Header.Get(headerFileSize); v != "" {
		size, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("remote: stat 响应的 %s 非法: %q", headerFileSize, v)
		}
		info.Size = size
	}
	if v := resp.Header.Get(headerFileMTime); v != "" {
		mt, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			return nil, fmt.Errorf("remote: stat 响应的 %s 非法: %q", headerFileMTime, v)
		}
		info.ModTime = mt
	}
	return info, nil
}

// Open 打开 ref 供流式读取；调用方负责 Close。
//
// `WithOffset(n)` 请求从第 n 字节开始（对端支持 Range）；对端若不支持 Range 而忽略了该
// 头，返回的是完整内容（调用方按需自行跳过）——本包不伪造偏移语义。
func (c *Client) Open(ctx context.Context, ref Ref, opts ...OpenOption) (io.ReadCloser, error) {
	resp, err := c.do(ctx, ref, http.MethodGet, "/remote/download", nil, opts...)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		defer func() { _ = resp.Body.Close() }()
		return nil, responseError("download", resp)
	}
	return resp.Body, nil
}

// responseError 把非 2xx 响应转成带状态码与对端文案的错误。
func responseError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	msg := strings.TrimSpace(string(body))
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("remote: %s 失败（HTTP %d）: %s", op, resp.StatusCode, msg)
}

// baseName 返回正斜杠路径的最后一段（空路径返回 ""）。
func baseName(p string) string {
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		return ""
	}
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
