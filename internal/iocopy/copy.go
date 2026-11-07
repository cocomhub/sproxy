// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package iocopy 提供 **ctx 感知的流拷贝**（`io.Copy` 的取消语义补充）。
package iocopy

// 背景：标准库 `io.Copy` 不感知 ctx——上游断开（客户端取消、请求超时）时仍会把整份
// 数据读完写完，浪费带宽/磁盘并延迟释放锁。历史上 pkg/files 与 pkg/server 各写过一份
// 逐字相同的 `copyWithContext`（连 buffer pool 都各自一份），靠一个源码级「两份实现必须
// 逐字一致」的守卫来防分叉。现在收敛为这里的唯一实现。

import (
	"context"
	"io"
	"sync"
)

// copyBufSize 是拷贝缓冲大小（32 KiB：与 os/io 默认量级一致，兼顾吞吐与单请求占用）。
const copyBufSize = 32 * 1024

// copyBufPool 复用拷贝缓冲（避免每请求分配 32 KiB）。
var copyBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, copyBufSize)
		return &b
	},
}

// CopyWithContext 在每次 Read/Write 前检查 ctx；取消时返回已写字节数与 ctx.Err()。
// 返回的 total 始终是**实际写入**的字节数（短写按实际计），便于调用方回滚/记账。
func CopyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	var total int64
	bufp, _ := copyBufPool.Get().(*[]byte) //nolint:errcheck // pool 无错误返回，断言防御
	if bufp == nil {
		buf := make([]byte, copyBufSize)
		bufp = &buf
	}
	defer copyBufPool.Put(bufp)
	buf := *bufp

	for {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		n, err := src.Read(buf)
		if n > 0 {
			nn, werr := dst.Write(buf[:n])
			total += int64(nn)
			if werr != nil {
				return total, werr
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}
