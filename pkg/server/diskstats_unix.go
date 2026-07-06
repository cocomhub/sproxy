// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix || linux || darwin || freebsd

package server

import "syscall"

// diskStats 返回指定目录所在磁盘的总大小、可用空间和使用量（字节）。
func diskStats(dir string) (total, free, used int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dir, &stat); err != nil {
		return 0, 0, 0
	}
	// stat.Blocks/stat.Bfree 类型因平台而异（uint64 或 int64），统一转换
	total = int64(stat.Blocks) * int64(stat.Bsize)
	free = int64(stat.Bfree) * int64(stat.Bsize)
	used = total - free
	return
}
