// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

// s3_multipart.go 是大文件分片上传配置（roadmap 3.3 P1 外部后端扩展「s3 分片」）。
//
// 复用 minio PutObject 的内建 multipart（size>16MiB 自动分片 + 失败自动 Abort 防孤儿）：
// 本文件补**可配能力**——阈值判定（shouldMultipart 决定显式 multipart 语义）、分片大小
// 归一（PartSize 透传）、失败重试（PutObject 退避重试 UploadRetries 次）。
//
// 小文件（< 阈值）仍单 PutObject（minio <16MiB 单原子 PUT）——零回归。

import (
	"context"
	"io"
	"time"

	"github.com/minio/minio-go/v7"
)

const (
	// defaultMultipartThreshold 是走 multipart 的大文件判定阈值（默认 64MiB）。
	defaultMultipartThreshold = 64 << 20
	// defaultMultipartPartSize 是分片大小默认值（16MiB；minio 默认 16MiB 一致）。
	defaultMultipartPartSize = 16 << 20
	// minMultipartPartSize 是 minio 允许的最小分片大小（5MiB 下限）。
	minMultipartPartSize = 5 << 20
	// defaultUploadRetries 是 PutObject 失败重试次数（退避重试，默认 3）。
	defaultUploadRetries = 3
	// multipartRetryBaseDelay 是重试退避基准延迟（第 1 次重试等待）。
	multipartRetryBaseDelay = 200 * time.Millisecond
)

// shouldMultipart 判定 size 是否走分片上传（>= threshold；threshold<=0 禁用分片）。
func shouldMultipart(size, threshold int64) bool {
	return threshold > 0 && size >= threshold
}

// normalizePartSize 归一分片大小（<=0 用默认；小于 minio 5MiB 下限钳制）。
func normalizePartSize(partSize int64) int64 {
	if partSize <= 0 {
		return defaultMultipartPartSize
	}
	if partSize < minMultipartPartSize {
		return minMultipartPartSize
	}
	return partSize
}

// normalizeRetries 归一重试次数（<=0 用默认）。
func normalizeRetries(n int) int {
	if n <= 0 {
		return defaultUploadRetries
	}
	return n
}

// multipartOptions 是 WriteFile 分片上传的可配参数（归一后）。
type multipartOptions struct {
	// threshold 是走 multipart 的阈值（0 = 禁用分片，恒单 PutObject）。
	threshold int64
	// partSize 是分片大小（已归一 >=5MiB）。
	partSize int64
	// retries 是 PutObject 失败重试次数（已归一 >=1）。
	retries int
}

// multipartOptsFromConfig 从 ClientConfig 构造归一后的分片参数。
func multipartOptsFromConfig(cfg ClientConfig) multipartOptions {
	return multipartOptions{
		threshold: cfg.MultipartThreshold,
		partSize:  normalizePartSize(cfg.MultipartPartSize),
		retries:   normalizeRetries(cfg.UploadRetries),
	}
}

// putter 是 PutObject 的最小接口（minio.Client 实现；测试注入失败用）。
type putter interface {
	PutObject(ctx context.Context, bucketName, objectName string, reader io.Reader, size int64,
		opts minio.PutObjectOptions) (minio.UploadInfo, error)
}

// putObjectWithRetry 带退避重试执行 PutObject（大文件时透传 PartSize 走 multipart）。
// minio 内部 multipart 失败自动 Abort（防孤儿）；此处只补应用层重试（网络抖动自愈）。
func putObjectWithRetry(ctx context.Context, client putter, bucket, key string, r io.Reader, size int64,
	opts minio.PutObjectOptions, retries int) error {
	var err error
	delay := multipartRetryBaseDelay
	for attempt := 0; attempt <= retries; attempt++ {
		// 每次重试需可重复读取的 reader：调用方保证传 bytes.Reader 等可重置源。
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
		}
		_, err = client.PutObject(ctx, bucket, key, r, size, opts)
		if err == nil {
			return nil
		}
	}
	return err
}
