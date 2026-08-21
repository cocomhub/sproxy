// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RelayStreamRequest 是向 hub 发起任意 TCP 流中继的请求体。
type RelayStreamRequest struct {
	Target string `json:"target"`
	Type   string `json:"type"` // 固定 "tcp"
	Addr   string `json:"addr"` // 目标叶子要出站连接的 TCP 地址
}

// RelayStream 通过 hub 的 /api/relay/stream 建立一条到目标叶子出口节点的
// 双向字节流。返回的 net.Conn 代表「本地 ↔ hub ↔ 叶子出站 TCP」全链路，
// 调用方拿到后按普通 socket 使用（如 SSH 客户端连接）。
//
// 该连接不经过 http.Client 的请求/响应模型，而是直接拨 hub 地址发原始
// HTTP POST（CONNECT 风格），成功建立后返回底层连接。目标叶子必须开启
// --dial-allow（出口模式）才能出站 dial 指定 addr。
func (c *FileClient) RelayStream(ctx context.Context, target, addr string) (net.Conn, error) {
	if target == "" || addr == "" {
		return nil, fmt.Errorf("RelayStream: target 与 addr 均不能为空")
	}
	body, err := json.Marshal(RelayStreamRequest{Target: target, Type: "tcp", Addr: addr})
	if err != nil {
		return nil, fmt.Errorf("RelayStream: 序列化请求失败: %w", err)
	}

	u, err := url.Parse(c.serverURL)
	if err != nil {
		return nil, fmt.Errorf("RelayStream: 解析 serverURL 失败: %w", err)
	}
	scheme := u.Scheme
	host := u.Host
	if host == "" {
		return nil, fmt.Errorf("RelayStream: 无效的 serverURL %q", c.serverURL)
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second}
	var raw net.Conn
	switch scheme {
	case "https", "wss":
		// 用 tls.Dialer.DialContext（支持 ctx 取消，Ctrl+C 可中断 TLS 拨号）
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: c.relayTLSConfig()}
		raw, err = tlsDialer.DialContext(ctx, "tcp", host)
	case "http", "ws":
		raw, err = dialer.DialContext(ctx, "tcp", host)
	default:
		return nil, fmt.Errorf("RelayStream: 不支持的 scheme %q", scheme)
	}
	if err != nil {
		return nil, fmt.Errorf("RelayStream: 连接 hub 失败: %w", err)
	}

	// 发送原始 HTTP CONNECT 风格请求
	path := "/api/relay/stream"
	var b strings.Builder
	fmt.Fprintf(&b, "POST %s HTTP/1.1\r\n", path)
	fmt.Fprintf(&b, "Host: %s\r\n", host)
	b.WriteString("Content-Type: application/json\r\n")
	fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	if c.authToken != "" {
		fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", c.authToken)
	}
	b.WriteString("Connection: close\r\n\r\n")
	if _, werr := io.WriteString(raw, b.String()); werr != nil {
		raw.Close()
		return nil, fmt.Errorf("RelayStream: 写请求头失败: %w", werr)
	}
	if _, werr := raw.Write(body); werr != nil {
		raw.Close()
		return nil, fmt.Errorf("RelayStream: 写请求体失败: %w", werr)
	}

	// 读取响应头，校验是否成功建立
	br := bufio.NewReader(raw)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("RelayStream: 读响应状态失败: %w", err)
	}
	if !strings.Contains(statusLine, " 200 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		raw.Close()
		return nil, fmt.Errorf("RelayStream: hub 返回 %s%s", strings.TrimSpace(statusLine), string(rest))
	}
	// 读取剩余响应头直到空行
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			raw.Close()
			return nil, fmt.Errorf("RelayStream: 读响应头失败: %w", rerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 返回原始连接（bufio.Reader 中可能已缓冲后续数据，包装回 raw）
	return &bufferedNetConn{Conn: raw, reader: br}, nil
}

// relayTLSConfig 返回用于连接 hub 的 TLS 配置，兼容自签证书。
// 优先沿用 http.Client Transport 上的 TLSClientConfig（WithInsecureTLS/WithClientCert 设置）。
func (c *FileClient) relayTLSConfig() *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if u, err := url.Parse(c.serverURL); err == nil && u.Hostname() != "" {
		cfg.ServerName = u.Hostname()
	}
	// 从 httpClient 的 Transport 继承 TLS 配置（自签/客户端证书等）
	if c.httpClient != nil {
		if tr, ok := c.httpClient.Transport.(*http.Transport); ok && tr.TLSClientConfig != nil {
			cfg.InsecureSkipVerify = tr.TLSClientConfig.InsecureSkipVerify
			if len(tr.TLSClientConfig.Certificates) > 0 {
				cfg.Certificates = tr.TLSClientConfig.Certificates
			}
		}
	}
	return cfg
}

// bufferedNetConn 包装 net.Conn，使 bufio.Reader 中已缓冲的数据可被继续读取。
type bufferedNetConn struct {
	net.Conn
	reader *bufio.Reader
}

func (b *bufferedNetConn) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

// MeshService 是 hub 返回的一条 mesh 服务宣告。
type MeshService struct {
	Name string `json:"name"`
	Node string `json:"node"`
	Addr string `json:"addr,omitempty"`
}

// MeshServices 查询 hub 上所有节点宣告的 mesh 服务（供选路发现）。
func (c *FileClient) MeshServices(ctx context.Context) ([]MeshService, error) {
	var svcs []MeshService
	if err := c.doJSON(ctx, http.MethodGet, "/api/hub/services", nil, &svcs); err != nil {
		return nil, fmt.Errorf("查询 mesh 服务失败: %w", err)
	}
	return svcs, nil
}

// MeshConnect 查找托管指定服务的节点并建立经 hub 的流中继连接。
// 返回的 net.Conn 代表「本地 ⇄ hub ⇄ 托管节点（出口或本地服务）」。
// 目标节点必须已宣告该服务（relay start/portal 通过 Meta.Services 宣告）。
// 若多个节点宣告同名服务，依次尝试直到某个建立成功（首个离线不影响其余）。
func (c *FileClient) MeshConnect(ctx context.Context, service string) (net.Conn, string, error) {
	svcs, err := c.MeshServices(ctx)
	if err != nil {
		return nil, "", err
	}
	var lastErr error
	for _, s := range svcs {
		if s.Name != service {
			continue
		}
		conn, cerr := c.RelayStream(ctx, s.Node, s.Addr)
		if cerr != nil {
			lastErr = cerr
			continue // 该节点不可达，尝试下一个候选
		}
		return conn, s.Node, nil
	}
	if lastErr != nil {
		return nil, "", fmt.Errorf("mesh 服务 %q 的所有候选节点均连接失败: %w", service, lastErr)
	}
	return nil, "", fmt.Errorf("mesh 服务 %q 未找到（请确认目标节点已宣告该服务）", service)
}
