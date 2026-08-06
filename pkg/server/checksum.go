// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

// Checksum 计算 src 的 SHA-256 十六进制摘要。
// 注意：调用方负责关闭 src 如果它实现了 io.Closer（如 os.File）。
// 返回的 hex 字符串均为小写字符。
func Checksum(src io.Reader) (string, error) {
	dst := sha256.New()
	if _, err := io.CopyBuffer(dst, src, make([]byte, 256*1024)); err != nil {
		return "", err
	}
	return hex.EncodeToString(dst.Sum(nil)), nil
}

// FileChecksum 计算文件的 SHA-256 十六进制摘要。
// 调用方需确保 filename 已通过 ValidateFilePath 校验，防止路径穿越。
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
// 注意：此函数会完全消耗 reader，调用方需确保 reader 可重复读取或已备份。
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
