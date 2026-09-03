// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package downloader 提供云端下载插件框架。
// 各下载器实现（HTTP、FTP 等）通过 Registry 注册，按 source URL 匹配调度。
package downloader

import (
	"context"
	"io"
	"time"
)

// ProgressFunc 是下载进度回调函数。
// downloaded 是已下载字节数，total 是总大小（-1 表示未知）。
type ProgressFunc func(downloaded, total int64)

// Result 是下载完成的结果。
type Result struct {
	Size     int64     // 实际下载大小
	Checksum string    // SHA-256 十六进制
	ModTime  time.Time // 原始文件修改时间（从 HTTP Last-Modified 提取）
	ETag     string    // 服务器 ETag（用于 If-Range 续传一致性校验）
}

// QuotaSink 是可选写盘记账 sink：io.Writer + 本次下载（写盘会话）结束时回调。
// 由调用方（如 cloud download 配额）注入；nil factory 时下载器直写底层文件。
// Finish(success, oldSize)：success=true 表示本次写盘成功落定（释放未用 reserve +
// 覆盖写释放 oldSize）；false 表示放弃（保留已 commit 供续传 / 回拨由实现决定）。
type QuotaSink interface {
	io.Writer
	Finish(success bool, oldSize int64)
}

// SinkFactory 由调用方注入，把下载器写盘目标包装为带记账的 sink。
// contentLength 是本次写盘会话预计写入的字节数（已知时）或 <=0（未知）；
// resume 为 true 时表示在既有 .partial 上追加（增量写入），false 表示新建/截断重建。
// nil 返回表示无需包装（直写）。创建失败返回错误（本次下载中止，不可重试）。
type SinkFactory func(w io.Writer, contentLength int64, resume bool) (QuotaSink, error)

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

// WriterDownloader 是支持写盘记账 sink 注入的下载器（HTTPDownloader 实现）。
// DownloadWithWriter 与 Download 等价，但允许调用方把写盘字节经 QuotaSink 记账
// （外部下载配额边写边记）。调用方通过类型断言判断下载器是否支持。
type WriterDownloader interface {
	Downloader
	DownloadWithWriter(ctx context.Context, source string, destPath string, onProgress ProgressFunc, sinkFactory SinkFactory) (*Result, error)
}
