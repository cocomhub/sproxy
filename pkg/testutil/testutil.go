// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package testutil 提供跨包可复用的测试辅助函数，避免各测试文件重复定义相同的工具函数。
//
// 本包刻意放在 pkg/ 下而非 internal/，以兼顾未来 cmd/* 独立为 go module 时的可达性。
// 包内不引入任何第三方依赖，仅使用 Go 标准库。
package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
)

// TestKey 返回一个 64 字符十六进制字符串，可用作 AES-256 测试密钥。
func TestKey() string {
	return strings.Repeat("a", 64)
}

// DiscardLogger 返回一个将所有输出写入 io.Discard 的 *slog.Logger 用于测试。
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// SHA256Hex 计算 data 的 SHA-256 摘要并以小写十六进制字符串返回。
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// CaptureStdout 执行 fn 并捕获在此期间写入 os.Stdout 的所有输出。
// fn 执行完毕后恢复原始的 os.Stdout。
func CaptureStdout(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stdout
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	return string(buf[:n])
}

// CaptureStderr 执行 fn 并捕获在此期间写入 os.Stderr 的所有输出。
// fn 执行完毕后恢复原始的 os.Stderr。
func CaptureStderr(fn func()) string {
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	w.Close()
	os.Stderr = old
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	return string(buf[:n])
}
