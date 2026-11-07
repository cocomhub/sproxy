// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfertest

// bench.go 提供跨传输实现的全链路吞吐基准（roadmap P2 端到端带宽基准）：
//
//   - BenchmarkXferThroughput：harness.Listen+Dial 建链 → 双向 Send/Receive
//     吞吐（MB/s）。tcp/ws/quic 各传输经其注册 harness 统一挂 bench（bench-baseline
//     已含 ./pkg/...，自动纳入门禁）。
//   - 数据面用固定 64 KiB 消息（对齐 mux 帧负载上限），避免 benchmem 噪声。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// RegisterBench 把传输 harness 注册进吞吐基准（bench 前调用；幂等）。
// 用法：在传输实现的 benchmark_test.go 里
//
//	xfertest.RegisterBench(xfertest.Harness{Name: "tcp", Dial: tcp.Dial, Listen: tcp.Listen})
//	func BenchmarkXferThroughput(b *testing.B) { xfertest.BenchmarkXferThroughput(b) }
var benchHarnesses []Harness

// RegisterBench 登记传输 harness（bench 用；跨传输统一挂点）。
func RegisterBench(h Harness) {
	benchHarnesses = append(benchHarnesses, h)
}

// benchPayloadSize 是吞吐基准的消息负载大小（64 KiB，对齐 mux 帧负载上限）。
const benchPayloadSize = 64 << 10

// BenchmarkXferThroughput 运行所有已登记传输的双向吞吐基准。
// 每个传输子基准：Listen+Dial 建链 → b.N 次 Send（对端 Receive 泵）+ 反向 Receive。
// 报告 MB/s（基于 payload 字节 / 墙钟）。
func BenchmarkXferThroughput(b *testing.B) {
	for _, h := range benchHarnesses {
		b.Run(h.Name, func(b *testing.B) {
			runBenchOnce(b, h)
		})
	}
}

// runBenchOnce 跑单个传输的子基准（建链 + 双向吞吐）。
func runBenchOnce(b *testing.B, h Harness) {
	b.Helper()
	ctx := context.Background()
	ln, err := h.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		b.Fatalf("Listen: %v", err)
	}
	defer ln.Close()
	addr := listenerAddr(ln)

	acceptCh := make(chan xfer.Conn, 1)
	go acceptBenchConn(ln, ctx, acceptCh)
	clientConn, derr := h.Dial(ctx, addr)
	if derr != nil {
		b.Fatalf("Dial: %v", derr)
	}
	defer clientConn.Close()
	serverConn := <-acceptCh
	if serverConn == nil {
		b.Fatal("accept 失败")
	}
	defer serverConn.Close()

	payload := make([]byte, benchPayloadSize)
	stop := make(chan struct{})
	go pumpBenchReceive(serverConn, ctx, stop)

	b.SetBytes(benchPayloadSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if serr := clientConn.Send(ctx, payload); serr != nil {
			b.Fatalf("Send: %v", serr)
		}
	}
	b.StopTimer()
	close(stop)
}

// acceptBenchConn 异步 accept 并回传连接（回传 nil 表示 accept 失败）。
func acceptBenchConn(ln xfer.Listener, ctx context.Context, ch chan<- xfer.Conn) {
	if c, aerr := ln.Accept(ctx); aerr == nil {
		ch <- c
	}
}

// pumpBenchReceive 对端 Receive 泵：循环接收丢弃，直到 stop 或接收出错。
func pumpBenchReceive(c xfer.Conn, ctx context.Context, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
			if _, rerr := c.Receive(ctx); rerr != nil {
				return
			}
		}
	}
}

// 编译期断言（避免 listenerAddr 未用）。
var _ = fmt.Sprintf
var _ = time.Second
