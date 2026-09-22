// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp

// tcp_stats_test.go 验证连接级统计（roadmap 6.x P1 传输层指标）：
//  1. FromNetConn 打开计数 +1；Close 关闭计数 +1（幂等只计一次）。
//  2. Send/Receive 成功一条 → messages/bytes 各 +1（payload 字节）。
//  3. 发送失败（peer 关闭）不计消息（计数只计成功）。

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestTCPStats_SendReceiveCounters(t *testing.T) {
	t.Parallel()
	before := Metrics()
	// 建一对内存 pipe：服务端接收 + 客户端发送。
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	sc := FromNetConn(server) // 服务端侧 Conn（Receive 侧）
	cc := FromNetConn(client) // 客户端侧 Conn（Send 侧）

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// net.Pipe 同步：Receive 必须先就位（阻塞读），Send 才能完成。
	recvCh := make(chan struct {
		msg string
		err error
	}, 1)
	go func() {
		m, err := sc.Receive(ctx)
		recvCh <- struct {
			msg string
			err error
		}{string(m), err}
	}()
	// Send "hello"（5 字节 payload）。
	if err := cc.Send(ctx, []byte("hello")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	got := <-recvCh
	if got.err != nil {
		t.Fatalf("Receive: %v", got.err)
	}
	if got.msg != "hello" {
		t.Fatalf("msg=%q", got.msg)
	}

	// 包级共享计数 + t.Parallel → 其它并行用例也会增量：断言「本用例必产生」的
	// 下界（delta >= 预期），并用 before 快照隔离噪声。
	m := Metrics()
	if got := m.MessagesSent - before.MessagesSent; got < 1 {
		t.Fatalf("messagesSent 增量 = %d, want >= 1", got)
	}
	if got := m.MessagesRecv - before.MessagesRecv; got < 1 {
		t.Fatalf("messagesRecv 增量 = %d, want >= 1", got)
	}
	if got := m.BytesSent - before.BytesSent; got < 5 {
		t.Fatalf("bytesSent 增量 = %d, want >= 5", got)
	}
	if got := m.BytesRecv - before.BytesRecv; got < 5 {
		t.Fatalf("bytesRecv 增量 = %d, want >= 5", got)
	}
	if got := m.ConnsOpened - before.ConnsOpened; got < 2 {
		t.Fatalf("connsOpened 增量 = %d, want >= 2", got)
	}
	// Close 幂等：只计一次（本用例至少 2 次 Close）。
	_ = sc.Close()
	_ = sc.Close()
	_ = cc.Close()
	m2 := Metrics()
	if got := m2.ConnsClosed - m.ConnsClosed; got < 2 {
		t.Fatalf("connsClosed 增量 = %d, want >= 2", got)
	}
}

// TestTCPStats_NoCountOnFailedSend 发送失败（对端关闭）不计消息计数。
func TestTCPStats_NoCountOnFailedSend(t *testing.T) {
	t.Parallel()
	before := Metrics()
	server, client := net.Pipe()
	sc := FromNetConn(server)
	cc := FromNetConn(client)

	// 对端立即关闭 → 发送失败。
	_ = sc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := cc.Send(ctx, []byte("x")); err == nil {
		t.Fatal("对端关闭后 Send 应失败")
	}
	m := Metrics()
	if got := m.MessagesSent - before.MessagesSent; got != 0 {
		t.Fatalf("失败发送不应计数: %d", got)
	}
	_ = cc.Close()
	_ = cc.Close()
}
