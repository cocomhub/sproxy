// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// EndToEndOptions 配置端到端加密（与 SK 解耦：密钥 = 端到端身份派生，中间节点 X
// 即使持有集群 SK 也无法派生会话密钥——X 只透传密文，读不到明文）。
type EndToEndOptions struct {
	// Enabled 启用端到端加密。
	Enabled bool
	// Identity 是本端长时身份（Ed25519 密钥对）。握手时向对端证明持有私钥
	// （proof of possession），并参与 ECDH 会话密钥派生。
	Identity *tunnel.Identity
	// PeerFingerprints 是对端身份指纹 pinning 白名单（"sha256:<64hex>"）。
	// 非空时握手 fail-closed 校验对端指纹，不匹配或对端无身份即拒绝——
	// 与 remote_read_listener 的"无 pin 拒绝"一致，绝不回退静态密钥。
	PeerFingerprints []string
	// StaticKey 是握手用静态密钥（公开指纹派生，非 SK；见
	// tunnel.DeriveRemoteStaticKey）。会话密钥 = ECDH(L私钥, T公钥) + 该静态
	// 密钥参与派生——中间人 X 无 L/T 私钥，无法派生。
	StaticKey []byte
	// Handler 是 T 侧（ServeE2E 的 listener）处理解密后 HTTP 请求的处理器。
	// 端到端"数据面"= 隧道 HTTP 请求-响应交换（复用 tunnel.Tunnel 语义）。
	// 为空时 ServeE2E 回显请求体（echo，测试/诊断）。
	Handler http.Handler
	// HandshakeTimeout 覆写隧道握手超时（0 = 默认 30s）。
	HandshakeTimeout time.Duration
}

// DialE2E 是 L 侧端到端加密拨号：在外层数据面连接 outer 之上建隧道（dialer
// 角色），返回 E2EConn。对端（T）身份须在 opts.PeerFingerprints 白名单中，
// 否则握手 fail-closed 失败（不回退静态密钥）。
//
// 安全语义：会话密钥 = ECDH(L身份私钥, T身份公钥)（tunnel.Tunnel 的 C-1 握手，
// 静态密钥参与派生）。X（外层数据面的中间节点）只透传下层 mux 流密文字节，
// 无 L/T 私钥无法派生会话密钥——X 即使持有集群 SK 也读不到明文。
func DialE2E(ctx context.Context, outer net.Conn, opts EndToEndOptions) (*E2EConn, error) {
	if !opts.Enabled {
		return nil, fmt.Errorf("endtoend: EndToEndOptions.Enabled 必须为 true")
	}
	if outer == nil {
		return nil, fmt.Errorf("endtoend: 外层数据面连接为空")
	}
	if opts.Identity == nil {
		return nil, fmt.Errorf("endtoend: 缺少本端身份（Identity 必填）")
	}
	if len(opts.PeerFingerprints) == 0 {
		return nil, fmt.Errorf("endtoend: 缺少对端指纹 pin（PeerFingerprints 必填，fail-closed）")
	}
	if len(opts.StaticKey) == 0 {
		return nil, fmt.Errorf("endtoend: 缺少静态密钥（StaticKey 必填，由公开指纹派生）")
	}
	// 静态密钥由**对端指纹**派生（与 pkg/remote 的 dialer 侧一致）：对端（listener
	// 侧）用自己指纹派生同一值，dialer 侧用对端指纹派生——远端只读面已验证此
	// 约定（pkg/remote/client.go:255 DeriveRemoteStaticKey(pins[0])）。
	staticKey := tunnel.DeriveRemoteStaticKey(opts.PeerFingerprints[0])
	// 把外层 net.Conn 包装为 xfer.Conn（mux 的载体）。
	xc := xferFromNetConn(outer)
	m := mux.New(xc, mux.RoleDialer)
	tunOpts := []tunnel.TunnelOption{
		tunnel.WithIdentity(opts.Identity),
		tunnel.WithPeerFingerprints(opts.PeerFingerprints),
	}
	if opts.HandshakeTimeout > 0 {
		tunOpts = append(tunOpts, tunnel.WithHandshakeTimeout(opts.HandshakeTimeout))
	}
	tun := tunnel.NewTunnel(m, staticKey, tunOpts...)
	return &E2EConn{tun: tun, mux: m, outer: outer}, nil
}

