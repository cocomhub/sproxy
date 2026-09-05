// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
)

// TestKey returns a 64 hex char AES-256 test key (all 'a').
func TestKey() string {
	return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

// TestAccessKey returns a deterministic AccessKey（ak-<no mesh>-<32hex>）。
// 服务端 access_keys 配置与客户端 WithTunnel(ak, sk) 共用此值，保证隧道密钥派生一致。
// 注意：测试固定 32hex 标准形态（前缀 ak-，2026-09-05 起统一）。
func TestAccessKey() string {
	return "ak-00000000000000000000000000000000"
}

// SHA256Hex computes the hex-encoded SHA-256 hash of data.
func SHA256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ChecksumAt computes the SHA-256 hex of length bytes from f at offset (seek then
// limit-read). want == "" skips and returns true.
func ChecksumAt(f *os.File, offset, length int64, want string) (bool, error) {
	if want == "" {
		return true, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, length)); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// DiscardLogger returns a slog.Logger that writes to io.Discard.
func DiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// CaptureStderr captures stderr output during fn execution.
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

// CaptureStdout captures stdout output during fn execution.
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
