// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package p2p_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/p2p"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

var (
	registerFakeOnce sync.Once
	fakeListenerPtr  *fakeListener
)

// fakeListener wraps a channel-based accept for use as xfer.Listener.
type fakeListener struct {
	acceptCh chan fakeAcceptResult
	addr     string
}

type fakeAcceptResult struct {
	conn xfer.Conn
	err  error
}

func (l *fakeListener) Accept(ctx context.Context) (xfer.Conn, error) {
	select {
	case r := <-l.acceptCh:
		return r.conn, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *fakeListener) Close() error { return nil }
func (l *fakeListener) Addr() string { return l.addr }

// registerFakeWebRTC registers a fake "webrtc" transport using xfertest.Pipe.
// Dial creates a pipe pair, queues one end into the corresponding listener's
// acceptCh, and returns the other end for the dialer. The registration is
// safe for concurrent calls — it happens exactly once.
func registerFakeWebRTC() *fakeListener {
	registerFakeOnce.Do(func() {
		fl := &fakeListener{
			acceptCh: make(chan fakeAcceptResult, 16),
			addr:     "pipe://webrtc-fake",
		}
		fakeListenerPtr = fl

		xfer.Register(&xfer.Transport{
			Name: "webrtc",
			Dial: func(_ context.Context, _ string) (xfer.Conn, error) {
				a, b := xfertest.Pipe()
				fl.acceptCh <- fakeAcceptResult{conn: b}
				return a, nil
			},
			Listen: func(_ context.Context, _ string) (xfer.Listener, error) {
				return fl, nil
			},
		})
	})
	return fakeListenerPtr
}

// TestP2PNodeRegisterAndLookup verifies that a node registered via Listen
// can be found via DHT Lookup.
func TestP2PNodeRegisterAndLookup(t *testing.T) {
	registerFakeWebRTC()

	dht := hub.NewDHT()
	node := p2p.NewP2PNode("node-a", dht)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err := node.Listen(ctx, "pipe://addr-a")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	t.Cleanup(func() { node.Close() })

	found, err := dht.Lookup(ctx, "node-a")
	if err != nil {
		t.Fatal("node-a not found in DHT after Listen")
	}
	if found.ID != "node-a" {
		t.Fatalf("expected ID node-a, got %s", found.ID)
	}
	if len(found.Addrs) == 0 || found.Addrs[0] != "pipe://addr-a" {
		t.Fatalf("expected addr pipe://addr-a, got %v", found.Addrs)
	}
}

// TestP2PNodeDial verifies that a P2PNode.Dial discovers a peer in DHT,
// establishes a fake transport connection, and returns a working mux.Mux.
func TestP2PNodeDial(t *testing.T) {
	_ = registerFakeWebRTC()

	dht := hub.NewDHT()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 注册目标节点 DHT
	dht.Register(ctx, hub.PeerInfo{ID: "target", Addrs: []string{"pipe://target-addr"}})

	// 用标准 Listen 模式创建接收端
	listener := p2p.NewP2PNode("target", dht)
	if err := listener.Listen(ctx, "pipe://target-addr"); err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	defer listener.Close()

	// Dialer 连接目标
	dialer := p2p.NewP2PNode("dialer", dht)
	m, err := dialer.Dial(ctx, "target")
	if err != nil {
		t.Fatalf("Dial failed: %v", err)
	}
	defer m.Close()

	// 从 listener 端 Accept 连接
	lm, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	defer lm.Close()

	// listener 端通过 mux 发数据
	stream, err := lm.Open(ctx)
	if err != nil {
		t.Fatalf("Open stream failed: %v", err)
	}
	defer stream.Close()

	_, _ = stream.Write([]byte("hello from peer"))
	_ = stream.CloseWrite()

	// Dialer 端读取数据
	ds, err := m.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept dialer side failed: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := ds.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if got := string(buf[:n]); got != "hello from peer" {
		t.Fatalf("expected 'hello from peer', got %q", got)
	}
}

// TestP2PNodeAccept verifies the full Listen+Accept cycle: a node registers
// itself in DHT via Listen, and another node can Dial in to create a
// connection that is received via Accept.
func TestP2PNodeAccept(t *testing.T) {
	fl := registerFakeWebRTC()

	dht := hub.NewDHT()
	listenerNode := p2p.NewP2PNode("listener", dht)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err := listenerNode.Listen(ctx, "pipe://listener-addr")
	if err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	t.Cleanup(func() { listenerNode.Close() })

	// Spawn a goroutine that accepts one connection and reads data.
	done := make(chan struct{})
	go func() {
		m, acceptErr := listenerNode.Accept(ctx)
		if acceptErr != nil {
			close(done)
			return
		}
		defer m.Close()

		stream, openErr := m.Open(ctx)
		if openErr != nil {
			close(done)
			return
		}
		defer stream.Close()

		_, _ = stream.Write([]byte("pong"))
		_ = stream.CloseWrite()
		close(done)

		<-m.Context().Done()
	}()

	// Feed a pipe pair through the fake listener's acceptCh to simulate
	// an incoming WebRTC connection.
	a, b := xfertest.Pipe()
	fl.acceptCh <- fakeAcceptResult{conn: b}

	dialerMux := mux.New(a, mux.RoleDialer)
	defer dialerMux.Close()

	<-done
	stream, err := dialerMux.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept stream failed: %v", err)
	}

	buf := make([]byte, 1024)
	n, err := stream.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if got := string(buf[:n]); got != "pong" {
		t.Fatalf("expected 'pong', got %q", got)
	}
}
