// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSignalQueue_PushPop(t *testing.T) {
	q := NewSignalQueue()
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "sdp-1"})
	_ = q.Push(SignalMsg{Kind: SignalAnswer, From: "b", To: "a", SDP: "sdp-2"})

	m := q.Pop("b")
	if m == nil || m.Kind != SignalOffer || m.SDP != "sdp-1" {
		t.Fatalf("unexpected first pop: %+v", m)
	}
	m = q.Pop("a")
	if m == nil || m.Kind != SignalAnswer || m.SDP != "sdp-2" {
		t.Fatalf("unexpected second pop: %+v", m)
	}
	if q.Pop("nobody") != nil {
		t.Fatal("expected nil for unknown peer")
	}
}

func TestSignalQueue_WaitWake(t *testing.T) {
	q := NewSignalQueue()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	// 并发等待 + 投递
	waitDone := make(chan error, 1)
	go func() {
		waitDone <- q.Wait(ctx, "peer-a")
	}()
	time.Sleep(50 * time.Millisecond)
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "x", To: "peer-a", SDP: "s"})

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait returned error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Wait not woken")
	}
	if q.Pop("peer-a") == nil {
		t.Fatal("expected message after wake")
	}
}

func TestSignalQueue_WaitSuccessCleansWaiter(t *testing.T) {
	q := NewSignalQueue()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	waitDone := make(chan error, 1)
	go func() {
		waitDone <- q.Wait(ctx, "peer-a")
	}()
	time.Sleep(50 * time.Millisecond)
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "x", To: "peer-a", SDP: "s"})

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait returned error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("Wait not woken")
	}
	// 成功唤醒路径应清理 waiter（I6/S10），否则 waiters 表只增不减。
	q.mu.Lock()
	n := len(q.waiters)
	q.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected waiter cleaned after successful wake, got %d", n)
	}
}

// TestSignalQueue_WaitTwoConcurrentWaiters_BothWake（P1-8 回归）：
// 同一 peer 的两个并发 Wait 必须都被一次 Push 唤醒——旧实现用单 waiter 通道且
// 唤醒即删 map 条目，第二个 waiter 被搁浅到 ctx 截止（最长 25s，可击穿 webrtc
// 30s 信令预算导致拨号超时失败）。
func TestSignalQueue_WaitTwoConcurrentWaiters_BothWake(t *testing.T) {
	q := NewSignalQueue()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	wait1 := make(chan error, 1)
	wait2 := make(chan error, 1)
	go func() { wait1 <- q.Wait(ctx, "peer-a") }()
	go func() { wait2 <- q.Wait(ctx, "peer-a") }()
	time.Sleep(50 * time.Millisecond) // 两个 waiter 都注册

	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "x", To: "peer-a", SDP: "s"})

	// 两个 waiter 都必须被唤醒（旧实现第二个搁浅到 ctx 截止）。
	select {
	case err := <-wait1:
		if err != nil {
			t.Fatalf("wait1 error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("wait1 未被唤醒")
	}
	select {
	case err := <-wait2:
		if err != nil {
			t.Fatalf("wait2 error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("wait2 未被唤醒（并发 waiter 搁浅 bug）")
	}
}

func TestSignalQueue_WaitTimeout(t *testing.T) {
	q := NewSignalQueue()
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := q.Wait(ctx, "nobody"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestSignalQueue_Overflow(t *testing.T) {
	q := NewSignalQueue()
	// 塞满 + 多塞一条：不应 panic，最旧被丢弃。
	// 每条用不同 From，避免 per-sender cap（maxSignalPerSender）干扰队列上限测试。
	for i := range maxSignalInbox + 10 {
		_ = q.Push(SignalMsg{Kind: SignalOffer, From: fmt.Sprintf("sender-%d", i), To: "peer", SDP: "s"})
	}
	count := 0
	for q.Pop("peer") != nil {
		count++
	}
	if count != maxSignalInbox {
		t.Fatalf("expected %d messages retained, got %d", maxSignalInbox, count)
	}
}

// TestSignalQueue_PopKind 验证按 kind 消费、不匹配消息保留（I9）。
func TestSignalQueue_PopKind(t *testing.T) {
	q := NewSignalQueue()
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "offer"})
	_ = q.Push(SignalMsg{Kind: SignalAnswer, From: "b", To: "a", SDP: "answer"})

	// PopKind 只取指定 kind，不匹配消息保留在队列
	m := q.PopKind("a", SignalAnswer)
	if m == nil || m.Kind != SignalAnswer || m.SDP != "answer" {
		t.Fatalf("unexpected answer pop: %+v", m)
	}
	if q.PopKind("a", SignalAnswer) != nil {
		t.Fatal("expected nil for consumed answer")
	}
	if q.PopKind("a", SignalOffer) != nil {
		t.Fatal("expected nil: peer-a inbox has no offer")
	}
	// peer-b 的 offer 未被 PopKind("a", ...) 触碰
	m = q.Pop("b")
	if m == nil || m.SDP != "offer" {
		t.Fatalf("expected offer preserved in peer-b inbox: %+v", m)
	}
}

// TestSignalQueue_Purge 验证节点下线时清空收件箱与 waiter（I6）。
func TestSignalQueue_Purge(t *testing.T) {
	q := NewSignalQueue()
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "peer", SDP: "s1"})
	_ = q.Push(SignalMsg{Kind: SignalAnswer, From: "b", To: "peer", SDP: "s2"})
	q.Purge("peer")
	if q.Pop("peer") != nil {
		t.Fatal("expected inbox purged")
	}
	q.mu.Lock()
	n := q.total
	q.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected total=0 after purge, got %d", n)
	}
	// 幂等：重复 Purge / 不存在的 peer 不 panic
	q.Purge("peer")
	q.Purge("nobody")
}

