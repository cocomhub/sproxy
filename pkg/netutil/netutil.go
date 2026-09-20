// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package netutil 提供网络相关的统一工具函数。
package netutil

import (
	"net/http"
	"sync"
)

// DefaultTransport 返回进程级共享的默认 Transport（连接复用）。
//
// 语义：与 http.DefaultTransport 等价（ProxyFromEnvironment / DialContext(30s) /
// MaxIdleConns(100) / IdleConnTimeout(90s) / TLSHandshakeTimeout(10s) /
// ForceAttemptHTTP2 等标准库默认调校），但：
//   - 经 sync.Once 惰性初始化一次，进程内所有调用方共享同一连接池（keep-alive 复用）；
//   - 避免裸包级 var 的初始化顺序问题与隐式共享（显式调用即显式声明共享意图）；
//   - TLSClientConfig 保持 nil（同 IsolatedTransport 的「TLS 未显式定制」约定）。
//
// 使用场景：**生产**长期运行进程——同一端点被多个实例/多次调用访问时连接池复用
// 是性能正确选择（Go 官方 DefaultTransport 即共享设计）。连接池生命周期由进程持有，
// 无外部 CloseIdleConnections 打断风险。
//
// 需要隔离（独立连接池）的场景（测试、不可信端点、防跨实例串连）用 IsolatedTransport。
func DefaultTransport() *http.Transport {
	sharedOnce.Do(func() {
		shared = IsolatedTransport()
	})
	return shared
}

var (
	sharedOnce sync.Once
	shared     *http.Transport
)

// IsolatedTransport 返回默认 Transport 的隔离副本（独立连接池）。
//
// 背景（硬规则 17）：http.DefaultTransport 是全局共享连接池——并行测试/多实例的
// CloseIdleConnections 会打断在途请求（"transport connection broken: http:
// CloseIdleConnections called"）。**测试场景**必须隔离：每个测试用例自建独立连接池，
// 避免 httptest.Server.Close() 打断其它用例在途的 idle 连接。
//
// 语义：
//   - 以 http.DefaultTransport 为基座 Clone()——继承 ProxyFromEnvironment /
//     DialContext(30s) / MaxIdleConns(100) / IdleConnTimeout(90s) /
//     TLSHandshakeTimeout(10s) / ForceAttemptHTTP2 等默认调校；
//   - TLSClientConfig 置 nil——保持「TLS 未显式定制」约定（WithClientCert /
//     WithInsecureTLS 的 nil 分支语义不变）；
//   - 类型断言失败时防御回退零值 &http.Transport{}（DefaultTransport 具体类型
//     恒为 *http.Transport，回退仅防御极端环境）。
//
// 使用场景：**测试**（每用例独立连接池，防打断）与需要明确隔离的生产场景。
// 生产默认共享请用 DefaultTransport()。
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
