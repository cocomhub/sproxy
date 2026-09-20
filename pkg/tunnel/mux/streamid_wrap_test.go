// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

// streamid_wrap_test.go 钉住 StreamID 回绕防护（审计 F7：nextID 无回绕检查）。
//
// 背景：Mux.nextID 为 uint32，RoleDialer 从 1 开始按 +2 递增分配用户流 ID
// （StreamID=0 保留给控制流 Ping/Pong/Datagram）。长命 mux（hub↔leaf 中继、
// pkg/server/relay_stream.go、pkg/sync/httptransport）上，若累计打开的流数超过
// 2^31（约 21 亿）次，nextID 会**回绕到 0**——而 0 是控制流专用 ID。此后：
//   - 本端 Open 分配 id=0 的流，与 Ping/Pong/Datagram 帧（EncodeFrame(0,...)）混用，
//     对端 frame_handler 会把用户数据帧当控制帧丢弃/误处理 ⇒ 静默丢字节；
//   - 若 nextID 回绕后继续 +2（2,4,6...）撞上仍存活的流表项（旧 id），新流覆盖旧流 ⇒
//     旧句柄读写失效，且 removeStreamIf 的对象身份闸门会让注销不再命中（悬挂表项）。
//
// 修复语义（TDD 红灯先行）：Open 分配 nextID 前检查回绕（nextID 已越过可用上限）⇒
// 返回 ErrStreamIDExhausted fail-closed 错误，绝不让 id=0 或已用 ID 流出。
// 本测试通过「把 nextID 拨到接近上限」的注入构造逼近回绕，断言 Open 返回错误且
// 不产生 id=0 的流（回归钉：无防护时该测试红）。

import (
	"errors"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestOpenRejectsStreamIDWrap 验证 nextID 回绕到 0 时 Open fail-closed。
//
// 注入方式：不依赖真实打开 2^31 条流（不现实），而是把 RoleDialer 的 nextID
// 拨到「+2 后即越过 uint32 上限」的临界值（白盒测试直接改字段——生产字段名变化
// 会红，属预期耦合；生产无导出 setter，不污染 API）。
func TestOpenRejectsStreamIDWrap(t *testing.T) {
	t.Parallel()
	// 用内存 pipe 连接（Open 在发帧前就应拒绝，对端不需真实交互）。
	a, b := xfertest.Pipe()
	muxA := New(a, RoleDialer)
	muxB := New(b, RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx := t.Context()

	// 正常分配路径基线：id 从 1 开始 +2 递增（不重复、不用 0）。
	first, err := muxA.Open(ctx)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	defer first.Close()
	if first.ID() != 1 {
		t.Fatalf("first stream ID = %d, want 1（控制流 0 保留）", first.ID())
	}
	second, err := muxA.Open(ctx)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	defer second.Close()
	if second.ID() != 3 {
		t.Fatalf("second stream ID = %d, want 3", second.ID())
	}

	// 回绕注入：把 nextID 拨到「+2 后恰好回绕到 0」的临界值 0xFFFFFFFE
	// （= 4294967294；+2 = 4294967296 mod 2^32 = 0）⇒ 下一次 Open 必须 fail-closed。
	muxA.mu.Lock()
	muxA.nextID = 0xFFFFFFFE
	muxA.mu.Unlock()

	s, err := muxA.Open(ctx)
	if err == nil {
		// 无防护时：拿到 id=0 的流（与 Ping/Pong 控制帧冲突）。
		s.Close()
		t.Fatalf("Open 应拒绝 StreamID 回绕（nextID=0xFFFFFFFE +2 = 0），实际拿到 id=%d", s.ID())
	}
	if !errors.Is(err, ErrStreamIDExhausted) {
		t.Fatalf("回绕拒绝错误应可识别（ErrStreamIDExhausted），实际 %v", err)
	}
}
