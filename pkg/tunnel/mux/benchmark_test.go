// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// benchMuxLogger 返回丢弃日志器：benchmark 高频开合 mux 时，对端先关连接会触发
// readLoop 的 recv error 日志（正常收尾噪音），混进输出会污染 benchstat 解析
// （见 docs/archive/benchmark-ci.md §6）。
func benchMuxLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startEchoServer 启动 echo server goroutine，将接收到的数据原样写回。
func startEchoServer(ctx context.Context, m *mux.Mux) {
	go func() {
		for {
			s, err := m.Accept(ctx)
			if err != nil {
				return
			}
			go echoAcceptedStream(s)
		}
	}()
}

// echoAcceptedStream 把流上读到的数据原样写回（读/写出错即返回，结束该流的 echo）。
func echoAcceptedStream(s mux.Stream) {
	buf := make([]byte, 65536)
	for {
		n, err := s.Read(buf)
		if err != nil {
			return
		}
		if _, err := s.Write(buf[:n]); err != nil {
			return
		}
	}
}

// newBenchMuxPair 创建 benchmark 用 mux 对（丢弃日志器），b 结束自动关闭两侧。
func newBenchMuxPair(b *testing.B) (*mux.Mux, *mux.Mux) {
	b.Helper()
	a, bConn := xfertest.Pipe()
	muxA := mux.NewWithOpts(a, mux.RoleDialer, mux.WithLogger(benchMuxLogger()))
	muxB := mux.NewWithOpts(bConn, mux.RoleListener, mux.WithLogger(benchMuxLogger()))
	b.Cleanup(func() { _ = muxA.Close(); _ = muxB.Close() })
	return muxA, muxB
}

// benchStreamRoundTrip 单次流往返：Open → 写 → 读回 → Close。
func benchStreamRoundTrip(b *testing.B, muxA *mux.Mux, ctx context.Context, payload []byte) {
	b.Helper()
	s, err := muxA.Open(ctx)
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	if _, err := s.Write(payload); err != nil {
		b.Fatalf("Write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := s.Read(buf); err != nil {
		b.Fatalf("Read: %v", err)
	}
	s.Close()
}

// openBenchStreams 并发打开 conc 条流；max-streams/rejected 错误按既有语义提前截断。
func openBenchStreams(b *testing.B, muxA *mux.Mux, ctx context.Context, conc int) []mux.Stream {
	b.Helper()
	streams := make([]mux.Stream, 0, conc)
	for range conc {
		s, err := muxA.Open(ctx)
		if err != nil {
			if errors.Is(err, mux.ErrMaxStreams) || errors.Is(err, mux.ErrStreamRejected) {
				break
			}
			b.Fatalf("Open: unexpected %v", err)
		}
		streams = append(streams, s)
	}
	return streams
}

// writeBenchStreams 向每条流写负载；rejected 错误按既有语义跳过。
func writeBenchStreams(b *testing.B, streams []mux.Stream, payload []byte) {
	b.Helper()
	for _, s := range streams {
		if _, err := s.Write(payload); err != nil {
			if errors.Is(err, mux.ErrStreamRejected) {
				continue
			}
			b.Fatalf("Write: unexpected %v", err)
		}
	}
}

// readBenchStreams 从每条流读回数据并关闭；rejected 错误按既有语义跳过。
func readBenchStreams(b *testing.B, streams []mux.Stream) {
	b.Helper()
	for _, s := range streams {
		buf := make([]byte, 1024)
		_, err := s.Read(buf)
		if err != nil {
			if errors.Is(err, mux.ErrStreamRejected) {
				continue
			}
			b.Fatalf("Read: unexpected %v", err)
		}
		s.Close()
	}
}

// BenchmarkMuxThroughput 测试不同负载大小下的吞吐性能。
func BenchmarkMuxThroughput(b *testing.B) {
	sizes := []int{64, 1024, 65536, 1048576} // 64B, 1KB, 64KB, 1MB
	for _, size := range sizes {
		b.Run(fmt.Sprintf("payload_%d", size), func(b *testing.B) {
			a, bConn := xfertest.Pipe()
			muxA := mux.NewWithOpts(a, mux.RoleDialer, mux.WithLogger(benchMuxLogger()))
			muxB := mux.NewWithOpts(bConn, mux.RoleListener, mux.WithLogger(benchMuxLogger()))
			defer muxA.Close()
			defer muxB.Close()

			payload := make([]byte, size)

			ctx, cancel := context.WithTimeout(b.Context(), 30*time.Second)
			defer cancel()
			startEchoServer(ctx, muxB)

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				benchStreamRoundTrip(b, muxA, ctx, payload)
			}
		})
	}
}

// BenchmarkMuxConcurrentStreams 测试不同并发流数下的性能。
func BenchmarkMuxConcurrentStreams(b *testing.B) {
	concurrency := []int{1, 10, 50, 100}
	for _, conc := range concurrency {
		b.Run(fmt.Sprintf("streams_%d", conc), func(b *testing.B) {
			muxA, muxB := newBenchMuxPair(b)

			payload := make([]byte, 1024)

			ctx, cancel := context.WithTimeout(b.Context(), 30*time.Second)
			defer cancel()
			startEchoServer(ctx, muxB)

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				streams := openBenchStreams(b, muxA, ctx, conc)
				writeBenchStreams(b, streams, payload)
				readBenchStreams(b, streams)
			}
		})
	}
}
