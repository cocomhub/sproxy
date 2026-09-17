// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
//
// 本文件是 BaiduPCS-Go（Apache-2.0，https://github.com/qjfoidnh/BaiduPCS-Go）
// pcsutil/pcsutil.go 的裁剪版本：仅保留 ContainsString。

// Package pcsutil 提供通用工具（裁剪自 BaiduPCS-Go）。
package pcsutil

// ContainsString 判断 ss 是否包含 s。
func ContainsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
