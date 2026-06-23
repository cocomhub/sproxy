// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// startEchoServer 启动 echo server goroutine，将接收到的数据原样写回。
func startEchoServer(ctx context.Context, m *mux.Mux) {
	go func() {
		for {
			s, err := m.Accept(ctx)
			if err != nil {
				return
			}
			go func() {
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
			}()
		}
	}()
}

// BenchmarkMuxThroughput 测试不同负载大小下的吞吐性能。
func BenchmarkMuxThroughput(b *testing.B) {
	sizes := []int{64, 1024, 65536, 1048576} // 64B, 1KB, 64KB, 1MB
	for _, size := range sizes {
		b.Run(fmt.Sprintf("payload_%d", size), func(b *testing.B) {
			a, bConn := xfertest.Pipe()
			muxA := mux.NewWithOpts(a, mux.RoleDialer)
			muxB := mux.NewWithOpts(bConn, mux.RoleListener)
			defer muxA.Close()
			defer muxB.Close()

			payload := make([]byte, size)

			ctx, cancel := context.WithTimeout(b.Context(), 30*time.Second)
			defer cancel()
			startEchoServer(ctx, muxB)

			b.SetBytes(int64(size))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
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
		})
	}
}

// BenchmarkMuxConcurrentStreams 测试不同并发流数下的性能。
func BenchmarkMuxConcurrentStreams(b *testing.B) {
	concurrency := []int{1, 10, 50, 100}
	for _, conc := range concurrency {
		b.Run(fmt.Sprintf("streams_%d", conc), func(b *testing.B) {
			a, bConn := xfertest.Pipe()
			muxA := mux.NewWithOpts(a, mux.RoleDialer)
			muxB := mux.NewWithOpts(bConn, mux.RoleListener)
			defer muxA.Close()
			defer muxB.Close()

			payload := make([]byte, 1024)

			ctx, cancel := context.WithTimeout(b.Context(), 30*time.Second)
			defer cancel()
			startEchoServer(ctx, muxB)

			b.ResetTimer()

			for i := 0; i < b.N; i++ {
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

				for _, s := range streams {
					if _, err := s.Write(payload); err != nil {
						if errors.Is(err, mux.ErrStreamRejected) {
							continue
						}
						b.Fatalf("Write: unexpected %v", err)
					}
				}
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
		})
	}
}
