// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package size 提供 client 和 server 共享的大小常量，避免魔数与重复定义。
//
// 传输大小上限（UploadBodyLimit、DefaultChunkBodyLimit、MaxChunkHashBuf）
// 为最终硬限制，禁止配置以避免误配导致 413 或 OOM。
// 默认值（DefaultChunkSize、DefaultMaxChunkSize 等）为建议值，可配置。
package size

const (
	KiB = 1 << 10
	MiB = 1 << 20
	GiB = 1 << 30

	// === 传输大小上限（最终硬限制，禁止配置）===

	// UploadBodyLimit 是普通（非分块）上传请求体的最大字节数。
	// 超过此上限的普通上传请求被拒绝（HTTP 413）。
	UploadBodyLimit = 1 * GiB

	// DefaultChunkBodyLimit 是分块上传单块请求体的最大字节数（含 multipart 开销）。
	// 超过此上限的分块请求被拒绝（HTTP 413）。
	DefaultChunkBodyLimit = 64 * MiB

	// MaxChunkHashBuf 是下载分块 hash 计算的最大缓冲字节数。
	// 对应单个 /download/chunk 请求能够返回的最大字节数。
	MaxChunkHashBuf = 64 * MiB

	// CompleteBodyLimit 是 /upload/complete 请求体的最大字节数。
	CompleteBodyLimit = 1 * KiB

	// === 建议默认值（可配置的 soft limit）===

	// DefaultChunkSize 是默认分块大小（4 MiB）。
	DefaultChunkSize = 4 * MiB

	// DefaultMaxChunkSize 是客户端最大分块大小（64 MiB）。
	DefaultMaxChunkSize = 64 * MiB

	// AutoChunkThreshold 超过此大小的文件自动启用分块上传（100 MiB）。
	AutoChunkThreshold = 100 * MiB

	// MultipartBufSize 是 ParseMultipartForm 的内存缓冲大小（1 MiB）。
	// 超出此大小的文件部分由 stdlib 落临时文件。
	MultipartBufSize = 1 * MiB
)
