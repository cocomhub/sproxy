// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockxfer

import (
	"context"
	"errors"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// MockListener 实现 xfer.Listener，可控 Accept/Close 返回。
type MockListener struct {
	AcceptFn func(ctx context.Context) (xfer.Conn, error)
	CloseFn  func() error

	addr        string
	AcceptCalls int
	CloseCalls  int
}

func NewMockListener(addr string) *MockListener {
	return &MockListener{addr: addr}
}

func (l *MockListener) Accept(ctx context.Context) (xfer.Conn, error) {
	l.AcceptCalls++
	if l.AcceptFn != nil {
		return l.AcceptFn(ctx)
	}
	return nil, context.Canceled
}

func (l *MockListener) Close() error {
	l.CloseCalls++
	if l.CloseFn != nil {
		return l.CloseFn()
	}
	return nil
}

func (l *MockListener) Addr() string { return l.addr }

var ErrAcceptFailed = errors.New("mockxfer: accept failed")
