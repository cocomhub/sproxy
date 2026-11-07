// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"fmt"
	"io"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
)

// newCipherWriter 构造流式加密 writer（复用 files.NewCipherWriter，与归档
// archiveCipherWriter 同实现，保证加密产物格式一致可解）。cipherName 空 = aes-256-gcm。
func newCipherWriter(w io.Writer, cipherName string, key []byte) (io.WriteCloser, error) {
	if cipherName == "" {
		cipherName = defaultCipher
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("transfer: 加密密钥需 32 字节，实际 %d", len(key))
	}
	cw, err := files.NewCipherWriter(cipherName, key, w)
	if err != nil {
		return nil, fmt.Errorf("transfer: 加密 writer: %w", err)
	}
	return cw, nil
}

// defaultCipher 是默认加密算法（与归档 /api/archive 一致）。
const defaultCipher = "aes-256-gcm"

// unixToTime 转换 Unix 秒为 time.Time（tar header ModTime）。
func unixToTime(unix int64) time.Time {
	if unix <= 0 {
		return time.Unix(0, 0)
	}
	return time.Unix(unix, 0)
}
