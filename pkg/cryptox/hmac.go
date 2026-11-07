// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// hmac.go 是 HMAC-SHA256 的**单一实现**：S3 签名（SigV4）与通知 webhook 签名共用。
package cryptox

import (
	"crypto/hmac"
	"crypto/sha256"
)

// HMACSHA256 返回 HMAC-SHA256(key, data) 的原始字节。
func HMACSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
