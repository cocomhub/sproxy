// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"golang.org/x/sys/windows"
)

// diskStats 返回指定目录所在磁盘的总大小、可用空间和使用量（字节）。
func diskStats(dir string) (total, free, used int64) {
	pDir, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, 0
	}
	var freeBytesAvailable, totalBytes, totalFreeBytes uint64
	if err := windows.GetDiskFreeSpaceEx(pDir, &freeBytesAvailable, &totalBytes, &totalFreeBytes); err != nil {
		return 0, 0, 0
	}
	return int64(totalBytes), int64(freeBytesAvailable), int64(totalBytes - freeBytesAvailable)
}
