// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"log/slog"

	"golang.org/x/sys/windows"
)

// diskStats 返回指定目录所在磁盘的总大小、可用空间和使用量（字节）。
// 注意：free 返回的是可用空间（freeBytesAvailable），而非总空闲空间（totalFreeBytes）。
// 在 Windows 上，freeBytesAvailable 是调用方可用的配额（考虑磁盘配额），
// 而 totalFreeBytes 是磁盘的总空闲空间（不含配额限制）。这里使用 freeBytesAvailable
// 以反映调用方实际可用的空间。
func diskStats(dir string) (total, free, used int64) {
	pDir, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		slog.Warn("diskStats: UTF16PtrFromString 失败", "dir", dir, "error", err)
		return 0, 0, 0
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(pDir, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		slog.Warn("diskStats: GetDiskFreeSpaceEx 失败", "dir", dir, "error", err)
		return 0, 0, 0
	}
	return int64(totalBytes), int64(freeBytesAvailable), int64(totalBytes - freeBytesAvailable)
}
