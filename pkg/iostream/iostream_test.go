// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package iostream

import (
	"bytes"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNormalizeListenAddr(t *testing.T) {
	cases := []struct{ in, want string }{
		{":2222", "127.0.0.1:2222"},
		{"127.0.0.1:2222", "127.0.0.1:2222"},
		{"0.0.0.0:2222", "0.0.0.0:2222"},
		{"10.0.0.1:2222", "10.0.0.1:2222"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := NormalizeListenAddr(tc.in); got != tc.want {
			t.Fatalf("NormalizeListenAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestWriteFullPartialWrite(t *testing.T) {
	var buf bytes.Buffer
	short := &shortWriter{w: &buf, limit: 3}
	if err := WriteFull(short, []byte("hello world")); err != nil {
		t.Fatalf("WriteFull: %v", err)
	}
	if buf.String() != "hello world" {
		t.Fatalf("WriteFull 部分写未写满: %q", buf.String())
	}
	shortErr := &shortWriter{w: &buf, limit: 0, err: io.ErrShortWrite}
	if err := WriteFull(shortErr, []byte("x")); err != io.ErrShortWrite {
		t.Fatalf("应传播 ErrShortWrite, got %v", err)
	}
}

type shortWriter struct {
	w     *bytes.Buffer
	limit int
	err   error
}

func (s *shortWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	if len(p) > s.limit {
		p = p[:s.limit]
	}
	return s.w.Write(p)
}

func TestPumpHalfCloseKeepsInFlight(t *testing.T) {
	a, b := netPipePair(t)
	defer a.Close()
	defer b.Close()

	// a 写半段后 CloseWrite（半关闭）；pump 应传播，b 读到完整数据。
	go func() {
		_, _ = a.Write([]byte("ping"))
		_ = a.(*netTCPConn).CloseWrite()
	}()
	got := make(chan string, 1)
	go func() {
		buf, _ := io.ReadAll(b)
		got <- string(buf)
	}()
	select {
	case s := <-got:
		if s != "ping" {
			t.Fatalf("pump 读侧数据 = %q, want ping", s)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pump 未读到数据")
	}
}

func TestPumpNonCooperativeForceClose(t *testing.T) {
	// a 是真实 TCP 端：测试向对端写"ping"+CloseWrite 后 a 读到数据+EOF（g1 方向完成）。
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	serverCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := ls.Accept()
		if aerr != nil {
			return
		}
		serverCh <- c
	}()
	client, derr := net.Dial("tcp", ls.Addr().String())
	if derr != nil {
		t.Fatal(derr)
	}
	defer client.Close()
	server := <-serverCh
	defer server.Close()

	// b 是非合作端：Read 永久阻塞直到 Close，且 CloseWrite 是 no-op（不解读阻塞）——
	// 使 g2 方向在宽限期内持续阻塞，验证 Pump 超时路径强制关闭两端。
	b := &blockingEnd{closed: make(chan struct{})}
	defer b.Close()

	go func() {
		_, _ = server.Write([]byte("ping"))
		_ = server.(*net.TCPConn).CloseWrite()
	}()

	done := make(chan struct{})
	go func() {
		Pump(client, b, 100*time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Pump 未在宽限期后强制关闭返回（非合作对端应被强制释放）")
	}
}

// blockingEnd 是阻塞读、可写、CloseWrite no-op 的流端（构造非合作对端）。
type blockingEnd struct {
	mu      sync.Mutex
	written bytes.Buffer
	closed  chan struct{}
	once    sync.Once
}

func (e *blockingEnd) Read(_ []byte) (int, error) {
	<-e.closed
	return 0, io.EOF
}
func (e *blockingEnd) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.written.Write(p)
}
func (e *blockingEnd) Close() error {
	e.once.Do(func() { close(e.closed) })
	return nil
}
func (e *blockingEnd) CloseWrite() error { return nil }

// netTCPConn 包装 *net.TCPConn，暴露 CloseWrite（本包测试用最小类型）。
type netTCPConn struct {
	net.Conn
}

func (c *netTCPConn) CloseWrite() error {
	if tc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return tc.CloseWrite()
	}
	return nil
}

func netPipePair(t *testing.T) (io.ReadWriteCloser, io.ReadWriteCloser) {
	t.Helper()
	ls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cc, err := net.Dial("tcp", ls.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ls.Accept()
	if err != nil {
		t.Fatal(err)
	}
	return &netTCPConn{Conn: cc}, &netTCPConn{Conn: sc}
}
