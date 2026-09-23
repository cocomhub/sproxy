// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp

// bench_test.go 把内置 TCP 传输登记进跨传输吞吐基准（roadmap P2 端到端带宽基准）。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func init() {
	xfertest.RegisterBench(xfertest.Harness{Name: "tcp", Dial: Dial, Listen: Listen})
}

// BenchmarkXferThroughput 运行 tcp 传输的全链路吞吐基准（bench-baseline 自动纳入）。
func BenchmarkXferThroughput(b *testing.B) {
	xfertest.BenchmarkXferThroughput(b)
}
