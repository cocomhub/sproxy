// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"fmt"
	"net/url"
)

// NormalizeEndpoints 将 hub 地址归一为信令 HTTP 基址与注册 WS 端点：
//   - httpBase（信令 post/poll 用，http(s)://host[:port]，剥 path）；
//   - wsURL（自动注册用，ws(s)://host[:port]/ws）。
//
// hubURL 接受 http(s):// 或 ws(s)://（含 /ws 等 path）；空串回退 serverURL。
// 畸形 URL / 未知 scheme 显式报错。
func NormalizeEndpoints(hubURL, serverURL string) (httpBase, wsURL string, err error) {
	if hubURL == "" {
		hubURL = serverURL
	}
	if hubURL == "" {
		return "", "", fmt.Errorf("hub 地址为空（--hub 未指定且 server_url 为空）")
	}
	u, perr := url.Parse(hubURL)
	if perr != nil {
		return "", "", fmt.Errorf("解析 hub 地址失败: %w", perr)
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
	default:
		return "", "", fmt.Errorf("不支持的 hub scheme %q（支持 http/https/ws/wss）", u.Scheme)
	}
	httpScheme, wsScheme := u.Scheme, u.Scheme
	switch u.Scheme {
	case "ws":
		httpScheme = "http"
	case "wss":
		httpScheme = "https"
	case "http":
		wsScheme = "ws"
	case "https":
		wsScheme = "wss"
	}
	return httpScheme + "://" + u.Host, wsScheme + "://" + u.Host + "/ws", nil
}
