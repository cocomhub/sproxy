// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// wait.go 把容量层（StorageManager）接入通用等待队列 pkg/waitq（设计
// docs/designs/2026-10-19-waitq.md §8.2「层 B 容量」）：
//
// 此前容量层只有 `TryReserve(n, cat)`（不足立即失败 ⇒ 调用方 507），与层 A 配额一致地
// 收敛为「**严格上界预占 + 不够时排队重试等待**」（用户裁定 2026-10-14）：
//
//	WaitReserve(ctx, n, cat, timeout) → 不足时排队（waitq）；释放唤醒 / ctx 取消 / 超时中断
//	Release(n, cat)                   → 释放后 NotifyAll 唤醒等待者（**释放侧唯一收口**）
//
// 丢唤醒闭合方式与 pkg/quota 一致：等待侧的「查 + 入队」在 `q.mu` 内完成
// （`waitq.AcquireUnder`），释放侧在同一把锁内改账 + 唤醒。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/waitq"
)

// DefaultWaitTimeout 是容量层等待的默认上限（与配额层同口径：30s）。
const DefaultWaitTimeout = 30 * time.Second

// ErrWaitTimeout 表示等待容量释放超时（包装 ErrStorageFull，调用方仍按容量不足处理）。
var ErrWaitTimeout = errors.New("capacity: 等待容量释放超时")

// waitQueue 是容量层的等待队列（懒建；**仅容量层**，与配额层各自独立 ⇒ 无跨层耦合）。
var (
	waitMu     sync.Mutex
	waitQueues = map[*StorageManager]*waitq.Queue{}
)

// queueFor 返回该 StorageManager 的等待队列（懒建；结构体化能力，不依赖全局可变态语义）。
func queueFor(m *StorageManager) *waitq.Queue {
	waitMu.Lock()
	defer waitMu.Unlock()
	if q, ok := waitQueues[m]; ok {
		return q
	}
	q := waitq.New(waitq.Options{DefaultWait: DefaultWaitTimeout, AgeBoost: DefaultWaitTimeout})
	waitQueues[m] = q
	return q
}

// capacityResource 是「某类别容量」的 waitq.Resource 适配器。
//
// Capacity/Used 取**全局**口径（容量上限与总占用都是 manager 级；类别只决定记账归属）
// ⇒ 与 TryReserve 的判据一致（它按总占用与 maxBytes 比较）。
type capacityResource struct {
	m   *StorageManager
	cat StorageCategory
}

// Name 实现 waitq.Resource（资源名 = 容量类别；R36-c 登记表见 internal/archcheck/waitq_resources.tsv）。
func (r capacityResource) Name() string {
	switch r.cat {
	case CategoryChunked:
		return "capacity:chunked"
	case CategoryCloud:
		return "capacity:cloud"
	default:
		return fmt.Sprintf("capacity:cat%d", int(r.cat))
	}
}

// Capacity 实现 waitq.Resource（0 = 无上限）。
func (r capacityResource) Capacity() int64 { return r.m.MaxBytes() }

// Used 实现 waitq.Resource。
func (r capacityResource) Used() int64 { return r.m.Usage() }

// TryAcquire 实现 waitq.Resource：调用既有 TryReserve（严格上界）；不足映射为 waitq.ErrInsufficient。
func (r capacityResource) TryAcquire(n int64) error {
	if err := r.m.TryReserve(n, r.cat); err != nil {
		if errors.Is(err, ErrStorageFull) {
			return waitq.ErrInsufficient
		}
		return err
	}
	return nil
}

// Release 实现 waitq.Resource（**回滚路径**）：释放并按类别记账。
// 走无锁版：waitq 的 tryAll 回滚发生在 AcquireUnder 的锁内 ⇒ 再取等待锁会自死锁。
func (r capacityResource) Release(n int64) { r.m.releaseNoLock(n, r.cat) }

// WaitReserve 预留 n 字节（类别 cat）；容量不足时**排队等待**（释放唤醒 / ctx 取消 / 超时）。
// timeout<=0 ⇒ DefaultWaitTimeout。成功返回 nil（额度已入账）；失败返回可识别错误
// （超时 ⇒ ErrWaitTimeout 包装 ErrStorageFull；取消 ⇒ ctx.Err()）。
//
// 语义与 pkg/quota 的 WaitReserve 一致：**零/负字节直通**（不排队）；不可能容纳（> 上限）
// ⇒ 立即失败（waitq.ErrTooLarge）。
func (m *StorageManager) WaitReserve(ctx context.Context, n int64, cat StorageCategory, timeout time.Duration) error {
	if m == nil {
		return errors.New("capacity: nil manager")
	}
	if n <= 0 {
		return nil // 零字节直通（与配额层同口径）
	}
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	q := queueFor(m)
	_, err := q.AcquireUnder(ctx, waitq.Request{
		Group:    []waitq.Resource{capacityResource{m: m, cat: cat}},
		N:        n,
		Priority: waitq.Normal,
		Timeout:  timeout,
		Label:    "capacity",
	}, &waitMu)
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, waitq.ErrTimeout):
		return fmt.Errorf("%w: %w", ErrWaitTimeout, ErrStorageFull)
	case errors.Is(err, waitq.ErrTooLarge):
		return fmt.Errorf("%w: 请求 %d 字节超过容量上限（即使全部释放也无法容纳，不排队）", ErrStorageFull, n)
	case errors.Is(err, waitq.ErrQueueFull):
		return fmt.Errorf("%w: %w", ErrStorageFull, err)
	default:
		return err
	}
}

// withWaitLock 在**容量等待锁**内执行 fn，随后唤醒等待者（释放侧唯一收口）。
// 锁序：waitMu → waitq 内部锁（与等待侧 AcquireUnder 同序 ⇒ 无死锁）。
func (m *StorageManager) withWaitLock(fn func()) {
	waitMu.Lock()
	defer waitMu.Unlock()
	fn()
	if q, ok := waitQueues[m]; ok && q != nil {
		q.NotifyAll()
	}
}
