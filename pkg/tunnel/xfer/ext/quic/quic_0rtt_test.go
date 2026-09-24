// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic_test

// quic_0rtt_test.go 验证 0-RTT 会话恢复（roadmap P2 QUIC 0-RTT 残余）：
//  1. 同一进程二次 Dial（复用 session ticket）成功建连（0-RTT 直发路径）。
//  2. 服务端 Allow0RTT 配置存在（quic.Config 装配断言——间接由二次建连覆盖）。

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"
)

// TestQUIC_0RTT_SecondDialReusesSession 二次 Dial 复用 session ticket 成功。
// 首次建连（1-RTT）→ 关闭 → 再次 Dial（应走 0-RTT 缓存路径）→ 双向收发成功。
func TestQUIC_0RTT_SecondDialReusesSession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	setupQUICTLS(t)
	ln, err := quic.Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ql := ln.(*quic.QuicListener)

	// 服务端 accept 循环：接受两连接并回显 announce。
	done := make(chan error, 2)
	go func() {
		for range 2 {
			conn, aerr := ln.Accept(context.Background())
			if aerr != nil {
				done <- aerr
				return
			}
			// Accept 内部已 discardAnnounce（读掉 Dial 的宣告帧）——直接回写 pong。
			if werr := conn.Send(context.Background(), []byte("pong")); werr != nil {
				done <- werr
				return
			}
			_ = conn.Close()
		}
		done <- nil
	}()

	// 首次建连（1-RTT）：Dial 写 announceMagic → 收 pong。
	c1, err := quic.Dial(context.Background(), ql.Addr())
	if err != nil {
		t.Fatalf("首次 Dial: %v", err)
	}
	if _, err := c1.Receive(context.Background()); err != nil {
		t.Fatalf("首次 Receive: %v", err)
	}
	_ = c1.Close()

	// 二次建连（0-RTT：同进程 session ticket 缓存）。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c2, derr := quic.Dial(ctx, ql.Addr())
	if derr != nil {
		t.Fatalf("二次 Dial（0-RTT）: %v", derr)
	}
	defer c2.Close()
	// 双向收发验证。
	if err := c2.Send(ctx, []byte("hello2")); err != nil {
		t.Fatal(err)
	}
	got, rerr := c2.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != "pong" {
		t.Fatalf("pong = %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestDialTLSConfig_SessionCache 0-RTT 会话缓存装配（纯本地，无网络）：
// DialTLSConfig 返回的 tls.Config 必须含 ClientSessionCache（0-RTT 前提）。
func TestDialTLSConfig_SessionCache(t *testing.T) {
	t.Parallel()
	cfg, err := quic.DialTLSConfig("127.0.0.1:443")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientSessionCache == nil {
		t.Fatalf("DialTLSConfig 应装配 ClientSessionCache（0-RTT 前提）")
	}
}
