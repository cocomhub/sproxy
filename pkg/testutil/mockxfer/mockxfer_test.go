// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockxfer_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func TestMockConn_Send(t *testing.T) {
	m := &mockxfer.MockConn{}
	if err := m.Send(context.Background(), []byte("hello")); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if m.SendCalls != 1 {
		t.Fatalf("expected 1 Send call, got %d", m.SendCalls)
	}
}

func TestMockConn_SendError(t *testing.T) {
	m := &mockxfer.MockConn{
		SendFn: func(_ context.Context, _ []byte) error {
			return mockxfer.ErrSendFailed
		},
	}
	if err := m.Send(context.Background(), []byte("x")); err != mockxfer.ErrSendFailed {
		t.Fatalf("expected ErrSendFailed, got %v", err)
	}
}

func TestMockConn_Receive(t *testing.T) {
	m := &mockxfer.MockConn{
		ReceiveFn: func(_ context.Context) ([]byte, error) {
			return []byte("pong"), nil
		},
	}
	got, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("expected 'pong', got %q", got)
	}
}

func TestMockConn_DoubleClose(t *testing.T) {
	m := &mockxfer.MockConn{}
	if err := m.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	if m.CloseCalls != 2 {
		t.Fatalf("expected 2 Close calls, got %d", m.CloseCalls)
	}
}

func TestMockListener_Accept(t *testing.T) {
	l := mockxfer.NewMockListener("pipe://addr")
	conn, err := l.Accept(context.Background())
	if err == nil {
		conn.Close()
		t.Fatal("expected error from default Accept")
	}
	if l.AcceptCalls != 1 {
		t.Fatalf("expected 1 Accept call, got %d", l.AcceptCalls)
	}
}

func TestMockListener_CustomAccept(t *testing.T) {
	mc := &mockxfer.MockConn{}
	l := mockxfer.NewMockListener("test")
	l.AcceptFn = func(_ context.Context) (xfer.Conn, error) {
		return mc, nil
	}
	got, err := l.Accept(context.Background())
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	if got != mc {
		t.Fatal("unexpected conn returned")
	}
}

func TestMockListener_Addr(t *testing.T) {
	l := mockxfer.NewMockListener("tcp://127.0.0.1:9000")
	if addr := l.Addr(); addr != "tcp://127.0.0.1:9000" {
		t.Fatalf("expected addr 'tcp://127.0.0.1:9000', got %q", addr)
	}
}
