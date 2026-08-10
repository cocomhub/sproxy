// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import "os"

// openFileNoFollow 安全打开文件。
// Windows 不支持 O_NOFOLLOW，回退到 os.Open（os.SameFile 交叉验证在调用方保证）。
func openFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
