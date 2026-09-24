// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package leader 提供选主（leader election）抽象：多节点共享存储下**写面节点唯一**。
//
// 问题（roadmap 11.11）：多节点挂同一外部卷时，每节点各自写本地 meta JSON，静默互相覆盖；
// 配额双账本、dedup 引用计数在跨进程下没有写面仲裁。LeaderElector 让写面唯一化——
// 仅主节点可写，从节点可读但拒绝写请求。
//
// 本期实现（F1，见 docs/designs/2026-09-24-leader-elector.md）：
//   - LeaderElector 接口：TryAcquire / Renew / Release；
//   - LocalLeaderElector：<root>/state/leader.lock 排他锁（Unix flock / Windows LockFileEx
//     双平台，**非回落恒主**）——单节点恒主零回归（无竞争者时 TryAcquire 恒 true）；
//   - WriteGuard：写面门（非主节点 Authorize() 拒绝，装配层映射 503 + Retry-After）；
//   - R23 门禁（internal/archcheck/flock_gate_test.go）：非测试源码禁直接 flock，
//     引导新代码走本抽象。
//
// MongoLeaderElector（TTL 租约）与写面装配属 F2/F3 后续片。
package leader

import (
	"context"
	"errors"
	"time"
)

// LeaderElector 是选主抽象：写面唯一化的互斥租约。
//
// 实现约定：
//   - TryAcquire：尝试获得租约；成功返回 (true, nil)；已被他人持有返回 (false, nil)（非错误）；
//     ttl <= 0 → 拒绝（调用方必须显式给 ttl，防「无限期持有」的隐式语义）；
//   - Renew：续租本 leaseID 持有的租约；未持有/租约已过期被他人接管 → 返回 ErrLeaseLost；
//   - Release：释放本 leaseID 持有的租约（幂等；未持有 no-op 成功）；
//   - leaseID 是调用方身份（如 node_id + 启动随机后缀）；空 → 拒绝。
type LeaderElector interface {
	TryAcquire(ctx context.Context, leaseID string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, leaseID string) error
	Release(ctx context.Context, leaseID string) error
}

// ErrLeaseLost 是续租失败（租约已过期被他人接管）的哨兵错误：
// 调用方必须停止一切写面操作并降级为只读（fail-closed）。
var ErrLeaseLost = errors.New("leader: lease lost")

// ErrNotLeader 是 WriteGuard.Authorize 的拒绝错误：当前节点非主节点（从节点只读）。
// 装配层映射为 503 Service Unavailable + Retry-After: 1（客户端可重试/重定向到主节点）。
var ErrNotLeader = errors.New("leader: not leader")
