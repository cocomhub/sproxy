// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"testing"
	"time"
)

func TestSignalQueue_PushPop(t *testing.T) {
	q := NewSignalQueue()
	q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "b", SDP: "sdp-1"})
	q.Push(SignalMsg{Kind: SignalAnswer, From: "b", To: "a", SDP: "sdp-2"})

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
	q.Push(SignalMsg{Kind: SignalOffer, From: "x", To: "peer-a", SDP: "s"})

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
	// 塞满 + 多塞一条：不应 panic，最旧被丢弃
	for range maxSignalInbox + 10 {
		q.Push(SignalMsg{Kind: SignalOffer, From: "a", To: "peer", SDP: "s"})
	}
	count := 0
	for q.Pop("peer") != nil {
		count++
	}
	if count != maxSignalInbox {
		t.Fatalf("expected %d messages retained, got %d", maxSignalInbox, count)
	}
}
