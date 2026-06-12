// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package xfertest 提供 xfer.Conn 的测试工具。
package xfertest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// message 是 pipeConn 信道上传输的内部消息类型。
type message struct {
	data []byte
}

// Pipe 创建一对内存中连接的 xfer.Conn，用于测试。
// 返回的两个 Conn 互为通信对端，数据通过有缓冲 channel 传递。
func Pipe() (a, b xfer.Conn) {
	chAB := make(chan message, 256)
	chBA := make(chan message, 256)

	var closeOnce sync.Once
	closeCh := make(chan struct{})

	return newPipe(chAB, chBA, closeCh, &closeOnce),
		newPipe(chBA, chAB, closeCh, &closeOnce)
}

func newPipe(rx, tx chan message, closeCh chan struct{}, closeOnce *sync.Once) *pipeConn {
	return &pipeConn{
		rx:        rx,
		tx:        tx,
		closeCh:   closeCh,
		closeOnce: closeOnce,
	}
}

type pipeConn struct {
	rx        chan message
	tx        chan message
	closeCh   chan struct{}
	closeOnce *sync.Once
	closed    atomic.Bool
}

func (p *pipeConn) Send(ctx context.Context, msg []byte) error {
	// Fast path: check context cancelled before sending.
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.closed.Load() {
		return xfer.ErrConnClosed
	}

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
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.closeCh:
		// If there's a buffered message, deliver it before returning closed.
		select {
		case m := <-p.rx:
			return m.data, nil
		default:
			return nil, xfer.ErrConnClosed
		}
	case m, ok := <-p.rx:
		if !ok {
			return nil, xfer.ErrConnClosed
		}
		return m.data, nil
	}
}

func (p *pipeConn) Close() error {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		close(p.closeCh)
	})
	return nil
}

func (p *pipeConn) String() string {
	return fmt.Sprintf("pipeConn{tx:%v, rx:%v}", p.tx, p.rx)
}
