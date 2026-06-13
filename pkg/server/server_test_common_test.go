// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// testKey 返回一个 64 字符 hex 密钥（32 字节）给测试使用。
// 安全警告：这是一个弱密钥（全 a），仅用于测试，不可用于生产环境。
// 生产环境应使用 sclient genkey 或 crypto/rand 生成密钥。
func testKey() string {
	return strings.Repeat("a", 64)
}

// testLogger 返回一个丢弃所有日志的 slog.Logger 供测试使用。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// withHeader 为 *http.Request 添加 header，返回自身便于链式调用。
// 该函数当前无调用者（由 server_auth_test.go 旧代码引用），但保留作为测试公共辅助模式参考。
//lint:file-ignore U1000 保留以备未来 auth 测试使用
func withHeader(r *http.Request, key, value string) *http.Request {
	r.Header.Set(key, value)
	return r
}
