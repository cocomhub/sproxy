// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

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

// DialHTTP 创建一个通过 HTTP POST 传输的 Conn。
// addr 是 sproxy 服务端地址（如 "http://localhost:18083"）。
func DialHTTP(ctx context.Context, addr string) (Conn, error) {
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
