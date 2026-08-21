// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"sync"
	"time"
)

// SignalKind 标识信令消息类型。
type SignalKind string

const (
	SignalOffer     SignalKind = "offer"
	SignalAnswer    SignalKind = "answer"
	SignalCandidate SignalKind = "candidate"
)

// SignalMsg 是经 hub 存转的一条信令消息。
type SignalMsg struct {
	ID   string     `json:"id,omitempty"` // 消息去重 ID（candidate 场景可选）
	Kind SignalKind `json:"kind"`
	From string     `json:"from"` // 发送方 peer
	To   string     `json:"to"`   // 目标 peer
	SDP  string     `json:"sdp,omitempty"`
	Cand string     `json:"cand,omitempty"`
	At   int64      `json:"at"` // 毫秒时间戳
}

// SignalQueue 是 per-peer 的信令收件箱（有界队列）。
// hub 收到 offer/answer/candidate 后 push 到目标 peer 的队列；
// 目标 peer 通过 poll 长轮询取走。
//
// 资源边界：per-peer 队列有上限（maxSignalInbox），并设全局消息总量上限
// （maxSignalTotal），防止大量空转 peer 的收件箱无界累积内存。
type SignalQueue struct {
	mu      sync.Mutex
	inboxes map[string][]SignalMsg   // peer -> 待消费消息
	waiters map[string]chan struct{} // peer -> 有新消息的信号
	total   int                      // 当前积压消息总数
}

// NewSignalQueue 创建信令队列。
func NewSignalQueue() *SignalQueue {
	return &SignalQueue{
		inboxes: make(map[string][]SignalMsg),
		waiters: make(map[string]chan struct{}),
	}
}

const (
	// maxSignalInbox 是单个 peer 收件箱的最大消息数。
	maxSignalInbox = 128
	// maxSignalTotal 是全局积压消息总数上限。
	maxSignalTotal = 1024
	// maxSignalWaiters 是 waiters 表的最大条目数（防空转 peer 无限注册 waiter）。
	maxSignalWaiters = 256
)

// Push 投递一条消息到目标 peer 的收件箱。
// 全局总量达上限时丢弃该消息（信令可重试，丢弃不致命）。
func (q *SignalQueue) Push(m SignalMsg) {
	q.mu.Lock()
	if q.total >= maxSignalTotal {
		q.mu.Unlock()
		return
	}
	inbox := q.inboxes[m.To]
	if len(inbox) >= maxSignalInbox {
		inbox = inbox[1:] // 单队列满丢弃最旧
		q.total--         // 补回被丢弃的一条的计数
	}
	q.inboxes[m.To] = append(inbox, m)
	q.total++
	if w := q.waiters[m.To]; w != nil {
		select {
		case w <- struct{}{}:
		default:
		}
	}
	q.mu.Unlock()
}

// Pop 非阻塞取走 target 收件箱里的一条消息；无则返回 nil。
func (q *SignalQueue) Pop(target string) *SignalMsg {
	q.mu.Lock()
	defer q.mu.Unlock()
	inbox := q.inboxes[target]
	if len(inbox) == 0 {
		return nil
	}
	m := inbox[0]
	q.inboxes[target] = inbox[1:]
	if len(inbox) == 1 {
		delete(q.inboxes, target) // 清空即删 map 项，防空 inbox 长期占内存
	}
	q.total--
	return &m
}

// Wait 阻塞等待 target 收件箱出现新消息（长轮询语义）。
// 返回后调用方应再 Pop 一次；有超时保护。
// ctx 取消时清理该 target 的 waiter，防止 waiters 表无界增长。
func (q *SignalQueue) Wait(ctx context.Context, target string) error {
	q.mu.Lock()
	// 若已有积压消息，立即返回
	if len(q.inboxes[target]) > 0 {
		q.mu.Unlock()
		return nil
	}
	w := q.waiters[target]
	if w == nil {
		// waiters 表已达上限：不再为新的 target 建 waiter，直接按超时处理
		if len(q.waiters) >= maxSignalWaiters {
			q.mu.Unlock()
			<-ctx.Done()
			return ctx.Err()
		}
		w = make(chan struct{}, 1)
		q.waiters[target] = w
	}
	q.mu.Unlock()

	select {
	case <-w:
		return nil
	case <-ctx.Done():
		// 清理该 target 的 waiter（若无新 waiter 覆盖）
		q.mu.Lock()
		if q.waiters[target] == w {
			delete(q.waiters, target)
		}
		q.mu.Unlock()
		return ctx.Err()
	}
}

// PollTimeout 是 poll 长轮询的单次最长等待。
const PollTimeout = 25 * time.Second

var _ = time.Second
