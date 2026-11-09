// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// upload_gate.go —— **G2：每卷上传并发闸**（复用 `pkg/waitq`，不新造排队原语；R36-b 禁 `sync.Cond`）。
//
// 为什么需要：并发上传无闸门时并发数会**叠乘** —— 上层块级并发（secretdata 默认 4）× 上传内核
// 分片并发（默认 4）= 单卷 16 并发请求，多卷再叠 ⇒ 触发百度风控（迁移期实测过 31034）。
// 闸门限制的是「**同时进行的上传动作**」数（每次真正上传占 1 槽），分片并发仍由内核
// `Options.Concurrency` 决定；两者相乘即单卷并发请求上限（默认 4×4=16）。
//
// 排队语义（全部复用 `waitq`）：有界可选（`MaxWaiters`）、`AgeBoost` 反饿死、ctx 可中断；
// **`Lease.Release()` 不负责唤醒**（waitq I1）⇒ 释放在闭包里 `NotifyAll`。

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/waitq"
)

// 上传并发闸的语义常量（`StorageConfig.UploadConcurrency`）：
// **缺省（0/负数）⇒ 不启用闸门**（保持既有并发行为，零副作用）；显式配置 1..32 才启用。
const (
	// RecommendedUploadConcurrency 是**建议**的每卷「同时进行的上传动作」数（与内核分片并发同量级）。
	// 它是给运维的推荐值，**不是**缺省行为（缺省不启用）。
	RecommendedUploadConcurrency = 4
	// MaxUploadConcurrency 是配置上限（防误配成无界并发而触发风控）。
	MaxUploadConcurrency = 32
	// uploadGateDefaultWait 是等待闸门的默认上限（ctx 取消仍即时生效）。
	uploadGateDefaultWait = 10 * time.Minute
)

// uploadGateResourceName 是等待队列中的资源名（受控枚举，供指标标签/排障）。
const uploadGateResourceName = "baidupcs_upload_slot"

// normalizeUploadConcurrency 归一化配置：**<=0 ⇒ 0（不启用闸门）**；超上限 ⇒ 钳到上限。
//
// 为什么缺省不启用（用户裁定 2026-11-09）：闸门是"限流"能力，缺省启用会给既有部署引入
// **新的并发上限副作用**（且上限值需按部署规模调参）；缺省保持既有行为 ⇒ 显式配置才限流。
// 需要防风控的部署按 `RecommendedUploadConcurrency`（或按规模）显式配置。
func normalizeUploadConcurrency(n int) int {
	switch {
	case n <= 0:
		return 0
	case n > MaxUploadConcurrency:
		return MaxUploadConcurrency
	default:
		return n
	}
}

// uploadSlotResource 是「每卷上传槽位」的 `waitq.Resource` 适配器
// （`Capacity`/`Used` 供队列判定可满足性，`TryAcquire` 真正占用；两者必须同源）。
type uploadSlotResource struct {
	limit int64
	used  atomic.Int64
}

// Name 实现 waitq.Resource（唯一标识 + 多资源排序键）。
func (r *uploadSlotResource) Name() string { return uploadGateResourceName }

// Capacity 是当前可发放上限。
func (r *uploadSlotResource) Capacity() int64 { return r.limit }

// Used 是当前已占用槽位。
func (r *uploadSlotResource) Used() int64 { return r.used.Load() }

// TryAcquire 原子占用 n 个槽位；不足返回 `waitq.ErrInsufficient`（由队列排队等待）。
func (r *uploadSlotResource) TryAcquire(n int64) error {
	for {
		cur := r.used.Load()
		if cur+n > r.limit {
			return waitq.ErrInsufficient
		}
		if r.used.CompareAndSwap(cur, cur+n) {
			return nil
		}
	}
}

// Release 释放 n 个槽位（**不唤醒**：唤醒由调用方 `NotifyAll`，见 waitq I1）。
func (r *uploadSlotResource) Release(n int64) { r.used.Add(-n) }

// newUploadGate 构造闸门（limit 须已归一化）；**limit <= 0 ⇒ 不启用**（返回 nil,nil，
// 由 `acquireUploadSlot` 走 no-op 分支）。
func newUploadGate(limit int) (*waitq.Queue, *uploadSlotResource) {
	if limit <= 0 {
		return nil, nil
	}
	res := &uploadSlotResource{limit: int64(limit)}
	q := waitq.New(waitq.Options{DefaultWait: uploadGateDefaultWait})
	return q, res
}

// acquireUploadSlot 获取一个上传槽位，返回**释放函数**（幂等且含唤醒）。
// 未装配闸门（极简构造/部分测试）⇒ 返回 no-op，不限制并发。
func (s *Storage) acquireUploadSlot(ctx context.Context, key string) (func(), error) {
	if s == nil || s.uploadGate == nil || s.uploadSlots == nil {
		return func() {}, nil
	}
	lease, err := s.uploadGate.Acquire(ctx, waitq.Request{
		Group:    []waitq.Resource{s.uploadSlots},
		N:        1,
		Priority: waitq.Normal,
		Label:    "upload",
		Holder:   key,
	})
	if err != nil {
		return nil, fmt.Errorf("baidupcs: 等待上传并发闸（每卷上限 %d）: %w", s.uploadLimit, err)
	}
	return func() {
		lease.Release()
		s.uploadGate.NotifyAll() // waitq I1：Release 不负责唤醒
	}, nil
}

// UploadGateStats 返回上传闸门统计（等待者数/获取/超时/取消）与当前上限，供指标与排障。
func (s *Storage) UploadGateStats() (waitq.Stats, int) {
	if s == nil || s.uploadGate == nil {
		return waitq.Stats{}, 0
	}
	return s.uploadGate.Stats(), s.uploadLimit
}
