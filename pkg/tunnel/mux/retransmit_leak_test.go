// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// TestMux_NoGoroutineLeakAfterRetransmitAndClose 锁定「重传路径 + 并发关闭」不泄漏 goroutine。
//
// 背景（AGENTS.md 技术债条目，2026-09 复核）：早期记录为
// 「retransmitLoop 因 releaseStream vs closeWithError 竞争导致极端情况泄漏」。
// 本测试是债务真相的回归钉（TDD 红灯先行验证）：
//   - 制造 Send 持续失败（非 ErrConnClosed）⇒ 帧进重传队列、writeLoop 反复扫描；
//   - 并发执行 Mux.Close（模拟 releaseStream/closeWithError 的竞争窗口）；
//   - 20 轮后要求 goroutine 数回落基线（超限即泄漏，测试失败）。
//
// 稳定通过 ⇒ 债务已不存在，AGENTS.md 的债条目应删除（勿留过期债描述）。
func TestMux_NoGoroutineLeakAfterRetransmitAndClose(t *testing.T) {
	t.Parallel()
	base := runtime.NumGoroutine()

	for range 20 {
		var sendCalls atomic.Int32
		mc := &mockxfer.MockConn{
			// 前 4 帧（Open 握手等）放行；之后的数据帧 Send 失败（非 ErrConnClosed）
			// ⇒ 进入重传队列、writeLoop 反复扫描。
			SendFn: func(context.Context, []byte) error {
				if sendCalls.Add(1) > 4 {
					return mockxfer.ErrSendFailed
				}
				return nil
			},
			ReceiveFn: func(ctx context.Context) ([]byte, error) {
				<-ctx.Done() // 连接空闲：readLoop 阻塞在 ctx 上，不消耗 CPU
				return nil, ctx.Err()
			},
		}
		m := mux.New(mc, mux.RoleDialer)

		// 开流并写入：Send 失败（非 ErrConnClosed）⇒ 帧进重传队列。
		s, err := m.Open(context.Background())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		_, _ = s.Write([]byte("payload-for-retransmit"))

		// 并发 Close（模拟与 releaseStream/closeWithError 竞争）。
		done := make(chan struct{})
		go func() { m.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("Mux.Close 未在超时内返回（可能死锁）")
		}
		m.Close() // 幂等
	}

	// 关闭后所有内部 goroutine（writeLoop/readLoop）必须退出。
	testutil.WaitFor(t, 30*time.Second, func() bool {
		return runtime.NumGoroutine() <= base+2
	}, func() string {
		return fmt.Sprintf("mux 关闭后 goroutine 未回落：base=%d now=%d", base, runtime.NumGoroutine())
	})
}
