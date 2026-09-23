// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ws_test

// ws_metrics_test.go 验证 WS 传输级指标（roadmap 6.x P1 残余）：
//  1. Dial/Accept 后 Metrics.ConnsOpened 增加。
//  2. Send/Receive 后 Metrics 消息/字节计数增加。

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
)

// TestWSMetrics_SendRecv 建链 Send/Receive → Metrics 计数。
func TestWSMetrics_SendRecv(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ln, err := ws.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	acceptCh := make(chan xfer.Conn, 1)
	go func() {
		c, aerr := ln.Accept(ctx)
		if aerr != nil {
			return
		}
		acceptCh <- c
	}()

	cli, err := ws.Dial(ctx, "ws://"+wsListenAddr(ln)+"/ws")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	var srv xfer.Conn
	select {
	case c := <-acceptCh:
		srv = c
	case <-ctx.Done():
		t.Fatal("accept timeout")
	}
	defer srv.Close()

	// 计数是全局共享（并行测试污染）——用相对基线断言消息计数（Send 后必增）。
	base := ws.Metrics()
	msgCount := base.MessagesSent
	if err := cli.Send(ctx, []byte("ping")); err != nil {
		t.Fatal(err)
	}
	msg, rerr := srv.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(msg) != "ping" {
		t.Fatalf("recv = %q", msg)
	}
	if serr := srv.Send(ctx, []byte("pong")); serr != nil {
		t.Fatal(serr)
	}
	if _, cerr := cli.Receive(ctx); cerr != nil {
		t.Fatal(cerr)
	}
	after := ws.Metrics()
	if after.MessagesSent < msgCount+2 {
		t.Fatalf("MessagesSent 应 +2: base=%d after=%d", msgCount, after.MessagesSent)
	}
	if after.MessagesRecv < base.MessagesRecv+2 {
		t.Fatalf("MessagesRecv 应 +2: base=%d after=%d", base.MessagesRecv, after.MessagesRecv)
	}
}

// wsListenAddr 提取 listener 地址（xfer.Listener 无 Addr 方法——经类型断言）。
func wsListenAddr(l xfer.Listener) string {
	if a, ok := l.(interface{ Addr() net.Addr }); ok {
		return a.Addr().String()
	}
	return "127.0.0.1:0"
}
