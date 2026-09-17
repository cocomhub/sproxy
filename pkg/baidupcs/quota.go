// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"fmt"
	"sync"
)

// Quota 是本地上传暂存（staging/cache）的配额挂钩。
//
// 用户硬约束：上传/下载的中间状态只依赖本地文件系统。为防止本地磁盘被
// staging/cache 占满，这些目录的写入计入配额（预留 → 传输完成释放）。
// 本实现为进程内内存计数（P2 基础版）；P4 与 sproxy pkg/quota.Scope 融合时
// 替换为 owner 配额池。
type Quota struct {
	mu     sync.Mutex
	used   int64
	limit  int64 // 上限字节；0 = 不限
	releas chan int64
}

// NewQuota 创建配额（limit 字节；0 = 不限）。
func NewQuota(limit int64) *Quota {
	return &Quota{limit: limit, releas: make(chan int64, 16)}
}

// Reserve 预留 size 字节。超限返回错误（调用方应暂停/拒绝继续写 staging）。
func (q *Quota) Reserve(size int64) error {
	if size <= 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit > 0 && q.used+size > q.limit {
		return fmt.Errorf("baidupcs: quota exceeded (used %d + %d > limit %d)", q.used, size, q.limit)
	}
	q.used += size
	return nil
}

// Release 释放 size 字节（传输完成/失败清理时调用）。
func (q *Quota) Release(size int64) {
	if size <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.used < size {
		q.used = 0
		return
	}
	q.used -= size
}

// Used 返回当前已用字节。
func (q *Quota) Used() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used
}

// Limit 返回上限（0 = 不限）。
func (q *Quota) Limit() int64 { return q.limit }
