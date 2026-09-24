// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"errors"
	"time"
)

// StateStore 是状态存储抽象：key → 不透明字节值。
//
// 实现约定（跨实现一致，见设计 2026-09-24-statestore.md §2.2）：
//   - Get：key 不存在返回 ErrKeyNotFound（与 os.ErrNotExist 语义对齐），
//     绝不返回 (nil, nil)；
//   - Put：原子写（tmp+rename 语义——本地实现；mongo 为 upsert 单文档），
//     成功返回后读必须看到新值（线性一致性，由 mongo 主节点或本地 rename 保证）；
//   - Delete：key 不存在静默成功（幂等）；
//   - List：返回 prefix 前缀下的完整 key 列表（排序不承诺稳定；调用方自行排序）；
//   - CAS：old 为 nil 表示「期望不存在」（create-only），new 为 nil 表示「期望删除」。
//     失败（当前值 ≠ old）返回 ErrCASMismatch。必须原子——禁止读-改-写非原子序列。
type StateStore interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Put(ctx context.Context, key string, data []byte) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
	CAS(ctx context.Context, key string, old, new []byte) error
}

// ErrKeyNotFound 是 Get 未命中的哨兵错误（各实现统一）。
var ErrKeyNotFound = errors.New("state: key not found")

// ErrCASMismatch 是 CAS 期望值不匹配的哨兵错误（调用方按 409/重试处理）。
var ErrCASMismatch = errors.New("state: CAS mismatch")

// AppendStore 是可追加存储（审计/事件）：Append 把 data 作为一条记录追加到 key 的尾部。
// 语义：线性追加、不覆盖历史；Read/List 由调用方按需读取（本接口不定义读取）。
type AppendStore interface {
	Append(ctx context.Context, key string, data []byte) error
}

// Change 是 Watch 发出的变更事件。
type Change struct {
	// Key 是变更的完整 key。
	Key string
	// Op 是 "put" | "delete"。
	Op string
	// Prev 是变更前的值（仅 put 且实现支持时填充；nil = 实现不提供）。
	Prev []byte
}

// WatchStore 是可观察存储：Watch 订阅 prefix 下的变更流。
//
// 实现约定：
//   - 初始不重放现有键（只推后续变更）——索引失效场景需要的是「后续写」；
//   - 通道由调用方负责消费；底层存储不可用时通道关闭（调用方应退避重连）；
//   - ctx 取消时关闭通道并返回。
type WatchStore interface {
	Watch(ctx context.Context, prefix string) (<-chan Change, error)
}

// LeaderElector 是选主抽象：写面唯一化的互斥租约（roadmap 11.11 方案 A 配套）。
// 本期（F1）只定义接口与哨兵错误；Local/Mongo 实现见后续片
// （2026-09-24-leader-elector.md F1-F3）。
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
var ErrLeaseLost = errors.New("state: lease lost")
