// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// pipeXfer 返回一对经过内存管道互连的 xfer 拨号函数。
// client 拨号产生客户端 Conn；serverConn 返回服务端 Conn。
func pipeXfer() (dial func(ctx context.Context) (xfer.Conn, error), serverConn func() xfer.Conn, cleanup func()) {
	serverCh := make(chan xfer.Conn, 4)
	serverConn = func() xfer.Conn {
		select {
		case c := <-serverCh:
			return c
		case <-time.After(3 * time.Second):
			return nil
		}
	}
	dial = func(ctx context.Context) (xfer.Conn, error) {
		a, b := xfertest.Pipe()
		serverCh <- b
		return a, nil
	}
	cleanup = func() {}
	return dial, serverConn, cleanup
}

func TestHubServerRegisterAndRemove(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewRouteTable()
	srv := NewHubServer(rt, NewAuthenticator("secret"), log)
	dial, serverConn, _ := pipeXfer()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvDone := make(chan error, 1)
	go func() {
		c := serverConn()
		if c == nil {
			srvDone <- context.DeadlineExceeded
			return
		}
		srvDone <- srv.HandleConn(ctx, c)
	}()
	clientConn, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	// 节点侧：先发一条注册帧（裸字节流），等价于旧的“直接写 nodeID”语义
	if err := clientConn.Send(ctx, NewRegisterFrame("node-a", "secret", Meta{})); err != nil {
		t.Fatal(err)
	}

	// 等待注册
	deadline := time.Now().Add(3 * time.Second)
	for !rt.Has("node-a") {
		if time.Now().After(deadline) {
			t.Fatal("node-a not registered")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 关闭客户端连接触发自动移除
	_ = clientConn.Close()
	select {
	case err := <-srvDone:
		if err != nil && !errors.Is(err, xfer.ErrConnClosed) && !errors.Is(err, context.Canceled) {
			t.Logf("HandleConn returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
	if rt.Has("node-a") {
		t.Fatal("node-a should have been removed")
	}
}

func TestHubServerBadToken(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewRouteTable()
	srv := NewHubServer(rt, NewAuthenticator("secret"), log)
	dial, serverConn, _ := pipeXfer()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	srvDone := make(chan error, 1)
	go func() {
		c := serverConn()
		if c == nil {
			srvDone <- context.DeadlineExceeded
			return
		}
		srvDone <- srv.HandleConn(ctx, c)
	}()
	clientConn, err := dial(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer clientConn.Close()

	// 错误 token：HubServer 读注册帧后鉴权失败，应立即返回
	if err := clientConn.Send(ctx, []byte(`{"node_id":"node-b","token":"wrong"}`)); err != nil {
		t.Fatal(err)
	}

	// 出错场景：HubServer 读完注册帧并鉴权失败，应尽快返回
	select {
	case err := <-srvDone:
		if err == nil {
			t.Fatal("expected authentication error")
		}
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("expected ErrInvalidToken, got: %v", err)
		}
		if err := clientConn.Close(); err != nil {
			t.Logf("close client: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return on bad token")
	}
	if rt.Has("node-b") {
		t.Fatal("node-b must not be registered")
	}
}

var _ = mux.RoleDialer
