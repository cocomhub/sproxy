// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"crypto/sha256"
	"io"
	"log/slog"
)

// sha256Sum 返回 sha256 摘要（测试 JWT 签名用）。
func sha256Sum(b []byte) [sha256.Size]byte {
	return sha256.Sum256(b)
}

// testLogger 返回丢弃日志器。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