// ServeE2E 是 T 侧（listener 角色）端到端加密接受：在外层数据面连接 outer 上
// 建隧道（listener 角色），进入 accept 循环前同步握手（fail-closed：对端指纹
// 不在白名单、或无身份、或不知静态密钥 → 返回错误，绝不回退）。
//
// 安全语义：与 DialE2E 对称。调用方（T）配置自己的 Identity + 白名单
// （PeerFingerprints = 允许的 L 指纹），X 只透传密文。
func ServeE2E(ctx context.Context, outer net.Conn, opts EndToEndOptions) error {
	if !opts.Enabled {
		return fmt.Errorf("endtoend: EndToEndOptions.Enabled 必须为 true")
	}
	if outer == nil {
		return fmt.Errorf("endtoend: 外层数据面连接为空")
	}
	if opts.Identity == nil {
		return fmt.Errorf("endtoend: 缺少本端身份（Identity 必填）")
	}
	if len(opts.PeerFingerprints) == 0 {
		return fmt.Errorf("endtoend: 缺少对端指纹 pin（PeerFingerprints 必填，fail-closed）")
	}
	if len(opts.StaticKey) == 0 {
		return fmt.Errorf("endtoend: 缺少静态密钥（StaticKey 必填，由公开指纹派生）")
	}
	handler := opts.Handler
	if handler == nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write(body)
		})
	}
	// 静态密钥由**本端指纹**派生（与 pkg/server/remote_read_listener.go:96 一致：
	// listener 用自己指纹派生）。dialer 侧用对端指纹（PeerFingerprints[0]）派生
	// 同一值——两端约定一致才能握手成功。
	staticKey := tunnel.DeriveRemoteStaticKey(opts.Identity.Fingerprint())
	xc := xferFromNetConn(outer)
	m := mux.New(xc, mux.RoleListener)
	tunOpts := []tunnel.TunnelOption{
		tunnel.WithIdentity(opts.Identity),
		tunnel.WithPeerFingerprints(opts.PeerFingerprints),
	}
	if opts.HandshakeTimeout > 0 {
		tunOpts = append(tunOpts, tunnel.WithHandshakeTimeout(opts.HandshakeTimeout))
	}
	tun := tunnel.NewTunnel(m, staticKey, tunOpts...)
	return tun.Serve(ctx, handler)
}

// E2EConn 是 L 侧端到端加密连接的对外视图：在隧道之上提供 HTTP 请求-响应交换
// （与 tunnel.Tunnel 语义一致；Do 触发首次握手）。
type E2EConn struct {
	tun   *tunnel.Tunnel
	mux   *mux.Mux
	outer net.Conn
}

// Do 发送一次 HTTP 请求并经隧道读回响应（首次调用触发 ECDH 握手，fail-closed）。
func (c *E2EConn) Do(ctx context.Context, method, urlPath, body string) (string, error) {
	if c == nil || c.tun == nil {
		return "", fmt.Errorf("endtoend: 隧道未初始化")
	}
	req, err := http.NewRequestWithContext(ctx, method, urlPath, strings.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("endtoend: 构造请求失败: %w", err)
	}
	resp, err := c.tun.Do(req)
	if err != nil {
		return "", fmt.Errorf("endtoend: 隧道请求失败: %w", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("endtoend: 读响应失败: %w", err)
	}
	return string(out), nil
}

// Close 关闭隧道与底层连接。
func (c *E2EConn) Close() error {
	if c == nil {
		return nil
	}
	if c.mux != nil {
		_ = c.mux.Close()
	}
	if c.outer != nil {
		return c.outer.Close()
	}
	return nil
}

// xferFromNetConn 把 net.Conn 包装为 xfer.Conn（mux 载体）。
// 复用内置 TCP 传输的 FromNetConn：4B 长度前缀帧定界 + 写超时兜底 + 读上限，
// 语义与 mesh 既有 webrtc/hub 中继数据面一致（Y 一期 AD-6 同款适配）。
var xferFromNetConn = builtin.FromNetConn
