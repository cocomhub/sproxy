// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
)

// insecureHTTPClient 返回跳过 TLS 证书验证的 http.Client（仅开发/测试自签证书环境；
// 生产环境应使用受信 CA 或把自签 CA 加入 RootCAs，而非关闭校验）。
//
// 语义与 client.WithInsecureTLS 对齐：克隆/新建 transport 设 InsecureSkipVerify，
// 保留 Timeout。Timeout 取 60s 对齐 HubSignaler 长轮询（I11）——单次 poll 超时
// 60s > 服务端 PollTimeout(25s) + 网络余量。
func insecureHTTPClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

// hubWSDial 拨号 hub 的 WS 端点；insecure 时跳过证书校验（自签 wss hub 场景）。
//
// 非 insecure 路径保持 xfer.Get("ws").Dial 原样（零行为变化）；insecure 路径走
// ws.DialWithOptions 注入跳过证书校验的 HTTPClient。保留 xfer.Get("ws") 注册检查，
// 错误文案与既有 `ws 传输层未注册` 一致。
func hubWSDial(ctx context.Context, addr string, insecure bool) (xfer.Conn, error) {
	tp := xfer.Get("ws")
	if tp == nil {
		return nil, fmt.Errorf("ws 传输层未注册")
	}
	if !insecure {
		return tp.Dial(ctx, addr)
	}
	return ws.DialWithOptions(ctx, addr, ws.DialOptions{HTTPClient: insecureHTTPClient()})
}
