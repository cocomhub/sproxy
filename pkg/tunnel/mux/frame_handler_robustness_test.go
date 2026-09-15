// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"encoding/binary"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件钉住「帧处理器对**远端可控负载长度**的健壮性」。
//
// 动机（2026-09-16 独立审计的 P1 缺陷，已确认）：`handleWindowUpdateFrame` 直接
// `binary.BigEndian.Uint32(payload)` 而**没有长度校验**——对端只要发一个负载 < 4 字节的
// `FrameWindowUpdate`，`Uint32` 就会 panic，而它在 **readLoop goroutine** 里执行 ⇒
// 整个进程崩溃（远程 DoS）。同文件的 `handleDatagramFrame` 有 `len(payload) < datagramFlowLen`
// 校验，说明这是遗漏。本文件既钉住该点，也用「所有帧类型 × 短负载」的不变量覆盖**未来的**
// 新处理器（新 handler 忘记校验会在 CI 直接红）。

// newHandlerTestMux 建一个带对端的 Mux（readLoop/writeLoop 会起 goroutine；对端是内存 pipe，
// 有缓冲不会阻塞）。返回的 peer conn 可用来投递**原始帧字节**，模拟远端攻击者。
func newHandlerTestMux(t *testing.T, opts ...Option) (*Mux, func(ctx context.Context, raw []byte) error) {
	t.Helper()
	a, b := xfertest.Pipe()
	m := NewWithOpts(a, RoleListener, opts...)
	t.Cleanup(func() { _ = m.Close() })
	return m, b.Send
}

// callHandlerNoPanic 调用处理器并把「panic」转成可读的测试失败（否则整个测试二进制会崩，
// 看不出是哪个帧类型/长度触发的）。
func callHandlerNoPanic(t *testing.T, name string, n int, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s 在负载长度 %d 时 panic（远端可触发进程崩溃）: %v", name, n, r)
		}
	}()
	fn()
}

// TestHandleWindowUpdateFrame_ShortPayload：负载 0/1/2/3 字节必须被当作协议错误丢弃
// （不 panic、不改窗口），4 字节正常帧仍生效。
func TestHandleWindowUpdateFrame_ShortPayload(t *testing.T) {
	t.Parallel()
	m, _ := newHandlerTestMux(t)

	const sid = StreamID(11)
	s := newStream(sid, m)
	m.mu.Lock()
	m.streams[sid] = s
	m.mu.Unlock()

	if want := int32(DefaultWindowSize); s.windowSize.Load() != want {
		t.Fatalf("初始窗口=%d want %d", s.windowSize.Load(), want)
	}
	beforeWin := s.windowSize.Load()

	for _, n := range []int{0, 1, 2, 3} {
		beforeErr := m.metrics.Errors.Load()
		payload := make([]byte, n)
		callHandlerNoPanic(t, "handleWindowUpdateFrame", n, func() {
			handleWindowUpdateFrame(m, sid, payload)
		})
		if got := s.windowSize.Load(); got != beforeWin {
			t.Errorf("负载 %d 字节不得改动窗口：got=%d want=%d", n, got, beforeWin)
		}
		if got := m.metrics.Errors.Load(); got != beforeErr+1 {
			t.Errorf("负载 %d 字节应计为协议错误：Errors=%d want=%d", n, got, beforeErr+1)
		}
	}

	// 正常 4 字节帧仍然生效，且不计错误。
	beforeErr := m.metrics.Errors.Load()
	ok := make([]byte, 4)
	binary.BigEndian.PutUint32(ok, 1234)
	callHandlerNoPanic(t, "handleWindowUpdateFrame(4B)", 4, func() {
		handleWindowUpdateFrame(m, sid, ok)
	})
	if got, want := s.windowSize.Load(), beforeWin+1234; got != want {
		t.Errorf("4 字节正常帧应更新窗口：got=%d want=%d", got, want)
	}
	if got := m.metrics.Errors.Load(); got != beforeErr {
		t.Errorf("正常帧不得计入协议错误：Errors=%d want=%d", got, beforeErr)
	}
}

// TestFrameHandlers_ShortPayloadDoNotPanic 是**面向未来的不变量**：分发表里的每个处理器，
// 在负载长度 0..3 时都不得 panic（无论目标流是否存在）。新增处理器若忘了长度校验，本用例即红。
func TestFrameHandlers_ShortPayloadDoNotPanic(t *testing.T) {
	t.Parallel()
	m, _ := newHandlerTestMux(t)

	const knownSID = StreamID(21)
	m.mu.Lock()
	m.streams[knownSID] = newStream(knownSID, m)
	m.mu.Unlock()

	const unknownSID = StreamID(99)

	frameTypes := make([]FrameType, 0, len(frameHandlers))
	for ftype := range frameHandlers {
		frameTypes = append(frameTypes, ftype)
	}
	if len(frameTypes) < 8 {
		t.Fatalf("分发表只有 %d 个处理器，门禁自检失败（是否漏读 frameHandlers？）", len(frameTypes))
	}

	for _, ftype := range frameTypes {
		h := frameHandlers[ftype]
		for _, sid := range []StreamID{knownSID, unknownSID} {
			for n := 0; n <= 3; n++ {
				payload := make([]byte, n)
				name := "frameHandler(type=" + string(rune('0'+ftype)) + ")"
				callHandlerNoPanic(t, name, n, func() {
					h(m, sid, payload)
				})
			}
		}
	}
}

