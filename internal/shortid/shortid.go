// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shortid 提供 shortHash 工具函数，截取 SHA-256 摘要的前 12 位用于日志显示。
// 避免在多个包中重复定义相同的 shortHash 函数。
package shortid

// ShortHash 截取十六进制字符串的前 12 位用于日志显示。
// 若字符串长度不超过 12，直接返回原串。
func ShortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}
