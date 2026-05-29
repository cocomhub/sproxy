// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import "fmt"

// FormatByte 格式化字节数为人类可读字符串。
func FormatByte(size float64) string {
	if size > 1024*1024 {
		return fmt.Sprintf("%.1f MB", size/1024/1024)
	} else if size > 1024 {
		return fmt.Sprintf("%.1f KB", size/1024)
	}
	return fmt.Sprintf("%.0f B", size)
}

// FormatETA 格式化剩余时间为人类可读字符串。
func FormatETA(seconds int64) string {
	if seconds <= 0 {
		return "--:--"
	}
	if seconds > 3600 {
		return fmt.Sprintf("%dh %dm", seconds/3600, (seconds%3600)/60)
	}
	if seconds > 60 {
		return fmt.Sprintf("%dm %ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%ds", seconds)
}
