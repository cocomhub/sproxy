// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// diag213.go 是 issue #213 的**临时诊断插桩**（用完即删，不得进入 master）。
package mux

import (
	"fmt"
	"os"
	"sync/atomic"
)

// diagSeq 是全局递增序号（原子，避免 -race 报数据竞争）。
var diagSeq atomic.Int64

// diagf 打印一条诊断行（全局序号 + 自定义字段）。
func diagf(format string, args ...any) {
	n := diagSeq.Add(1)
	fmt.Fprintf(os.Stderr, "[DIAGMUX] #%d "+format+"\n", append([]any{n}, args...)...)
}
