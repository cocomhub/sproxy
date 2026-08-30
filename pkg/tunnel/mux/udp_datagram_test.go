// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// TestDatagram_Bidirectional：UDP 数据报帧双向传输——dialer 发数据报、listener 收到
// （flowID+负载正确），listener 回数据报、dialer 收到。验证消息边界保持。
func TestDatagram_Bidirectional(t *testing.T) {
	dm, lm := newMuxPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dmRecv := make(chan [2]any, 4) // [flowID, data]
	lmRecv := make(chan [2]any, 4)
	dm.SetDatagramHandler(func(flowID uint32, data []byte) {
		dmRecv <- [2]any{flowID, string(data)}
	})
	lm.SetDatagramHandler(func(flowID uint32, data []byte) {
		lmRecv <- [2]any{flowID, string(data)}
	})

	// dialer → listener。
	if err := dm.SendDatagram(7, []byte("hello-datagram")); err != nil {
		t.Fatalf("dialer SendDatagram: %v", err)
	}
	select {
	case got := <-lmRecv:
		if got[0] != uint32(7) || got[1] != "hello-datagram" {
			t.Fatalf("listener 收到 %v, want [7 hello-datagram]", got)
		}
	case <-ctx.Done():
		t.Fatal("listener 未收到数据报")
	}

	// listener → dialer。
	if err := lm.SendDatagram(3, []byte("response")); err != nil {
		t.Fatalf("listener SendDatagram: %v", err)
	}
	select {
	case got := <-dmRecv:
		if got[0] != uint32(3) || got[1] != "response" {
			t.Fatalf("dialer 收到 %v, want [3 response]", got)
		}
	case <-ctx.Done():
		t.Fatal("dialer 未收到数据报")
	}
}

// TestDatagram_MaxPayload：超限数据报报错（ErrDatagramTooLarge），不 panic。
func TestDatagram_MaxPayload(t *testing.T) {
	dm, _ := newMuxPair(t)
	big := make([]byte, mux.MaxDatagramPayload+1)
	if err := dm.SendDatagram(0, big); err != mux.ErrDatagramTooLarge {
		t.Fatalf("SendDatagram 超限 = %v, want ErrDatagramTooLarge", err)
	}
	// 边界值可通过。
	if err := dm.SendDatagram(0, make([]byte, mux.MaxDatagramPayload)); err != nil {
		t.Fatalf("SendDatagram 边界值: %v", err)
	}
}
