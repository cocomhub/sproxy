// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package netutil 提供网络相关的统一工具函数。
package netutil

import (
	"net/http"
)

// IsolatedTransport 返回默认 Transport 的隔离副本（独立连接池）。
//
// 背景（硬规则）：http.DefaultTransport 是全局共享连接池——并行测试/多实例的
// CloseIdleConnections 会打断在途请求（"transport connection broken: http:
// CloseIdleConnections called"）。任何需要独立连接池的地方都应使用本函数，
// 而非零值 &http.Transport{}（零值丢失标准库精心调校的默认配置）或直接复用
// DefaultTransport。
//
// 语义：
//   - 以 http.DefaultTransport 为基座 Clone()——继承 ProxyFromEnvironment /
//     DialContext(30s) / MaxIdleConns(100) / IdleConnTimeout(90s) /
//     TLSHandshakeTimeout(10s) / ForceAttemptHTTP2 等默认调校；
//   - TLSClientConfig 置 nil——保持「TLS 未显式定制」约定（WithClientCert /
//     WithInsecureTLS 的 nil 分支语义不变）；
//   - 类型断言失败时防御回退零值 &http.Transport{}（DefaultTransport 具体类型
//     恒为 *http.Transport，回退仅防御极端环境）。
func IsolatedTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		// DefaultTransport 的具体类型恒为 *Transport；防御性回退为空配置。
		return &http.Transport{}
	}
	tr := base.Clone()
	tr.TLSClientConfig = nil
	return tr
}
