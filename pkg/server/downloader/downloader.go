// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package downloader 提供云端下载插件框架。
// 各下载器实现（HTTP、FTP 等）通过 Registry 注册，按 source URL 匹配调度。
package downloader

import "context"

// ProgressFunc 是下载进度回调函数。
// downloaded 是已下载字节数，total 是总大小（-1 表示未知）。
type ProgressFunc func(downloaded, total int64)

// Result 是下载完成的结果。
type Result struct {
	Size     int64  // 实际下载大小
	Checksum string // SHA-256 十六进制
}

// Downloader 是云端下载器接口。
// 各协议实现通过 Registry 注册，按 source URL 匹配调度。
type Downloader interface {
	// Download 从 source 下载到 destPath。
	// ctx 取消时尽早退出，保留已下载的部分。
	// onProgress 可为 nil（不关心进度）。
	Download(ctx context.Context, source string, destPath string, onProgress ProgressFunc) (*Result, error)

	// Supports 判断是否支持该 source（如根据 URL scheme 判断）。
	Supports(source string) bool

	// Name 返回下载器名称（如 "http"、"ftp"）。
	Name() string
}
