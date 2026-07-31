// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"fmt"
	"math"
)

// 字节大小常量。
const (
	KB = 1024
	MB = 1024 * KB
	GB = 1024 * MB
	TB = 1024 * GB
)

// FormatByte 格式化字节数为人类可读字符串。
func FormatByte(size float64) string {
	if size <= 0 || math.IsNaN(size) || math.IsInf(size, 0) {
		return "0 B"
	}
	if size >= TB {
		return fmt.Sprintf("%.1f TB", size/TB)
	}
	if size >= GB {
		return fmt.Sprintf("%.1f GB", size/GB)
	}
	if size >= MB {
		return fmt.Sprintf("%.1f MB", size/MB)
	} else if size >= KB {
		return fmt.Sprintf("%.1f KB", size/KB)
	}
	return fmt.Sprintf("%.0f B", size)
}

// FormatETA 格式化剩余时间为人类可读字符串。
func FormatETA(seconds int64) string {
	if seconds <= 0 {
		return "--:--"
	}
	if seconds >= 3600 {
		return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
	}
	if seconds >= 60 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}
