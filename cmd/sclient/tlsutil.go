// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// insecureHTTPClient 返回跳过 TLS 证书验证的 http.Client（仅自签证书开发/测试环境）。
// 委托 pkg/client.InsecureHTTPClient；语义一致（transport InsecureSkipVerify +
// Timeout 60s 对齐信令长轮询）。生产应使用真实证书/受信 CA。
func insecureHTTPClient() *http.Client {
	return client.InsecureHTTPClient()
}

// hubWSDial 拨号 hub 的 WS 端点；insecure 时跳过证书校验（自签 wss hub 场景）。
// 委托 pkg/tunnel/mesh.HubWSDial。
func hubWSDial(ctx context.Context, addr string, insecure bool) (xfer.Conn, error) {
	return mesh.HubWSDial(ctx, addr, insecure)
}
