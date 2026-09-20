// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"strings"
	"testing"
)

// TestSlogTracerIndentFollowsNesting 钉住 span 日志缩进的两个方向：
//
//   - **顺序** span（同一个 ctx 上连续 StartSpan/end）必须**没有**缩进——缩进表达的是嵌套层级，
//     不是「累计打了多少条 span」；曾因 depth 只 `++` 不 `--`，每请求 +2 空格、单行最多 4.3 KB
//     空白，成功 run 的 CI job 日志 26 MB 里 94% 是这些空白
//     （取证见 docs/archive/benchmark-ci-timeout-disk-io.md）；
//   - **真嵌套** span（子 span 从父 ctx 上开）仍要保留缩进，否则父/子难以肉眼区分。
func TestSlogTracerIndentFollowsNesting(t *testing.T) {
	// sproxy:serial: 需要接管全局 slog default（captureLog）才能断言渲染后的缩进。
	output := captureLog(t, func() {
		tr := New()

		for range 3 {
			_, end := tr.StartSpan(context.Background(), "sequential")
			end()
		}

		ctx, endParent := tr.StartSpan(context.Background(), "parent")
		_, endChild := tr.StartSpan(ctx, "child")
		endChild()
		endParent()
	})

	// 顺序 span：出现带缩进的 "sequential" 行即为 depth 泄漏。
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, "sequential") && strings.Contains(line, `msg="  [trace`) {
			t.Fatalf("顺序 span 不应有缩进（depth 未释放 ⇒ 日志线性膨胀）：\n%s", line)
		}
	}
	// 真嵌套：child 必须有缩进（2 空格），parent 必须没有。
	sawChildIndented := false
	for line := range strings.SplitSeq(output, "\n") {
		if strings.Contains(line, " child ") && strings.Contains(line, `msg="  [trace`) {
			sawChildIndented = true
		}
		if strings.Contains(line, " parent ") && strings.Contains(line, `msg="  [trace`) {
			t.Fatalf("父 span 不应有缩进：\n%s", line)
		}
	}
	if !sawChildIndented {
		t.Fatalf("嵌套 child span 应保留一级缩进，实际日志：\n%s", output)
	}
}
