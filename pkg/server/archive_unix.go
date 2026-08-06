// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package server

import (
	"os"
	"syscall"
)

// openFileNoFollow 安全打开文件，使用 O_NOFOLLOW 拒绝跟随符号链接。
func openFileNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}
