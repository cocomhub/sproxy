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
	total = int64(stat.Blocks) * stat.Bsize
	free = int64(stat.Bfree) * stat.Bsize
	used = total - free
	return
}