// TestSignalQueue_PushAssignsID 验证 Push 为无 ID 消息赋值去重 ID（I10）。
func TestSignalQueue_PushAssignsID(t *testing.T) {
	q := NewSignalQueue()
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "s"})
	m := q.Pop("b")
	if m == nil {
		t.Fatal("expected message")
	}
	if m.ID == "" {
		t.Fatal("expected Push to assign message ID")
	}
	// 显式 ID 保留
	_ = q.Push(SignalMsg{ID: "custom", Kind: SignalAnswer, From: "b", To: "a", SDP: "s2"})
	m2 := q.Pop("a")
	if m2 == nil || m2.ID != "custom" {
		t.Fatalf("expected custom ID preserved, got %+v", m2)
	}
}

// TestSignalQueue_PerSenderCap 验证同一 sender 的未消费消息达上限后拒绝（I9）。
func TestSignalQueue_PerSenderCap(t *testing.T) {
	q := NewSignalQueue()
	for i := range maxSignalPerSender {
		if err := q.Push(SignalMsg{Kind: SignalOffer, From: "flooder", To: "peer", SDP: "s"}); err != nil {
			t.Fatalf("unexpected push error at %d: %v", i, err)
		}
	}
	err := q.Push(SignalMsg{Kind: SignalOffer, From: "flooder", To: "peer", SDP: "s"})
	if !errors.Is(err, ErrSignalPerSenderCap) {
		t.Fatalf("expected ErrSignalPerSenderCap, got %v", err)
	}
	// 不同 sender 的消息不受影响
	if err := q.Push(SignalMsg{Kind: SignalOffer, From: "legit", To: "peer", SDP: "s"}); err != nil {
		t.Fatalf("unexpected push error for legit sender: %v", err)
	}
}

