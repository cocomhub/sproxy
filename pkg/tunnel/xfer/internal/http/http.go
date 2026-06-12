// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package http 提供基于 HTTP POST 的 xfer.Conn 传输层实现。
//
// 将一次 Send + Receive 包装为一次 HTTP POST 请求-响应往返。
// 在 init() 中自动注册到 xfer 全局注册表，名字为 "http"。
//
// 注意：HTTP 传输仅支持客户端 Dial，不支持服务端 Listen。
package http

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func init() {
	xfer.TransportRegistry.Register(plugin.Plugin[*xfer.Transport]{
		Name: "http",
		Instance: &xfer.Transport{
			Name:   "http",
			Dial:   Dial,
			Listen: nil, // HTTP 传输仅支持客户端 Dial
		},
		Priority: 0,
	})
}

// httpConn 将 HTTP POST 请求-响应包装为 Conn。
//
// 工作方式：
//
//	Send(msg) — 暂存消息
//	Receive(ctx) — 将暂存消息作为 POST body 发送到 /tunnel 端点，
//	                读取响应体作为接收消息返回
//
// 注意：此实现要求每次 Send 后必须调用一次 Receive，
// 不可多次 Send 后批量 Receive。
type httpConn struct {
	url     string
	client  *http.Client
	pending []byte
}

// Dial 创建一个通过 HTTP POST 传输的 Conn。
// addr 是 sproxy 服务端地址（如 "http://localhost:18083"）。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	return &httpConn{
		url:    addr + "/tunnel",
		client: http.DefaultClient,
	}, nil
}

func (c *httpConn) Send(ctx context.Context, msg []byte) error {
	c.pending = msg
	return nil
}

func (c *httpConn) Receive(ctx context.Context) ([]byte, error) {
	body := c.pending
	c.pending = nil
	if body == nil {
		body = []byte{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *httpConn) Close() error {
	c.client.CloseIdleConnections()
	return nil
}
