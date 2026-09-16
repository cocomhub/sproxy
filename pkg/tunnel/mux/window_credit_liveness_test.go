// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"testing"
	"testing/synctest"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件用 testing/synctest 钉住「窗口耗尽 → `Write` 阻塞 → 经真实帧路径收到窗口更新 →
// `Write` 恢复」这条**端到端活性链路**。
//
// 覆盖缺口（2026-09-16 独立复核指出）：这条链路是「窗口信用不得静默丢失」（见
// window_credit_test.go）的**动机**，但此前两半各自覆盖——window_credit_test.go 钉本侧补发、
// frame_handler_robustness_test.go 钉「收帧加窗口」，中间的「阻塞中的 `Write` 是否真会被
// 唤醒」无人过问：若唤醒路径回归（例如 handler 不再通知 `windowUpdateCh`），两半仍各自绿，
// 线上却是永久挂起的 `Write`。
//
// 用 synctest 的理由（同 pkg/sync/httptransport/deadline_test.go 的先例）：气泡内的阻塞
// channel 收发被判为 durably blocked ⇒ `synctest.Wait` 正常返回且无需任何固定 sleep；
// mux 内部的 50ms 扫描 ticker 与 30s 心跳走**虚拟时钟**，不会给用例引入真实等待。

// TestWindowUpdate_WriteUnblocksOnCredit：窗口耗尽的 `Write` 必须一直阻塞，直到**经真实帧
// 路径**（readLoop → handleWindowUpdateFrame）收到窗口更新才返回，且更新量真实加到窗口上。
func TestWindowUpdate_WriteUnblocksOnCredit(t *testing.T) {
	t.Parallel()
	synctest.Test(t, windowUpdateWriteUnblocksBody)
}

// windowUpdateWriteUnblocksBody 在 synctest 气泡内运行。
func windowUpdateWriteUnblocksBody(t *testing.T) {
	a, b := xfertest.Pipe() // 内存 pipe：b.Send 投递的字节成为 a 侧 readLoop 收到的帧
	m := New(a, RoleDialer)
	defer func() { _ = m.Close() }()

	ctx := t.Context()
	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	// 窗口耗尽：等价于 DefaultWindowSize(65536) 的额度已全部发出（白盒置 0，避免搬运 64 KiB）。
	ss.windowSize.Store(0)

	const payload = "hello"
	type writeResult struct {
		n   int
		err error
	}
	resCh := make(chan writeResult, 1)
	go func() {
		n, werr := s.Write([]byte(payload))
		resCh <- writeResult{n: n, err: werr}
	}()

	// 窗口为 0 ⇒ `Write` 必须停在 `for s.windowSize.Load() <= 0`；此时气泡内全部 goroutine
	// 持久阻塞（mux 的 writeLoop/readLoop/心跳各自等 channel 或虚拟 timer），Wait 正常返回。
	synctest.Wait()
	select {
	case r := <-resCh:
		t.Fatalf("窗口为 0 时 Write 不得返回（n=%d err=%v）", r.n, r.err)
	default:
	}

	// 经真实帧路径投递窗口更新。
	const delta = 1024
	pl := make([]byte, windowUpdateLen)
	binary.BigEndian.PutUint32(pl, delta)
	frame, err := EncodeFrame(s.ID(), FrameWindowUpdate, pl)
	if err != nil {
		t.Fatalf("编码窗口更新帧: %v", err)
	}
	if err := b.Send(ctx, frame); err != nil {
		t.Fatalf("投递窗口更新帧: %v", err)
	}

	synctest.Wait()
	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("收到窗口更新后 Write 应成功返回: err=%v", r.err)
		}
		if r.n != len(payload) {
			t.Errorf("Write n=%d want %d", r.n, len(payload))
		}
	default:
		t.Fatal("收到窗口更新后阻塞的 Write 必须被唤醒并返回（本修复所依赖的活性链路）")
	}

	// 窗口 = 0 + delta − 已写出的 payload 字节；断言用「> 0」避免与上面那次 Write 的
	// `windowSize.Add(-writeLen)` 争时序（精确值取决于 Write 是否已扣减）。
	if got := ss.windowSize.Load(); got <= 0 {
		t.Errorf("窗口更新未真实加到发送窗口上: got=%d", got)
	}
}
