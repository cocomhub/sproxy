// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"errors"
	"strings"
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

	// 节点侧：先发一条注册帧（token 非空 → JSON 帧；裸字节回退见 TestHubServerBareNodeID）
	if err := clientConn.Send(ctx, NewRegisterFrame("node-a", "secret", Meta{})); err != nil {
		t.Fatal(err)
	}

	// 客户端侧验证 REG_OK 帧内容（I49）：未声明 per-node-secret 能力 → 纯 "REG_OK"
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK frame, got error: %v", ackErr)
	}
	if string(ack) != RegisterAckOK {
		t.Fatalf("expected %q, got %q", RegisterAckOK, string(ack))
	}

	// 注册应已生效
	if !rt.Has("node-a") {
		t.Fatal("node-a not registered")
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

	// 客户端侧验证 REG_ERR 帧内容（I49）：isTerminalRelayError 唯一采信的依据
	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_ERR frame, got error: %v", ackErr)
	}
	ackStr := string(ack)
	if !strings.HasPrefix(ackStr, RegisterAckErr) {
		t.Fatalf("expected REG_ERR prefix, got %q", ackStr)
	}
	if !strings.Contains(ackStr, "invalid token") {
		t.Fatalf("expected 'invalid token' in REG_ERR, got %q", ackStr)
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

func TestHubServer_TryHandleConn_MaxConns(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewRouteTable()
	srv := NewHubServer(rt, NewAuthenticator("secret"), log, 1)

	ctx := t.Context()

	// 第一个连接：信号量空，应被接受
	client1, server1 := xfertest.Pipe()
	if !srv.TryHandleConn(ctx, server1) {
		t.Fatal("expected first connection to be accepted")
	}

	// 第二个连接：信号量已满，应被拒绝（TryHandleConn 返回 false，调用方负责 Close）
	client2, server2 := xfertest.Pipe()
	if srv.TryHandleConn(ctx, server2) {
		t.Fatal("expected second connection to be rejected when maxConns=1")
	}
	_ = client2.Close()
	_ = server2.Close()

	// 关闭第一个连接，处理 goroutine 结束后应释放名额
	_ = client1.Close()
	_ = server1.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		client3, server3 := xfertest.Pipe()
		if srv.TryHandleConn(ctx, server3) {
			_ = client3.Close()
			_ = server3.Close()
			return
		}
		_ = client3.Close()
		_ = server3.Close()
		if time.Now().After(deadline) {
			t.Fatal("semaphore not released after conn close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHubServer_TryHandleConn_NoLimit(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewRouteTable()
	srv := NewHubServer(rt, NewAuthenticator("secret"), log) // 不传上限 = 无上限
	ctx := t.Context()

	client, server := xfertest.Pipe()
	if !srv.TryHandleConn(ctx, server) {
		t.Fatal("expected connection accepted when maxConns unset")
	}
	_ = client.Close()
	_ = server.Close()
}

// TestHubServerBareNodeID 覆盖 readRegisterFrame 的裸字节回退分支（I48）：
// 非 JSON 裸字符串被当作 nodeID；nil auth（测试专用）下注册应成功并回发 REG_OK。
func TestHubServerBareNodeID(t *testing.T) {
	log := testutil.DiscardLogger()
	rt := NewRouteTable()
	srv := NewHubServer(rt, nil, log) // nil auth：测试专用，跳过鉴权
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

	// 裸字节注册帧：非 JSON，readRegisterFrame 走裸字符串容错分支
	if err := clientConn.Send(ctx, []byte("node-bare")); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK frame, got error: %v", ackErr)
	}
	if string(ack) != RegisterAckOK {
		t.Fatalf("expected %q, got %q", RegisterAckOK, string(ack))
	}
	if !rt.Has("node-bare") {
		t.Fatal("node-bare not registered")
	}

	_ = clientConn.Close()
	select {
	case err := <-srvDone:
		if err != nil && !errors.Is(err, xfer.ErrConnClosed) && !errors.Is(err, context.Canceled) {
			t.Logf("HandleConn returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestHubServerRegisterSecretCapability 验证 per-node secret 下发（I1）：
// 声明 per-node-secret 能力的节点，REG_OK 携带 secret，且 LookupInfo 返回一致。
func TestHubServerRegisterSecretCapability(t *testing.T) {
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

	// 声明 per-node-secret 能力：REG_OK 应携带 "<base64url secret>"
	frame := []byte(`{"node_id":"node-sec","token":"secret","capabilities":["per-node-secret"]}`)
	if err := clientConn.Send(ctx, frame); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK frame, got error: %v", ackErr)
	}
	ackStr := string(ack)
	prefix := RegisterAckOK + registerAckSecretSep
	if !strings.HasPrefix(ackStr, prefix) {
		t.Fatalf("expected %q prefix, got %q", prefix, ackStr)
	}
	wantSecret := strings.TrimPrefix(ackStr, prefix)
	if wantSecret == "" {
		t.Fatal("expected non-empty per-node secret in REG_OK")
	}

	// LookupInfo 应返回同一下发的 secret
	info, ok := rt.LookupInfo("node-sec")
	if !ok {
		t.Fatal("node-sec should be registered")
	}
	if info.Secret == "" {
		t.Fatal("expected non-empty per-node secret in NodeInfo")
	}
	if info.Secret != wantSecret {
		t.Fatalf("secret mismatch: ack=%q lookup=%q", wantSecret, info.Secret)
	}

	_ = clientConn.Close()
	select {
	case err := <-srvDone:
		if err != nil && !errors.Is(err, xfer.ErrConnClosed) && !errors.Is(err, context.Canceled) {
			t.Logf("HandleConn returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestHubServerRegisterNoCapabilityPlainAck 验证未声明能力的节点：
// REG_OK 为纯 "REG_OK"，NodeInfo.Secret 为空（行为不变）。
func TestHubServerRegisterNoCapabilityPlainAck(t *testing.T) {
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

	// 未声明能力：REG_OK 为纯 "REG_OK"
	frame := []byte(`{"node_id":"node-plain","token":"secret"}`)
	if err := clientConn.Send(ctx, frame); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_OK frame, got error: %v", ackErr)
	}
	if string(ack) != RegisterAckOK {
		t.Fatalf("expected %q, got %q", RegisterAckOK, string(ack))
	}

	info, ok := rt.LookupInfo("node-plain")
	if !ok {
		t.Fatal("node-plain should be registered")
	}
	if info.Secret != "" {
		t.Fatalf("expected empty secret for node without capability, got %q", info.Secret)
	}

	_ = clientConn.Close()
	select {
	case err := <-srvDone:
		if err != nil && !errors.Is(err, xfer.ErrConnClosed) && !errors.Is(err, context.Canceled) {
			t.Logf("HandleConn returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
}

// TestHubServerJSONMissingNodeID 覆盖 S2：合法 JSON 但缺 node_id 应报错并回发
// REG_ERR（而非把 JSON 垃圾整串当裸串回退）。
func TestHubServerJSONMissingNodeID(t *testing.T) {
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

	// 合法 JSON 但缺 node_id：readRegisterFrame 返回错误
	if err := clientConn.Send(ctx, []byte(`{"foo":"bar"}`)); err != nil {
		t.Fatal(err)
	}

	ack, ackErr := clientConn.Receive(ctx)
	if ackErr != nil {
		t.Fatalf("expected REG_ERR frame, got error: %v", ackErr)
	}
	ackStr := string(ack)
	if !strings.HasPrefix(ackStr, RegisterAckErr) {
		t.Fatalf("expected REG_ERR prefix, got %q", ackStr)
	}
	if !strings.Contains(ackStr, "bad register frame") {
		t.Fatalf("expected 'bad register frame' in REG_ERR, got %q", ackStr)
	}

	select {
	case err := <-srvDone:
		if err == nil {
			t.Fatal("expected readRegisterFrame error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("HandleConn did not return")
	}
	if rt.NodeCount() != 0 {
		t.Fatalf("invalid frame must not register a node, count=%d", rt.NodeCount())
	}
}

var _ = mux.RoleDialer
