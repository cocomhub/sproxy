// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package netutil 提供网络相关的统一工具函数。
// 当前核心：隔离 Transport 构造（硬规则：不共享 http.DefaultTransport 连接池）。
package netutil

import (
	"net/http"
	"testing"
)

// TestIsolatedTransport_KeepsDefaults：Clone 基座保留标准库默认调校。
func TestIsolatedTransport_KeepsDefaults(t *testing.T) {
	t.Parallel()
	tr := IsolatedTransport()
	if tr == nil {
		t.Fatal("IsolatedTransport 返回 nil")
	}
	// 继承 DefaultTransport 的默认值（ProxyFromEnvironment / 拨号超时 / 连接池）。
	if tr.Proxy == nil {
		t.Error("应继承 ProxyFromEnvironment（非 nil）")
	}
	if tr.TLSHandshakeTimeout == 0 {
		t.Error("应继承 TLSHandshakeTimeout=10s")
	}
	if tr.MaxIdleConns == 0 {
		t.Error("应继承 MaxIdleConns=100")
	}
	if tr.ForceAttemptHTTP2 != true {
		t.Error("应继承 ForceAttemptHTTP2=true")
	}
}

// TestIsolatedTransport_TLSConfigNil：保持「TLS 未定制」约定。
func TestIsolatedTransport_TLSConfigNil(t *testing.T) {
	t.Parallel()
	tr := IsolatedTransport()
	if tr.TLSClientConfig != nil {
		t.Error("TLSClientConfig 应为 nil（保持 TLS 未定制语义）")
	}
}

// TestIsolatedTransport_IndependentPool：独立连接池（不共享 DefaultTransport 状态）。
func TestIsolatedTransport_IndependentPool(t *testing.T) {
	t.Parallel()
	tr := IsolatedTransport()
	// 改 clone 的连接池字段不影响 DefaultTransport。
	tr.MaxIdleConns = 1
	def := http.DefaultTransport.(*http.Transport)
	if def.MaxIdleConns == 1 {
		t.Error("修改隔离副本不应影响 DefaultTransport（连接池独立）")
	}
}

// TestIsolatedTransport_RepeatedCall：多次调用各自独立（不共享）。
func TestIsolatedTransport_RepeatedCall(t *testing.T) {
	t.Parallel()
	a := IsolatedTransport()
	b := IsolatedTransport()
	if a == b {
		t.Error("两次调用应返回不同实例（独立连接池）")
	}
	a.MaxIdleConns = 1
	if b.MaxIdleConns == 1 {
		t.Error("两个实例应互不影响")
	}
}
