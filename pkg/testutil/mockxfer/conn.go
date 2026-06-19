// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockxfer

import (
	"context"
	"errors"
)

// MockConn 实现 xfer.Conn，可控 Send/Receive/Close 返回。
type MockConn struct {
	SendFn    func(ctx context.Context, msg []byte) error
	ReceiveFn func(ctx context.Context) ([]byte, error)
	CloseFn   func() error

	SendCalls    int
	ReceiveCalls int
	CloseCalls   int
}

func (m *MockConn) Send(ctx context.Context, msg []byte) error {
	m.SendCalls++
	if m.SendFn != nil {
		return m.SendFn(ctx, msg)
	}
	return nil
}

func (m *MockConn) Receive(ctx context.Context) ([]byte, error) {
	m.ReceiveCalls++
	if m.ReceiveFn != nil {
		return m.ReceiveFn(ctx)
	}
	return nil, nil
}

func (m *MockConn) Close() error {
	m.CloseCalls++
	if m.CloseFn != nil {
		return m.CloseFn()
	}
	return nil
}

func (m *MockConn) String() string { return "mockxfer.Conn" }

var (
	ErrSendFailed    = errors.New("mockxfer: send failed")
	ErrReceiveFailed = errors.New("mockxfer: receive failed")
)
