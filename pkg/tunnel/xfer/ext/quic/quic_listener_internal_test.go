// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// mockQUICListener 实现 quicListener 接口，用于 QuicListener 单元测试。
type mockQUICListener struct {
	addr       net.Addr
	acceptConn connInterface
	acceptErr  error
	closeErr   error
	closed     bool
}

func (m *mockQUICListener) Addr() net.Addr { return m.addr }

func (m *mockQUICListener) Accept(ctx context.Context) (connInterface, error) {
	// 先检查 context 是否已取消，避免 goroutine 完成过快导致
	// select 在 ch、ctx.Done()、closeCh 之间竞态。
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if m.acceptErr != nil {
		return nil, m.acceptErr
	}
	if m.acceptConn == nil {
		return nil, errors.New("no connection available")
	}
	return m.acceptConn, nil
}

func (m *mockQUICListener) Close() error {
	m.closed = true
	return m.closeErr
}

// stubConnection 是一个最小 connInterface 实现，只返回固定的 stream。
type stubConnection struct {
	stream    streamInterface
	streamErr error
	closeErr  error
	closed    bool
}

func (s *stubConnection) AcceptStream(ctx context.Context) (streamInterface, error) {
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	return s.stream, nil
}

func (s *stubConnection) Close() error {
	s.closed = true
	return s.closeErr
}

func TestQuicListenerAddr(t *testing.T) {
	expectedAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000}
	mln := &mockQUICListener{addr: expectedAddr}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	got := ln.Addr()
	if got != expectedAddr.String() {
		t.Fatalf("expected addr %q, got %q", expectedAddr.String(), got)
	}
}

func TestQuicListenerClose(t *testing.T) {
	mln := &mockQUICListener{addr: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000}}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	if err := ln.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if !mln.closed {
		t.Fatal("underlying listener should be closed")
	}

	// closeCh must be closed
	select {
	case _, ok := <-ln.closeCh:
		if ok {
			t.Fatal("closeCh should be closed, but received value")
		}
	default:
		t.Fatal("closeCh should be closed, but it's still open")
	}

	// 幂等 Close：第二次调用不应 panic。
	if err := ln.Close(); err != nil {
		t.Fatalf("second close failed: %v", err)
	}
}

func TestQuicListenerAcceptSuccess(t *testing.T) {
	ms := &mockStream{}
	// Accept 会读取并校验 Dial 侧发送的流宣告魔数，先放入读缓冲。
	ms.readBuf.WriteString(announceMagic)
	mconn := &stubConnection{stream: ms}
	mln := &mockQUICListener{
		addr:       &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9001},
		acceptConn: mconn,
	}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	ctx := context.Background()
	conn, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("accept failed: %v", err)
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}

	// 验证返回的 conn 是 *quicConn 并封装正确的 stream
	qc, ok := conn.(*quicConn)
	if !ok {
		t.Fatalf("expected *quicConn, got %T", conn)
	}
	if qc.stream != ms {
		t.Fatal("conn should wrap the mock stream")
	}
	// 宣告魔数不得被当作业务消息：Accept 后首个 Receive 应读到对端首条业务消息。
	if got := ms.readBuf.Len(); got != 0 {
		t.Fatalf("announce magic should be consumed by Accept, %d bytes left", got)
	}

	conn.Close()
}

// TestQuicListenerAccept_InvalidAnnounce 校验首帧非宣告魔数时 Accept 返回明确错误，
// 并关闭该 QUIC 连接与流（不静默丢弃数据、不残留连接）。
func TestQuicListenerAccept_InvalidAnnounce(t *testing.T) {
	tests := []struct {
		name  string
		first []byte
	}{
		{"合法空消息（旧的零长度帧）", []byte{0, 0, 0, 0}},
		{"普通消息首帧", []byte{0, 0, 0, 5, 'h', 'e', 'l', 'l', 'o'}},
		{"魔数被篡改", []byte("SPROXYQ2")},
		{"读到 EOF（无任何数据）", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := &mockStream{}
			ms.readBuf.Write(tt.first)
			mconn := &stubConnection{stream: ms}
			mln := &mockQUICListener{
				addr:       &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9003},
				acceptConn: mconn,
			}
			ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

			conn, err := ln.Accept(context.Background())
			if err == nil {
				conn.Close()
				t.Fatal("expected error for non-magic first frame")
			}
			if !mconn.closed {
				t.Fatal("QUIC connection should be closed on announce failure")
			}
			if !ms.closed {
				t.Fatal("stream should be closed on announce failure")
			}
		})
	}
}

// TestQuicListenerClose_ClosesAccepted 校验 listener Close 时已 accept 的连接
// 与流被一并关闭（不残留到空闲超时）。
func TestQuicListenerClose_ClosesAccepted(t *testing.T) {
	ms := &mockStream{}
	ms.readBuf.WriteString(announceMagic)
	mconn := &stubConnection{stream: ms}
	mln := &mockQUICListener{
		addr:       &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9006},
		acceptConn: mconn,
	}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	conn, err := ln.Accept(context.Background())
	if err != nil {
		t.Fatalf("accept failed: %v", err)
	}

	if err := ln.Close(); err != nil {
		t.Fatalf("listener close failed: %v", err)
	}
	if !ms.closed {
		t.Fatal("accepted stream should be closed on listener Close")
	}
	if !mconn.closed {
		t.Fatal("accepted QUIC connection should be closed on listener Close")
	}

	// Accept 返回的 conn 随 listener 一并关闭，再次 Close 幂等。
	if err := conn.Close(); err != nil {
		t.Fatalf("idempotent close failed: %v", err)
	}
}

func TestQuicListenerAccept_InternalError(t *testing.T) {
	mln := &mockQUICListener{
		addr:      &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9002},
		acceptErr: errors.New("simulated accept error"),
	}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	ctx := context.Background()
	conn, err := ln.Accept(ctx)
	if err == nil {
		conn.Close()
		t.Fatal("expected error from Accept")
	}
}

func TestQuicListenerAccept_StreamError(t *testing.T) {
	mln := &mockQUICListener{
		addr:       &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9003},
		acceptConn: &stubConnection{streamErr: errors.New("simulated stream error")},
	}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	ctx := context.Background()
	conn, err := ln.Accept(ctx)
	if err == nil {
		conn.Close()
		t.Fatal("expected error from AcceptStream")
	}
}

func TestQuicListenerAcceptAfterClose(t *testing.T) {
	mln := &mockQUICListener{addr: &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9004}}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	ln.Close()

	ctx := context.Background()
	_, err := ln.Accept(ctx)
	if err == nil {
		t.Fatal("expected error when accepting on closed listener")
	}
	if !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("expected ErrConnClosed, got %v", err)
	}
}

func TestQuicListenerAcceptContextCancel(t *testing.T) {
	mln := &mockQUICListener{
		addr:       &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9005},
		acceptConn: &stubConnection{stream: &mockStream{}},
	}
	ln := &QuicListener{ln: mln, closeCh: make(chan struct{})}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ln.Accept(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}
