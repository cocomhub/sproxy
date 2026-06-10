// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package xfertest 提供 xfer.Conn 的测试工具。
package xfertest

import (
	"context"
	"fmt"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// Pipe 创建一对通过内存管道连接的 xfer.Conn。
// 用于测试 mux 和 tunnel 层，无需真实网络。
func Pipe() (a, b xfer.Conn) {
	chAB := make(chan message, 256)
	chBA := make(chan message, 256)

	closeOnce := new(sync.Once)
	closeCh := make(chan struct{})

	return newPipe(chAB, chBA, closeCh, closeOnce), newPipe(chBA, chAB, closeCh, closeOnce)
}

type message struct {
	data []byte
	err  error
}

type pipeConn struct {
	rx        <-chan message
	tx        chan<- message
	closeCh   chan struct{}
	closeOnce *sync.Once
	mu        sync.Mutex
	closed    bool
}

func newPipe(rx <-chan message, tx chan<- message, closeCh chan struct{}, closeOnce *sync.Once) *pipeConn {
	return &pipeConn{rx: rx, tx: tx, closeCh: closeCh, closeOnce: closeOnce}
}

func (p *pipeConn) Send(ctx context.Context, msg []byte) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return xfer.ErrConnClosed
	}
	p.mu.Unlock()

	cp := make([]byte, len(msg))
	copy(cp, msg)
	select {
	case p.tx <- message{data: cp}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closeCh:
		return xfer.ErrConnClosed
	}
}

func (p *pipeConn) Receive(ctx context.Context) ([]byte, error) {
	select {
	case msg := <-p.rx:
		return msg.data, msg.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.closeCh:
		return nil, fmt.Errorf("pipe: %w", xfer.ErrConnClosed)
	}
}

func (p *pipeConn) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.closeOnce.Do(func() {
		close(p.closeCh)
	})
	return nil
}
