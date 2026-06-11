// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func BenchmarkMuxThroughput(b *testing.B) {
	sizes := []int{64, 1024, 65536, 1048576} // 64B, 1KB, 64KB, 1MB
	for _, size := range sizes {
		b.Run(fmt.Sprintf("payload_%d", size), func(b *testing.B) {
			a, pipeB := xfertest.Pipe()
			muxA := mux.New(a, mux.RoleDialer)
			muxB := mux.New(pipeB, mux.RoleListener)
			defer muxA.Close()
			defer muxB.Close()

			payload := make([]byte, size)
			ctx := context.Background()

			// Server: echo
			go func() {
				for {
					s, err := muxB.Accept(ctx)
					if err != nil {
						return
					}
					go func() {
						buf := make([]byte, 65536)
						for {
							_, err := s.Read(buf)
							if err != nil {
								return
							}
							_, err = s.Write(payload)
							if err != nil {
								return
							}
						}
					}()
				}
			}()

			b.ResetTimer()
			b.SetBytes(int64(size))

			for range b.N {
				s, err := muxA.Open(ctx)
				if err != nil {
					b.Fatal(err)
				}
				_, err = s.Write(payload)
				if err != nil {
					b.Fatal(err)
				}

				buf := make([]byte, len(payload))
				_, err = s.Read(buf)
				if err != nil {
					b.Fatal(err)
				}
				s.Close()
			}
		})
	}
}

func BenchmarkMuxConcurrentStreams(b *testing.B) {
	concurrencies := []int{1, 10, 50, 100}
	for _, conc := range concurrencies {
		b.Run(fmt.Sprintf("streams_%d", conc), func(b *testing.B) {
			a, pipeB := xfertest.Pipe()
			muxA := mux.New(a, mux.RoleDialer)
			muxB := mux.New(pipeB, mux.RoleListener)
			defer muxA.Close()
			defer muxB.Close()

			payload := []byte("hello")
			ctx := context.Background()

			// Server: echo
			go func() {
				for {
					s, err := muxB.Accept(ctx)
					if err != nil {
						return
					}
					go func() {
						buf := make([]byte, 1024)
						for {
							_, err := s.Read(buf)
							if err != nil {
								return
							}
							_, err = s.Write(payload)
							if err != nil {
								return
							}
						}
					}()
				}
			}()

			b.ResetTimer()

			for range b.N {
				streams := make([]*mux.Stream, conc)
				for i := range conc {
					s, err := muxA.Open(ctx)
					if err != nil {
						b.Fatal(err)
					}
					streams[i] = s
				}

				for _, s := range streams {
					if _, err := s.Write(payload); err != nil {
						b.Fatal(err)
					}
				}

				buf := make([]byte, 1024)
				for _, s := range streams {
					_, err := s.Read(buf)
					if err != nil {
						b.Fatal(err)
					}
					s.Close()
				}
			}
		})
	}
}
