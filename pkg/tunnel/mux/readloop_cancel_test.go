// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
)

// TestReadLoop_CtxCanceledExitsImmediately 钉住「readLoop 收到 context.Canceled 应立即退出，
// 不当作 transient 重试（退避）」。context.Canceled 是本 mux 被关闭的信号（Mux.Context 的
// cancel 由 Close→done 触发），不是瞬时传输错误——重试只会让每个 mux 在关闭时多打
// 「recv transient error, retrying」日志并以 1s/2s/4s… 退避拖延（CI 实证：pkg/tunnel
// benchmark 收尾卡 44s、pkg/tunnel/mux 54s，两个隧道包异常慢 + 367 条 mux error 日志风暴）。
func TestReadLoop_CtxCanceledExitsImmediately(t *testing.T) {
	// sproxy:serial: 用 mock 返回 ctx.Err 的时序敏感用例，与并行用例交错时 Receive 节奏被拉长 ⇒ 断言超时
	t.Parallel()

	conn := &mockxfer.MockConn{
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			return nil, ctx.Err()
		},
		CloseFn: func() error { return nil },
	}
	m := New(conn, RoleDialer)
	defer m.Close()

	// 记录 Receive 调用次数（readLoop 首次 Receive 返回 ctx.Err 后应立即退出，不再调用）。
	done := make(chan struct{})
	go func() {
		// 触发 m 的 ctx 取消（模拟 Close）——Mux.Context() 的 cancel 由 done 关闭触发。
		m.Close()
		close(done)
	}()

	// 触发 Close（done 关闭 → ctx 取消）。readLoop 的下一次 Receive 返回 ctx.Err：
	// 修复前把 ctx.Err 当 transient ⇒ RecvRetries=1 且进入退避（≥1s 才退）；
	// 修复后立即退出 ⇒ RecvRetries 恒 0。
	// 判据：等 ReceiveCalls 稳定（readLoop 已退出，不再调 Receive）后，断言 RecvRetries==0。
	m.Close()
	var stableCalls int
	ok := testutil.WaitForBool(3*time.Second, func() bool {
		cur := conn.ReceiveCallsCount()
		if cur == stableCalls {
			return true // 两次轮询间无新 Receive ⇒ readLoop 已退出
		}
		stableCalls = cur
		return false
	})
	if !ok {
		t.Fatalf("readLoop 未退出（ReceiveCalls 持续增长至 %d）", conn.ReceiveCallsCount())
	}
	if got := m.metrics.RecvRetries.Load(); got != 0 {
		t.Fatalf("RecvRetries=%d，期望 0（context.Canceled 不应被当作 transient 重试）", got)
	}
}

// 辅助：errors 导入占位（readLoop 实现用 errors.Is；此处仅保证编译期可见性一致）。
var _ = errors.Is
