// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
)

// Checksum 计算 src 的 SHA-256 十六进制摘要。
func Checksum(src io.Reader) (string, error) {
	dst := sha256.New()
	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", dst.Sum(nil)), nil
}

// FileChecksum 计算文件的 SHA-256 十六进制摘要。
func FileChecksum(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return Checksum(f)
}

// verifyChecksum 计算 reader 的实际 SHA-256 摘要并与 expected 比较。
// expected 为空时跳过校验，返回 true。
func verifyChecksum(expected string, reader io.Reader) bool {
	if expected == "" {
		return true
	}
	actual, err := Checksum(reader)
	if err != nil {
		return false
	}
	return actual == expected
}