// TestMux_SurvivesShortWindowUpdateFrame 是**端到端**版本：目标流真实存在时把短负载窗口更新
// 当成对端发来的原始帧投递（走 readLoop → DecodeFrame → 处理器），断言连接仍可用。
//
// 为什么必须先 Open 一条流：处理器在「流不存在」时会提前 return，从而**掩盖**未校验负载的解析。
// 真实攻击者会先建流再发短帧，因此本用例必须命中「流存在」分支。
// 修复前该帧会让 readLoop panic ⇒ 整个测试进程崩溃（即缺陷的真实后果：远程可致进程死亡）。
func TestMux_SurvivesShortWindowUpdateFrame(t *testing.T) {
	t.Parallel()
	a, b := xfertest.Pipe()
	m := NewWithOpts(a, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	victim, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("建立受害流: %v", err)
	}
	defer func() { _ = victim.Close() }()

	before := m.Metrics().Errors.Load()
	short, err := EncodeFrame(victim.ID(), FrameWindowUpdate, []byte{0x01})
	if err != nil {
		t.Fatalf("编码短负载窗口更新帧: %v", err)
	}
	if err = b.Send(ctx, short); err != nil {
		t.Fatalf("投递原始帧: %v", err)
	}

	// 连接必须仍然可用：随后一次合法 Open 应能建立流并被 Accept。
	const newSID = StreamID(3)
	open, err := EncodeFrame(newSID, FrameOpen, nil)
	if err != nil {
		t.Fatalf("编码 Open 帧: %v", err)
	}
	if err = b.Send(ctx, open); err != nil {
		t.Fatalf("投递 Open 帧: %v", err)
	}

	s, err := m.Accept(ctx)
	if err != nil {
		t.Fatalf("短负载窗口更新后 mux 应仍可用: %v", err)
	}
	defer func() { _ = s.Close() }()
	if s.ID() != newSID {
		t.Fatalf("Accept 到错误的流: got=%d want=%d", s.ID(), newSID)
	}
	if got := m.Metrics().Errors.Load(); got <= before {
		t.Errorf("短负载窗口更新应计为协议错误：Errors=%d want >%d", got, before)
	}
}

// TestHandleWindowUpdateFrame_NonPositiveDeltaIgnored：窗口增量必须 **> 0** 才生效。
//
// 动机（2026-09-16 审计的可选加固，纵深防御）：合法编码侧只发 `size > 0`（窗口更新的构造见
// `retransmit.go`），而 `int32(binary.BigEndian.Uint32(...))` 会把 ≥ 2^31 的负载解释成**负数 delta**，
// 把发送窗口打成负（后续 `Write` 会一直在 `for ws <= 0` 里等到 done）。这类非正增量按协议错误
// 计数并丢弃，不改窗口、也不惊动等待中的写者。
//
// 注：对端保持沉默即可达到同样效果，故这不是新增 DoS，属纵深防御（不属本片必须项）。
func TestHandleWindowUpdateFrame_NonPositiveDeltaIgnored(t *testing.T) {
	t.Parallel()
	m, _ := newHandlerTestMux(t)

	const sid = StreamID(31)
	s := newStream(sid, m)
	m.mu.Lock()
	m.streams[sid] = s
	m.mu.Unlock()

	base := s.windowSize.Load()
	for _, tc := range []struct {
		name string
		val  uint32
	}{
		{"零增量", 0},
		{"负 delta（2^31）", 1 << 31},
		{"负 delta（2^32-1）", ^uint32(0)},
	} {
		beforeErr := m.metrics.Errors.Load()
		payload := make([]byte, 4)
		binary.BigEndian.PutUint32(payload, tc.val)
		callHandlerNoPanic(t, "handleWindowUpdateFrame("+tc.name+")", 4, func() {
			handleWindowUpdateFrame(m, sid, payload)
		})
		if got := s.windowSize.Load(); got != base {
			t.Errorf("%s：窗口不得变化 got=%d want=%d", tc.name, got, base)
		}
		if got := m.metrics.Errors.Load(); got != beforeErr+1 {
			t.Errorf("%s：应计为协议错误 Errors=%d want=%d", tc.name, got, beforeErr+1)
		}
	}

	// 正当的正增量仍生效（否则本加固会把正常窗口更新一并拒掉）。
	ok := make([]byte, 4)
	binary.BigEndian.PutUint32(ok, 7)
	beforeErr := m.metrics.Errors.Load()
	handleWindowUpdateFrame(m, sid, ok)
	if got, want := s.windowSize.Load(), base+7; got != want {
		t.Errorf("正增量应生效：got=%d want=%d", got, want)
	}
	if got := m.metrics.Errors.Load(); got != beforeErr {
		t.Errorf("正增量不得计为协议错误：Errors=%d want=%d", got, beforeErr)
	}
}
