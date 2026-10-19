// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cocomhub/sproxy/pkg/waitq"
)

// wait.go 实现**配额不足时的排队等待重试**（2026-10-14 用户裁定）：
// 「严格上界预占 + 不够时继续队列重试等待」——这样**真正放得下的文件最终一定成功**，而不是被一次 507
// 直接拒掉；同时因为预占是严格上界，「绝不打满磁盘/绝不超真实配额」仍由预留保证。
//
// 形态与既有 staging 排队（`staging_tracker.go`）一致：**释放广播唤醒 + 持锁 double-check（闭死丢唤醒
// 窗口）+ ctx 取消 / 超时中断**；区别是等待挂在**根节点**的队列上（任何一层释放都可能让等待者满足）。
//
// 超时上限（用户裁定）：默认 `DefaultWaitTimeout`（30s）；超时后返回**包装 ErrStorageFull** 的错误
// （调用方映射 507 + 提示「释放空间或申请提额」）。

// WaitRounds 返回本节点进入等待的轮次（可观测性/测试同步用）。
func (p *Pool) WaitRounds() int64 {
	if p == nil {
		return 0
	}
	return p.waitRounds.Load()
}

// DefaultWaitTimeout 是配额等待的默认上限（用户裁定 30s）。
const DefaultWaitTimeout = 30 * time.Second

// ErrWaitTimeout 表示等待配额释放超时（包装 ErrStorageFull，调用方仍按容量不足处理）。
var ErrWaitTimeout = errors.New("quota: 等待配额释放超时")

// WaitReserve 尝试预留 n 字节；容量不足时**排队等待**（释放唤醒 / ctx 取消 / 超时），
// 超时或取消才失败。timeout<=0 ⇒ DefaultWaitTimeout。
//
// 实现收敛：等待队列用通用能力包 `pkg/waitq`（设计 docs/designs/2026-10-19-waitq.md §8）；
// 「查 + 入队」在**根等待锁**下与释放侧的「改状态 + NotifyAll」串行 ⇒ 结构性无丢唤醒。
//
// 语义要点（与迁移前**逐条等价**，能力不丢失）：
//   - **严格上界**由 TryReserve 沿父链保证（适配器 TryAcquire 即 TryReserve）；
//   - 不可能容纳（n > 任一层「上限 − 运维余量」）⇒ **立即失败不排队**（waitq.ErrTooLarge
//     ⇒ 映射为包装 ErrStorageFull 的同一文案）；
//   - 非容量类错误（非法参数等）**立即返回**，不排队；
//   - 超时 ⇒ `ErrWaitTimeout` 包装 `ErrStorageFull`（调用方仍按容量不足处理 ⇒ 507）；
//   - 成功返回的 Reservation 与 TryReserve 完全一致（Commit/Release 语义相同）。
func (s *Scope) WaitReserve(ctx context.Context, n int64, timeout time.Duration) (*Reservation, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("quota: nil scope")
	}
	if timeout <= 0 {
		timeout = DefaultWaitTimeout
	}
	// **零/负字节直通**：waitq 要求 N>0（排队对零字节无意义），而既有语义是
	// `TryReserve(0)` 立即成功（空文件/mkdir 等路径会传 0）⇒ 必须原样保留（能力不丢失）：
	// 零 ⇒ 返回零预留；负 ⇒ 由 TryReserve 按其错误语义返回。
	if n <= 0 {
		return (&Scope{pool: s.pool}).TryReserve(n)
	}
	root := s.pool.rootPool()
	if root.queue == nil {
		return nil, fmt.Errorf("quota: 等待队列未初始化: %w", ErrStorageFull)
	}
	holder := "wq-" + strconv.FormatUint(root.waitSeq.Add(1), 10)
	res := poolResource{p: s.pool, holder: holder}

	_, err := root.queue.AcquireUnder(ctx, waitq.Request{
		Group:    []waitq.Resource{res},
		N:        n,
		Priority: waitq.Normal,
		Timeout:  timeout,
		Label:    "quota",
		Holder:   holder,
	}, &root.waitMu)
	if err != nil {
		res.p.dropHolder(holder, true) // 防御性清理（正常路径下适配器已无暂存）
		switch {
		case errors.Is(err, waitq.ErrTooLarge):
			return nil, fmt.Errorf("%w: 请求 %d 字节超过容量上限（即使全部释放也无法容纳，不排队）",
				ErrStorageFull, n)
		case errors.Is(err, waitq.ErrTimeout):
			return nil, fmt.Errorf("%w: %w", ErrWaitTimeout, ErrStorageFull)
		case errors.Is(err, waitq.ErrQueueFull):
			return nil, fmt.Errorf("%w: %w", ErrStorageFull, err)
		default:
			return nil, err // ctx 取消 / 资源错误：原样返回（既有契约）
		}
	}
	// 取回配额所有权（**不**调 lease.Release()：Lease 不承载配额语义，释放它反而会误释放）。
	if taken := res.take(); taken != nil {
		return taken, nil
	}
	// 理论上不可达：TryAcquire 成功必然暂存 Reservation。防御性返回明确错误而非 nil。
	return nil, fmt.Errorf("quota: 等待成功后未能取回预留（内部错误）: %w", ErrStorageFull)
}

// ProbeReserve 是**校验型**预留（探测式）：预留 n 后**立即释放**，只用于沿父链校验
// 「是否放得下」（副作用 = 校验租户根/全局上限），**不排队** —— 探测语义 ≠ 等待语义。
//
// 设计 docs/designs/2026-10-19-waitq.md §8.2「探测式」：明确为校验型 ⇒ 收敛为**具名 API**
// （不再让调用方散落「TryReserve + Release」两行 —— 漏掉 Release 会永久泄漏 reserved，
// 是真实的账本风险）。
func (s *Scope) ProbeReserve(n int64) error {
	if s == nil || s.pool == nil {
		return errors.New("quota: nil scope")
	}
	if n <= 0 {
		return nil // 零/负字节无需探测（与 WaitReserve 的零字节直通一致）
	}
	res, err := s.TryReserve(n)
	if err != nil {
		return err
	}
	res.Release()
	return nil
}

// ProbeReserve 是 Pool 侧的校验型探测（与 Scope.ProbeReserve 对称）：预留后立即释放，
// 只用于校验「是否放得下」（不排队）。
func (p *Pool) ProbeReserve(n int64) error {
	if p == nil {
		return errors.New("quota: nil pool")
	}
	return (&Scope{pool: p}).ProbeReserve(n)
}

// WaitReserve 是 Pool 侧的同一入口（在本节点上预留并排队等待）。
func (p *Pool) WaitReserve(ctx context.Context, n int64, timeout time.Duration) (*Reservation, error) {
	if p == nil {
		return nil, errors.New("quota: nil pool")
	}
	return (&Scope{pool: p}).WaitReserve(ctx, n, timeout)
}
