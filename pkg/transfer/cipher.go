// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"fmt"
	"io"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
)

// newAESWriter 构造 AES-256-GCM 流式加密 writer（复用 files.NewCipherWriter，
// 与归档 archiveCipherWriter 同实现，保证加密产物格式一致可解）。
func newAESWriter(w io.Writer, key []byte) (io.WriteCloser, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("transfer: 加密密钥需 32 字节，实际 %d", len(key))
	}
	cw, err := files.NewCipherWriter("aes-256-gcm", key, w)
	if err != nil {
		return nil, fmt.Errorf("transfer: 加密 writer: %w", err)
	}
	return cw, nil
}

// unixToTime 转换 Unix 秒为 time.Time（tar header ModTime）。
func unixToTime(unix int64) time.Time {
	if unix <= 0 {
		return time.Unix(0, 0)
	}
	return time.Unix(unix, 0)
}
