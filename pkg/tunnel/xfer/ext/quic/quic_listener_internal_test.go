// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/quic-go/quic-go"
)

// mockQUICListener 实现 quicListener 接口，用于 QuicListener 单元测试。
type mockQUICListener struct {
	addr       net.Addr
	acceptConn quic.Connection
	acceptErr  error
	closeErr   error
	closed     bool
}

func (m *mockQUICListener) Addr() net.Addr { return m.addr }

func (m *mockQUICListener) Accept(ctx context.Context) (quic.Connection, error) {
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

// stubConnection 是一个最小 quic.Connection 实现，只返回固定的 stream。
type stubConnection struct {
	stream    quic.Stream
	streamErr error
}

func (s *stubConnection) AcceptStream(ctx context.Context) (quic.Stream, error) {
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	return s.stream, nil
}

func (s *stubConnection) AcceptUniStream(ctx context.Context) (quic.ReceiveStream, error) {
	return nil, errors.New("not implemented")
}
func (s *stubConnection) OpenStream() (quic.Stream, error) {
	return nil, errors.New("not implemented")
}
func (s *stubConnection) OpenStreamSync(ctx context.Context) (quic.Stream, error) {
	return nil, errors.New("not implemented")
}
func (s *stubConnection) OpenUniStream() (quic.SendStream, error) {
	return nil, errors.New("not implemented")
}
func (s *stubConnection) OpenUniStreamSync(ctx context.Context) (quic.SendStream, error) {
	return nil, errors.New("not implemented")
}
func (s *stubConnection) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (s *stubConnection) RemoteAddr() net.Addr { return &net.TCPAddr{} }
func (s *stubConnection) CloseWithError(_ quic.ApplicationErrorCode, _ string) error {
	return nil
}
func (s *stubConnection) Context() context.Context              { return context.Background() }
func (s *stubConnection) ConnectionState() quic.ConnectionState { return quic.ConnectionState{} }
func (s *stubConnection) SendDatagram(_ []byte) error           { return errors.New("not implemented") }
func (s *stubConnection) ReceiveDatagram(_ context.Context) ([]byte, error) {
	return nil, errors.New("not implemented")
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
}

func TestQuicListenerAcceptSuccess(t *testing.T) {
	ms := &mockStream{}
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

	conn.Close()
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
