// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"time"
)

// AuditFilter 是 AuditRing.Recent 的过滤条件。
// 空字段不过滤；多个字段同时满足（AND）；Since 保留 evt.TS.After(since) 的事件。
type AuditFilter struct {
	Action string
	Actor  string
	Mesh   string
	Since  time.Time // 零值不过滤
}

// defaultRecentLimit 是 Recent 的 limit<=0 防御兜底条数（handler 已 clamp，此处仅兜底）。
const defaultRecentLimit = 50

// AuditRing 是有界内存环形审计缓冲：保存最近 N 条 AuditEvent，容量满后环形覆盖
// 丢弃最旧。thread-safe（sync.RWMutex：Add 写锁、Recent/Len 读锁）。
//
// 与分享/云任务的内存存储模式一致，**不落盘**——审计留档交给日志 collector 消费
// RecordAudit 输出的 JSON 行，本 ring 仅供 Web UI /api/audit 快速回看最近操作。
type AuditRing struct {
	mu       sync.RWMutex
	buf      []AuditEvent
	head     int // 下一个写入位置（环形游标）
	size     int // 当前有效条数
	capacity int // 容量上限（0 = 禁用 no-op）
}

// NewAuditRing 创建容量为 capacity 的环形审计缓冲。
// capacity <= 0 时返回可用的 no-op ring（Add/Recent/Len 均安全返回空语义，
// 调用方无需判空；上层装配时仍按 cfg.Audit.BufferSize > 0 才挂到 Handlers.auditRing）。
func NewAuditRing(capacity int) *AuditRing {
	if capacity <= 0 {
		return &AuditRing{capacity: 0}
	}
	return &AuditRing{
		buf:      make([]AuditEvent, capacity),
		capacity: capacity,
	}
}

// Disabled 报告 ring 是否为禁用态（capacity<=0 的 no-op）。
func (r *AuditRing) Disabled() bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.capacity <= 0
}

// Add 追加一条审计事件到 ring。容量满时覆盖最旧（环形覆盖）。no-op ring 静默丢弃。
// 永不 panic——ring 为 nil 时直接返回（Handlers.auditRing 可能为 nil）。
func (r *AuditRing) Add(evt AuditEvent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.capacity <= 0 {
		return
	}
	if len(r.buf) == 0 {
		r.buf = make([]AuditEvent, r.capacity)
	}
	r.buf[r.head] = evt
	r.head = (r.head + 1) % r.capacity
	if r.size < r.capacity {
		r.size++
	}
}

// Recent 按时间倒序（最新在前）返回至多 limit 条按 f 过滤后的审计事件。
// limit<=0 时用默认兜底 defaultRecentLimit（handler 已 clamp，此处仅防御）。
// 返回的切片是拷贝，不持有内部锁。
func (r *AuditRing) Recent(limit int, f AuditFilter) []AuditEvent {
	if r == nil {
		return []AuditEvent{}
	}
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.capacity <= 0 || r.size == 0 {
		return []AuditEvent{}
	}

	out := make([]AuditEvent, 0, min(limit, r.size))
	// 时间倒序遍历：最新的有效元素下标是 (head-1+capacity)%capacity，逆序走 size 个。
	idx := (r.head - 1 + r.capacity) % r.capacity
	for i := 0; i < r.size && len(out) < limit; i++ {
		evt := r.buf[idx]
		if auditMatches(evt, f) {
			out = append(out, evt)
		}
		if idx == 0 {
			idx = r.capacity - 1
		} else {
			idx--
		}
	}
	return out
}

// Len 返回当前有效条数（no-op ring 返回 0）。
func (r *AuditRing) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.size
}

// Capacity 返回容量上限（no-op ring 返回 0）。
func (r *AuditRing) Capacity() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.capacity
}

// auditMatches 判断事件是否满足过滤条件（空字段不过滤；Since 保留 TS.After(since)）。
func auditMatches(evt AuditEvent, f AuditFilter) bool {
	if f.Action != "" && evt.Action != f.Action {
		return false
	}
	if f.Actor != "" && evt.Actor != f.Actor {
		return false
	}
	if f.Mesh != "" && evt.Mesh != f.Mesh {
		return false
	}
	if !f.Since.IsZero() && !evt.TS.After(f.Since) {
		return false
	}
	return true
}
