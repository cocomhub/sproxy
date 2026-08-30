// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockxfer

import (
	"context"
	"errors"
	"sync"
)

// MockConn 实现 xfer.Conn，可控 Send/Receive/Close 返回。
// 计数器用互斥保护：mux 可能从多 goroutine（调用方 + writeLoop/readLoop）并发
// 调用 Send/Receive/Close（-race 下 mock 自身不能有数据竞争）。
type MockConn struct {
	SendFn    func(ctx context.Context, msg []byte) error
	ReceiveFn func(ctx context.Context) ([]byte, error)
	CloseFn   func() error

	mu           sync.Mutex
	SendCalls    int
	ReceiveCalls int
	CloseCalls   int
}

func (m *MockConn) Send(ctx context.Context, msg []byte) error {
	m.mu.Lock()
	m.SendCalls++
	m.mu.Unlock()
	if m.SendFn != nil {
		return m.SendFn(ctx, msg)
	}
	return nil
}

func (m *MockConn) Receive(ctx context.Context) ([]byte, error) {
	m.mu.Lock()
	m.ReceiveCalls++
	m.mu.Unlock()
	if m.ReceiveFn != nil {
		return m.ReceiveFn(ctx)
	}
	return nil, nil
}

func (m *MockConn) Close() error {
	m.mu.Lock()
	m.CloseCalls++
	m.mu.Unlock()
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
