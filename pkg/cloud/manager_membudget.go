// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/integrity"
	"golang.org/x/sync/semaphore"
)

// newCheckMemSem 构造校验内存配额信号量；maxBytes<=0 返回 nil（配额禁用）。
func newCheckMemSem(maxBytes int64) *semaphore.Weighted {
	if maxBytes <= 0 {
		return nil
	}
	return semaphore.NewWeighted(maxBytes)
}

// acquireCheckMem 在 Check 前按估算内存占用排队获取配额（Weighted 信号量：
// 并发校验的总估算内存 ≤ MaxCheckMemBytes；不足等待释放）。返回 release 释放函数。
// 排序：FIFO？Weighted 不保证 FIFO——但按字节计数本身即「等待释放」语义，顺序
// 不关键（校验是短操作）。估算 <=0 → nil（无内存需求，直接执行）。
func (m *CloudDownloadManager) acquireCheckMem(ctx context.Context, est int64) func() {
	if m.checkMemSem == nil || est <= 0 {
		return func() {}
	}
	// 等待期间任务取消/删除（ctx=dlCtx 取消）则中止
	if err := m.checkMemSem.Acquire(ctx, est); err != nil {
		return func() {}
	}
	return func() { m.checkMemSem.Release(est) }
}

// checkMemEstimate 估算校验器一次 Check 的峰值内存占用（MemEstimator 可选接口；
// 未实现返回 0 = 无内存需求）。
func checkMemEstimate(c integrity.Checker, path string, size int64) int64 {
	return integrity.MemEstimateOf(c, path, size)
}

// checkMemOverQuote 判断估算是否超配额（> MaxCheckMemBytes）：超配额 → 跳过校验
// 标记 unverified（用户裁定：超配额直接标记，不排队不校验——单文件估算超总预算，
// 排队也无解）。m.checkMemMax 是构造时保存的总预算（Weighted 不暴露 size 查询）。
func (m *CloudDownloadManager) checkMemOverQuote(est int64) bool {
	if m.checkMemSem == nil || m.checkMemMax <= 0 || est <= 0 {
		return false
	}
	return est > m.checkMemMax
}
