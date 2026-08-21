// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"golang.org/x/sync/errgroup"
)

// DownloadItem 表示一个批量下载条目。
type DownloadItem struct {
	// RemotePath 服务端文件路径。
	RemotePath string
	// LocalPath 本地保存路径。为空时使用 RemotePath 的 basename。
	LocalPath string
}

// DownloadOption 批量下载选项函数。
type DownloadOption func(*downloadItemsOptions)

type downloadItemsOptions struct {
	concurrency int
}

// WithDownloadConcurrency 设置最大并发下载数。
// 0 表示不限制；1 表示顺序下载；N 表示最多 N 个并发下载。
// 未指定时默认并发 2。
func WithDownloadConcurrency(n int) DownloadOption {
	return func(o *downloadItemsOptions) {
		if n >= 0 {
			o.concurrency = n
		}
	}
}

func defaultDownloadItemsOptions() downloadItemsOptions {
	return downloadItemsOptions{concurrency: 2}
}

// DownloadItems 批量下载多个文件。
//
// 并发控制：concurrency=0 表示不限制，concurrency=1 顺序下载，
// concurrency=N 最多 N 个并发。默认并发 2（避免全部并发对服务端与
// 本地 IO 造成过大压力，也不至于串行太慢）。
// 单文件失败不影响其余文件；全部完成后将所有失败信息合并为一个错误返回。
func (c *FileClient) DownloadItems(ctx context.Context, items []DownloadItem, opts ...DownloadOption) error {
	cfg := defaultDownloadItemsOptions()
	for _, o := range opts {
		o(&cfg)
	}
	if len(items) == 0 {
		return nil
	}

	limit := cfg.concurrency
	if limit <= 0 {
		limit = len(items) // 不限制：上限等于条目数
	}
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(limit)

	// 错误聚合：errgroup.Wait 只返回第一个错误，用带锁的收集器累积全部失败，
	// 与"失败信息合并后一并返回"的文档承诺一致。
	var mu sync.Mutex
	var errs []error
	for _, item := range items {
		eg.Go(func() error {
			if item.RemotePath == "" {
				err := fmt.Errorf("远程路径为空")
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return nil
			}
			local := item.LocalPath
			if local == "" {
				local = filepath.Base(item.RemotePath)
			}
			if err := c.Download(egCtx, item.RemotePath, local); err != nil {
				wrapped := fmt.Errorf("下载 %s 失败: %w", item.RemotePath, err)
				mu.Lock()
				errs = append(errs, wrapped)
				mu.Unlock()
			}
			return nil
		})
	}
	// 等待全部完成；不直接返回 errgroup 错误（首个错误），而返回聚合后的完整错误
	_ = eg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// DownloadItemsSequential 顺序下载多个文件。
// 等价于 DownloadItems(items, WithDownloadConcurrency(1))。
func (c *FileClient) DownloadItemsSequential(ctx context.Context, items []DownloadItem) error {
	return c.DownloadItems(ctx, items, WithDownloadConcurrency(1))
}
