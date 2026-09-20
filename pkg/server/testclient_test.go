// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"testing"
)

// testHTTPClientAt 返回独立连接池的 HTTP client（无 t.Cleanup——供无 t 参数的并发
// goroutine helper 使用，调用方负责使用后 CloseIdleConnections）。
func testHTTPClientAt() *http.Client {
	return &http.Client{Transport: &http.Transport{}}
}

// testHTTPClient 返回测试专用的 HTTP client（每测试独立连接池）。
//
// 硬规则（AGENTS.md §17）：测试禁用 http.DefaultClient / 共享 DefaultTransport——
// 并行用例的 httptest.Server.Close() / CloseIdleConnections 会打断共享连接池上其它
// 用例在途的 idle 连接（表现为 "transport connection broken: http: CloseIdleConnections
// called"，pkg/accesskey #399 实证）。本仓已在 pkg/client、syncmock、cmd/sclient 多次实证。
//
// 用法：client := testHTTPClient(t)   （t.Cleanup 自动 CloseIdleConnections）
func testHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	c := testHTTPClientAt()
	t.Cleanup(c.CloseIdleConnections)
	return c
}