// TestSignalQueue_TTLExpired 验证过期消息被惰性清除（I9）。
func TestSignalQueue_TTLExpired(t *testing.T) {
	q := NewSignalQueue()
	old := time.Now().Add(-signalMsgTTL - time.Second).UnixMilli()
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "s", At: old})
	if m := q.Pop("b"); m != nil {
		t.Fatalf("expected expired message dropped, got %+v", m)
	}
	q.mu.Lock()
	n := q.total
	q.mu.Unlock()
	if n != 0 {
		t.Fatalf("expected total=0 after expired drop, got %d", n)
	}
}

// TestSignalQueue_PeekConfirm 验证 Peek 不删除、Confirm 按 ID 消费（I5 原语）。
func TestSignalQueue_PeekConfirm(t *testing.T) {
	q := NewSignalQueue()
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "s"})

	m := q.Peek("b", "")
	if m == nil || m.SDP != "s" {
		t.Fatalf("expected peek message: %+v", m)
	}
	// Peek 不删除：消息仍在
	if q.Pop("b") == nil {
		t.Fatal("expected message still present after Peek")
	}

	// Peek 后按 ID Confirm 删除
	_ = q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "s2"})
	m2 := q.Peek("b", SignalOffer)
	if m2 == nil {
		t.Fatal("expected peek after repush")
	}
	if !q.Confirm("b", m2.ID) {
		t.Fatalf("expected Confirm to succeed for id %q", m2.ID)
	}
	if q.Pop("b") != nil {
		t.Fatal("expected message removed after Confirm")
	}
	// 重复 Confirm 返回 false（已被删除）
	if q.Confirm("b", m2.ID) {
		t.Fatal("expected second Confirm to fail")
	}
}

// TestSignalQueue_GlobalTotalLimit 验证全局总量上限 maxSignalTotal（I52）。
// 分散到多个 peer 避免 maxSignalInbox 干扰；每 peer 用不同 sender 避免
// per-sender cap 干扰。
func TestSignalQueue_GlobalTotalLimit(t *testing.T) {
	q := NewSignalQueue()
	const peers = 64
	const perPeer = maxSignalTotal / peers // 16
	for p := range peers {
		for i := range perPeer {
			_ = q.Push(SignalMsg{Kind: SignalOffer, From: fmt.Sprintf("sender-%d-%d", p, i), To: fmt.Sprintf("peer-%d", p), SDP: "s"})
		}
	}
	err := q.Push(SignalMsg{Kind: SignalOffer, From: "new-sender", To: "peer-extra", SDP: "s"})
	if !errors.Is(err, ErrSignalQueueFull) {
		t.Fatalf("expected ErrSignalQueueFull, got %v", err)
	}
	// 已入队的消息仍可消费
	total := 0
	for p := range peers {
		for q.Pop(fmt.Sprintf("peer-%d", p)) != nil {
			total++
		}
	}
	if total != maxSignalTotal {
		t.Fatalf("expected %d retained, got %d", maxSignalTotal, total)
	}
}

// TestSignalQueue_WaitersLimit 验证 waiters 表上限 maxSignalWaiters（I52）：
// 表满后新 peer 的 Wait 不注册 waiter，直接等 ctx 超时返回。
func TestSignalQueue_WaitersLimit(t *testing.T) {
	q := NewSignalQueue()

	var wg sync.WaitGroup
	for i := range maxSignalWaiters {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			wctx, wcancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer wcancel()
			_ = q.Wait(wctx, fmt.Sprintf("peer-%d", id))
		}(i)
	}
	// 等待 waiters 表填满（持锁轮询避免 data race）
	deadline := time.Now().Add(3 * time.Second)
	for {
		q.mu.Lock()
		n := len(q.waiters)
		q.mu.Unlock()
		if n >= maxSignalWaiters {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiters not populated: got %d", n)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 第 257 个 peer 的 Wait：不注册 waiter，直接阻塞到 ctx 超时
	start := time.Now()
	wctx, wcancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer wcancel()
	err := q.Wait(wctx, "peer-over-limit")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded for over-limit waiter, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("expected over-limit wait to block until timeout, got %v", elapsed)
	}
	wg.Wait()
}
