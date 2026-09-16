// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件钉住 2026-09-16 审计 **F2（readLoop 级头阻塞）的主体修复**：帧不再阻塞投递。
//
// 根因（量纲错配）：`readLoop` 是**单 goroutine 串行**处理所有帧（loop.go），而
// `handleDataFrame → pushData` 原先是**无 default、无超时**的通道投递；`dataCh` 容量按**帧**
// 计（64）而流控窗口按**字节**计（DefaultWindowSize=65536）⇒ 对端在窗口内以小帧（平均 <1 KiB）
// 写时**第 65 帧即阻塞 readLoop** ⇒ 整条 mux（含其它健康流）冻结，并于 90–120s 后被本侧
// pingLoop 的 90s 心跳超时把**整条连接**拆掉（hub 拓扑下一条 mux = 一个节点的全部用户流量）。
//
// 修复语义：
//   - dataCh 满 ⇒ 帧落入**溢出缓冲**（FIFO，保序），**绝不阻塞 readLoop**；
//   - 溢出缓冲以**窗口为上界**（守协议对端的未确认字节 ≤ DefaultWindowSize）；
//   - 超出上界即判定**对端违反流控** ⇒ 只 Abort 该流（fail-closed）+ 计数，连接与其它流存活。
//
// 所有用例都是确定性的：帧输入由测试直接驱动（或经内存 pipe 精确投喂），
// 等待一律用 testutil.WaitFor / 有界 ctx，**不使用固定 time.Sleep**。

// pushFramesAsync 在后台 goroutine 里推入 n 帧（每帧 payloadLen 字节，内容为 byte(i)），
// 返回 "全部返回" 的 done channel。
//
// 为何用 goroutine：修复前 pushData 在 dataCh 满时会**阻塞**（生产者即 readLoop），直接调用会
// 让用例挂死；放进 goroutine 后，红灯表现为「done 一直不关闭」这一可读失败（有 WaitFor 上限），
// 而不是不可诊断的挂死。
func pushFramesAsync(ss *stream, n, payloadLen int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			ss.pushData(bytes.Repeat([]byte{byte(i)}, payloadLen))
		}
	}()
	return done
}

// waitPushesReturned 等待后台推帧全部返回，失败信息直指 F2 根因。
func waitPushesReturned(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	testutil.WaitFor(t, 10*time.Second, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, what)
}

// readFramesInOrder 按帧读回 n 帧并逐帧校验内容为 byte(i)，长度必须等于 payloadLen。
func readFramesInOrder(t *testing.T, ss *stream, n, payloadLen int) {
	t.Helper()
	rbuf := make([]byte, payloadLen)
	for i := range n {
		got, err := ss.Read(rbuf)
		if err != nil {
			t.Fatalf("Read(第 %d 帧): %v", i, err)
		}
		if got != payloadLen {
			t.Fatalf("Read(第 %d 帧) 返回长度=%d want %d", i, got, payloadLen)
		}
		if want := bytes.Repeat([]byte{byte(i)}, payloadLen); !bytes.Equal(rbuf, want) {
			t.Fatalf("第 %d 帧内容错乱（丢帧或乱序）", i)
		}
	}
}

// windowUpdateSumOf 返回 mock 连接已发出帧中、针对 sid 的窗口更新**总量**（字节）。
func windowUpdateSumOf(c *observingConn, sid StreamID) int32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int32
	for _, raw := range c.sent {
		gotSID, ftype, payload, err := DecodeFrame(raw)
		if err != nil || ftype != FrameWindowUpdate || gotSID != sid || len(payload) < windowUpdateLen {
			continue
		}
		total += int32(binary.BigEndian.Uint32(payload[:windowUpdateLen]))
	}
	return total
}

// TestStreamOverflow_ReadLoopNotBlockedByStalledStream 是本修复的**核心端到端证据**：
// 一条流「合规但停读」时，readLoop 必须仍能处理**其它流**的帧。
//
// 构造（全部确定性，无 sleep）：
//  1. 建立真实 mux 对（xfertest.Pipe），开两条流 s1/s2 并各自 Accept；
//  2. 对端在**不超窗口**的前提下用小帧写 s1：512 B × 128 = 65536 字节（恰好一个窗口），
//     而消费侧**完全不读 s1**；
//  3. 等「dataCh 满」这一事实发生（ReadLoopPush.Waits ≥ 1）——修复前该时刻 readLoop 已卡在
//     pushData 上；
//  4. 在 s2 上写数据并断言**能被读到**：修复前 s2 的帧永远不会被处理（readLoop 停摆），本步超时红。
func TestStreamOverflow_ReadLoopNotBlockedByStalledStream(t *testing.T) {
	t.Parallel()
	const frameLen = 512
	// 128 帧 × 512 B = 65536 = 恰好一个窗口（守协议的对端在无信用前最多发这么多）。
	const frames = int(DefaultWindowSize) / frameLen

	cc, sc := xfertest.Pipe()
	t.Cleanup(func() { _ = cc.Close(); _ = sc.Close() })
	dialer := New(cc, RoleDialer)
	listener := New(sc, RoleListener)
	t.Cleanup(func() { _ = dialer.Close(); _ = listener.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s1, err := dialer.Open(ctx)
	if err != nil {
		t.Fatalf("dialer.Open(s1): %v", err)
	}
	t.Cleanup(func() { _ = s1.Abort() })
	a1, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("listener.Accept(s1): %v", err)
	}
	t.Cleanup(func() { _ = a1.Abort() })

	s2, err := dialer.Open(ctx)
	if err != nil {
		t.Fatalf("dialer.Open(s2): %v", err)
	}
	t.Cleanup(func() { _ = s2.Abort() })
	a2, err := listener.Accept(ctx)
	if err != nil {
		t.Fatalf("listener.Accept(s2): %v", err)
	}
	t.Cleanup(func() { _ = a2.Abort() })

	// s1：小帧写满一个窗口（dialer 侧窗口恰好够，全部立即成功）。
	payload := make([]byte, frameLen)
	for i := range frames {
		for j := range payload {
			payload[j] = byte(i)
		}
		if _, err := s1.Write(payload); err != nil {
			t.Fatalf("s1.Write(第 %d 帧): %v", i, err)
		}
	}

	// 等「dataCh 满」真的发生：进入次数在进入阻塞路径时即 +1（修复前就是卡住的那一刻）。
	testutil.WaitFor(t, 20*time.Second, func() bool {
		return listener.Metrics().ReadLoopPush.Waits.Load() >= 1
	}, "前置失败：未观测到 dataCh 满，用例没有覆盖到会阻塞 readLoop 的路径")

	// 核心断言：另一条流的数据必须仍被处理。
	if _, err := s2.Write([]byte("hello")); err != nil {
		t.Fatalf("s2.Write: %v", err)
	}
	read2 := make(chan string, 1)
	go func() {
		buf := make([]byte, 32)
		n, err := a2.Read(buf)
		if err != nil && err != io.EOF {
			return
		}
		read2 <- string(buf[:n])
	}()
	testutil.WaitFor(t, 20*time.Second, func() bool {
		select {
		case got := <-read2:
			return got == "hello"
		default:
			return false
		}
	}, "停读流不得阻塞 readLoop：另一条流的数据必须仍能送达（修复前 readLoop 卡在 pushData 上，本步必然超时）")

	// 停读流的数据**不丢且保序**（溢出缓冲是 FIFO）。
	readFramesInOrder(t, a1.(*stream), frames, frameLen)

	// 守协议对端不得被误判违约。
	if got := listener.Metrics().StreamOverflowSpills.Load(); got == 0 {
		t.Errorf("溢出发生但 StreamOverflowSpills=0（观测缺失：dataCh 已满，帧必须走溢出缓冲）")
	}
	if got := listener.Metrics().StreamWindowViolations.Load(); got != 0 {
		t.Errorf("守协议对端（恰好一个窗口、未超信用）不得被判违约, got %d", got)
	}
}

// TestStreamOverflow_SpillDoesNotBlockAndPreservesOrder：dataCh 满后帧落入溢出缓冲，
// 生产者（生产中即 readLoop）**立即返回**，且数据保序。
func TestStreamOverflow_SpillDoesNotBlockAndPreservesOrder(t *testing.T) {
	t.Parallel()
	const frameLen = 512
	// 70 > dataCh 容量 64 ⇒ 第 65 帧起必须走溢出缓冲。
	const frames = 70

	m, _ := newTestMux(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	done := pushFramesAsync(ss, frames, frameLen)
	waitPushesReturned(t, done, "pushData 不得阻塞 readLoop：dataCh 满（容量 64 帧）时应落入溢出缓冲，而不是等待读者")

	readFramesInOrder(t, ss, frames, frameLen)

	// dataCh 是**帧数量纲**的通道：修复后它仍然最多被填满，溢出部分不再阻塞、也不再堆积在那里。
	if got, want := m.Metrics().DataChMaxFrames.Load(), int64(cap(ss.dataCh)); got != want {
		t.Errorf("dataCh 峰值帧数=%d want %d", got, want)
	}
	// 「已收未消费字节峰值」必须真的记到（它是与窗口可比的量纲，DataChMaxFrames 不能替代）。
	if got, want := m.Metrics().MaxBufferedBytes.Load(), int64(frames*frameLen); got < want {
		t.Errorf("已收未消费字节峰值=%d want ≥%d（观测缺失）", got, want)
	}
}

// TestStreamOverflow_OrderPreservedWhenReaderFreesSlotsBetweenPushes 钉住溢出缓冲的
// **顺序不变式**：一旦 pending 尚有未消费项，后续帧必须继续追加 pending，不得因为读者刚腾出
// 一个 dataCh 槽位就把它塞回 dataCh——因为 Read 先取 dataCh，那会让「后到的帧」先于
// 「更早到达但已溢出的帧」交付（字节流乱序，上层分块加密会直接认证失败）。
func TestStreamOverflow_OrderPreservedWhenReaderFreesSlotsBetweenPushes(t *testing.T) {
	t.Parallel()
	m, _ := newTestMux(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	// 填满 dataCh（帧号 = 内容，1 字节），再溢出 1 帧。
	for i := range cap(ss.dataCh) {
		ss.pushData([]byte{byte(i)})
	}
	ss.pushData([]byte{byte(cap(ss.dataCh))}) // 第 65 帧 ⇒ 溢出

	// 读走最旧的一帧（腾出 dataCh 一个槽位）。
	buf := make([]byte, 1)
	if _, err := ss.Read(buf); err != nil || buf[0] != 0 {
		t.Fatalf("读第一帧失败: n=%d buf=%v err=%v", 1, buf, err)
	}

	// 此时 dataCh 有 63 帧、pending 有 1 帧；再推一帧（更晚到达）——它必须排在 pending 尾部。
	ss.pushData([]byte{byte(cap(ss.dataCh) + 1)})

	// 全部 66 帧必须按到达顺序交付。
	for i := 1; i <= cap(ss.dataCh)+1; i++ {
		if _, err := ss.Read(buf); err != nil {
			t.Fatalf("Read(第 %d 帧): %v", i, err)
		}
		if buf[0] != byte(i) {
			t.Fatalf("第 %d 帧内容=%d（乱序：溢出缓冲必须保持 FIFO）", i, buf[0])
		}
	}
}

// TestStreamOverflow_EOFOrderedAfterSpilledData：EOF（FrameCloseWrite）在溢出模式下也必须
// **排在已溢出数据之后**（FIFO），不得因为走了不同队列而被提前交付或丢失。
func TestStreamOverflow_EOFOrderedAfterSpilledData(t *testing.T) {
	t.Parallel()
	const frameLen = 512
	const frames = 66 // > 64 ⇒ 至少 2 帧溢出，EOF 也必然溢出

	m, _ := newTestMux(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range frames {
			ss.pushData(bytes.Repeat([]byte{byte(i)}, frameLen))
		}
		ss.pushEOF() // 数据之后立即半关闭（对端可能已发送数据后立即 FrameCloseWrite）
	}()
	waitPushesReturned(t, done, "溢出模式下的 pushEOF 同样不得阻塞 readLoop")

	readFramesInOrder(t, ss, frames, frameLen)

	if _, err := ss.Read(make([]byte, 16)); err != io.EOF {
		t.Fatalf("EOF 必须排在溢出数据之后：got err=%v want io.EOF", err)
	}
}

// TestStreamOverflow_CreditIssuedExactlyOncePerFrame：信用只为**被取走的帧**补发一次。
// 溢出缓冲不改变信用语义：数据帧从溢出缓冲被取走时，必须与从 dataCh 取走时一样补发等长信用
// （漏发 ⇒ 对端窗口永久少一笔；重发 ⇒ 对端窗口虚增）。这条与 #306 的
// 「窗口更新不得静默丢失」是同一条不变量的另一侧。
func TestStreamOverflow_CreditIssuedExactlyOncePerFrame(t *testing.T) {
	t.Parallel()
	const frameLen = 512
	const frames = 70 // > 64 ⇒ 跨过溢出边界

	m, c := newTestMux(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	done := pushFramesAsync(ss, frames, frameLen)
	waitPushesReturned(t, done, "pushData 不得阻塞 readLoop（跨溢出边界）")

	readFramesInOrder(t, ss, frames, frameLen)

	// 每帧被取走一次 ⇒ 恰好 frames*frameLen 字节信用（经 writeCh 异步出线，故用 WaitFor 等齐）。
	want := int32(frames * frameLen)
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return windowUpdateSumOf(c, ss.id) == want
	}, "信用必须恰好按被取走的帧字节补发一次（含从溢出缓冲取走的帧）")

	if got := m.Metrics().StreamWindowViolations.Load(); got != 0 {
		t.Errorf("守协议对端不得被判违约, got %d", got)
	}
}

// TestStreamOverflow_WindowViolationAbortsOnlyThatStream：对端**超出其应守窗口**（不是「读得慢」，
// 那是合规的）时判定违约：只终止该流（fail-closed），**连接与其它流必须存活**。
//
// 3 × 65535 = 196605 字节 > 窗口 65536 + 一帧余量 65535 ⇒ 只有「对端超窗口灌数据」能解释。
func TestStreamOverflow_WindowViolationAbortsOnlyThatStream(t *testing.T) {
	t.Parallel()
	m, _ := newTestMux(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)
	frame := bytes.Repeat([]byte{7}, MaxFramePayload)
	for range 3 {
		ss.pushData(frame) // 生产中由 readLoop 调用；不阻塞
	}

	// 违约 ⇒ 该流必须被 Abort（注销出流表）。修复前帧只是排队（dataCh 容量 64 帧），流安然无恙。
	if _, ok := m.streams[ss.id]; ok {
		t.Fatalf("对端超出流控窗口后必须 Abort 该流（注销），但它仍在流表中")
	}
	if got := m.Metrics().StreamWindowViolations.Load(); got != 1 {
		t.Errorf("违约计数=%d want 1（该指标是违约的唯一可观测出口）", got)
	}

	// 连接必须存活（fail-closed 只针对该流）。
	select {
	case <-m.Done():
		t.Fatal("违约只应终止该流，不得关闭整条 mux")
	default:
	}

	// 已排队的合规字节仍可读出，之后该流终止（不静默把违约帧交给应用）。
	buf := make([]byte, MaxFramePayload)
	if _, rerr := ss.Read(buf); rerr != nil {
		t.Fatalf("违约前已入队的合规帧应仍可读出: %v", rerr)
	}
	if _, rerr := ss.Read(buf); rerr != nil {
		t.Fatalf("第二帧（合规）应仍可读出: %v", rerr)
	}
	if _, rerr := ss.Read(buf); rerr == nil {
		t.Fatal("违约后该流必须终止：Read 不得返回数据")
	}

	// 同 mux 的另一条流不受影响（连接与其它流存活）。
	s2, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("违约后同 mux 的 Open 失败（连接被误伤）: %v", err)
	}
	ss2 := s2.(*stream)
	ss2.pushData([]byte("hello"))
	got := make([]byte, 32)
	n, err := ss2.Read(got)
	if err != nil {
		t.Fatalf("同 mux 的另一条流读取失败（连接被误伤）: %v", err)
	}
	if string(got[:n]) != "hello" {
		t.Fatalf("同 mux 的另一条流数据错乱: got %q want %q", got[:n], "hello")
	}
}

// TestStreamOverflow_ZeroByteFramesDoNotGrowUnbounded：**条目数**也必须有界——对端可以反复发
// 0 长度 FrameData（不增字节、只增条目）或重复 FrameCloseWrite，若只看字节判据则溢出缓冲仍可被
// 远程无界增长。这里同时钉两件事：① 重复 EOF 标记幂等去重，不得把条目数推高、也不得误判违约；
// ② 零字节帧涌填至条目上界后判违约（只 Abort 该流）。
func TestStreamOverflow_ZeroByteFramesDoNotGrowUnbounded(t *testing.T) {
	t.Parallel()

	t.Run("重复 EOF 标记去重且仍保序", func(t *testing.T) {
		t.Parallel()
		m, _ := newTestMux(t)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		t.Cleanup(cancel)
		s, err := m.Open(ctx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		ss := s.(*stream)

		// 填满 dataCh 后进入溢出模式，再连发大量 EOF 标记（对端异常行为）。
		for i := range cap(ss.dataCh) {
			ss.pushData([]byte{byte(i)})
		}
		for range 1000 {
			ss.pushEOF()
		}

		ss.pendingMu.Lock()
		entries := len(ss.pending) - ss.pendingOff
		ss.pendingMu.Unlock()
		if entries > 1 {
			t.Fatalf("重复 EOF 标记必须去重：溢出缓冲条目数=%d want <=1", entries)
		}
		if got := m.Metrics().StreamWindowViolations.Load(); got != 0 {
			t.Fatalf("重复 EOF 标记不得被判违约（语义幂等）, got %d", got)
		}

		// 数据仍然保序，紧接恰好一个 EOF。
		readFramesInOrder(t, ss, cap(ss.dataCh), 1)
		if _, rerr := ss.Read(make([]byte, 1)); rerr != io.EOF {
			t.Fatalf("got err=%v want io.EOF", rerr)
		}
	})

	t.Run("零字节帧涌填至条目上界即判违约", func(t *testing.T) {
		t.Parallel()
		m, _ := newTestMux(t)
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		t.Cleanup(cancel)
		s, err := m.Open(ctx)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		ss := s.(*stream)

		// 填满 dataCh（占用 64 条），其余零字节帧全部溢入 pending。
		for i := range cap(ss.dataCh) {
			ss.pushData([]byte{byte(i)})
		}
		empty := []byte{}
		for range pendingEntryLimit + 2 {
			ss.pushData(empty) // 0 长度数据帧：不增 buffered 字节
		}

		ss.pendingMu.Lock()
		entries := len(ss.pending) - ss.pendingOff
		ss.pendingMu.Unlock()
		if entries > pendingEntryLimit {
			t.Fatalf("溢出缓冲条目数=%d 超出上界 %d（远程可用零字节帧无界增长内存）", entries, pendingEntryLimit)
		}
		if got := m.Metrics().StreamWindowViolations.Load(); got != 1 {
			t.Fatalf("零字节帧涌填越过条目上界必须判违约, violations=%d want 1", got)
		}
		if _, ok := m.streams[ss.id]; ok {
			t.Fatal("违约后该流必须被注销")
		}
	})
}

// TestStreamOverflow_CompliantWindowBoundaryNotAborted：**合规上界不得被误判违约**。
//
// 边界依据（协议安全）：本侧只为「被取走的帧」补发等长信用，故守协议对端未确认字节 ≤ 窗口；
// 而信用在「取帧」时即发放（帧可能尚未被应用读完）⇒ 本侧持有量上界 = 窗口 + 一帧。
// 本用例恰好压到这个上界（2 × 65535 = 131070 ≤ 65536 + 65535），断言不违约、数据可读，
// 且消费后仍可继续接收（不会因为一次接近上界就永久拒绝该流）。
func TestStreamOverflow_CompliantWindowBoundaryNotAborted(t *testing.T) {
	t.Parallel()
	const frameLen = MaxFramePayload
	const frames = 2 // 131070 字节 = 窗口 + 一帧余量 - 1（合规可达的最大持有量）

	m, _ := newTestMux(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	done := pushFramesAsync(ss, frames, frameLen)
	waitPushesReturned(t, done, "合规上界内的推送不得阻塞")

	if got := m.Metrics().StreamWindowViolations.Load(); got != 0 {
		t.Fatalf("合规对端（持有量 = 窗口 + 一帧 - 1）不得被判违约, got %d", got)
	}
	if _, ok := m.streams[ss.id]; !ok {
		t.Fatal("合规对端不得被 Abort（流被误注销）")
	}

	readFramesInOrder(t, ss, frames, frameLen)

	// 消费后必须还能继续接收（上界判定不得把流永久打上标记）。
	done2 := pushFramesAsync(ss, 1, frameLen)
	waitPushesReturned(t, done2, "消费后应可继续接收")
	if got := m.Metrics().StreamWindowViolations.Load(); got != 0 {
		t.Fatalf("消费后继续接收不得被判违约, got %d", got)
	}
	readFramesInOrder(t, ss, 1, frameLen)
}

// TestStreamOverflow_ExactLimitNotAborted：**恰好等于上界**（131071 = 窗口 + 一帧）也不得误判。
//
// 判据是 `buffered + len(payload) > pendingOverflowLimit`（严格大于）⇒ 等界必须放行。既有
// CompliantWindowBoundaryNotAborted 只压到上界 - 1（2 × 65535 = 131070）；等界同样可被对端
// 构造（如 65535 + 32768 + 32768），且它是判据的**边界值**，被 `>` 误写成 `>=` 时只有本例会红。
func TestStreamOverflow_ExactLimitNotAborted(t *testing.T) {
	t.Parallel()
	// 65535 + 32768 + 32768 = 131071 = pendingOverflowLimit（严格等界）。
	frameLens := []int{MaxFramePayload, 32768, 32768}
	total := 0
	for _, n := range frameLens {
		total += n
	}
	if int64(total) != pendingOverflowLimit {
		t.Fatalf("前置：本用例必须恰好压在等界上, total=%d limit=%d", total, pendingOverflowLimit)
	}

	m, _ := newTestMux(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	for i, n := range frameLens {
		ss.pushData(make([]byte, n))
		if got := m.Metrics().StreamWindowViolations.Load(); got != 0 {
			t.Fatalf("第 %d 帧后恰好等界（%d ≤ limit）不得判违约, got %d", i+1, total, got)
		}
	}
	if _, ok := m.streams[ss.id]; !ok {
		t.Fatal("恰好等界不得被 Abort（流被误注销）")
	}

	// 等界内的数据必须完整可读（各自长度正确）。
	buf := make([]byte, MaxFramePayload)
	for i, want := range frameLens {
		n, readErr := ss.Read(buf)
		if readErr != nil {
			t.Fatalf("读第 %d 帧: %v", i+1, readErr)
		}
		if n != want {
			t.Fatalf("第 %d 帧读出长度=%d want %d", i+1, n, want)
		}
	}
}

// TestStreamOverflow_BackingArrayBoundedUnderSustainedSpill 钉住溢出缓冲**底层数组**的有界性。
//
// 为何需要独立断言：`pendingOverflowLimit` 约束的是**未消费量**，而 append 不回收已消费前缀，
// `tryPull` 又只在整条队列排空时才整体释放 ⇒ 若对端长期保持溢出（读得比写得快，但队列从不
// 排空），底层数组会随**传输总量**线性增长（复核探针：push + Read 各 20 万轮 ⇒ cap 达 224256，
// 约 5.4 MB，仅排空时才释放）——这与「溢出缓冲天然有界」的结论不符：有界的是未消费部分。
//
// 为何不能只断言 len：len 是**未消费条目数**，本来就有界（≤ pendingEntryLimit），出问题的正是
// 已消费前缀所占的底层数组 ⇒ 必须读 cap（本文件与实现同包，可直接观察，无需暴露探针）。
//
// 两个情形都覆盖：浅队列（存活 ≈ dataCh 容量，即复核探针场景）与**深队列**（存活 ≈ 条目上界，
// 数组峰值的理论最坏点）。
func TestStreamOverflow_BackingArrayBoundedUnderSustainedSpill(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		live     int // 常驻未消费条目数（全程保持不变：每轮取一推一）
		rounds   int
		frameLen int
	}{
		// 浅队列：与复核探针同形（存活 64 条）；无压缩时 20 万轮后 cap=224256。
		{"浅队列", 64, 200000, 64},
		// 深队列：存活接近条目上界（65538）⇒ 数组峰值最大；这里 30000 存活 + 1 B 帧，
		// 同时处在「字节远未满（< 131071）但条目数很高」的真实小帧病态区。
		{"深队列", 30000, 300000, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, _ := newTestMux(t)
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
			t.Cleanup(cancel)

			s, err := m.Open(ctx)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			ss := s.(*stream)
			peak, liveEnd := driveSustainedSpill(t, ss, tc.live, tc.rounds, tc.frameLen)

			if got := m.Metrics().StreamWindowViolations.Load(); got != 0 {
				t.Fatalf("全程合规（每轮消费后立即补推，buffered 恒小）不得判违约, got %d", got)
			}
			t.Logf("rounds=%d 常驻条目=%d ⇒ 底层数组峰值 cap=%d", tc.rounds, liveEnd, peak)

			// 判据 1（硬上界）：峰值不得超出「条目上界 + 压缩阈值」的同阶范围
			// （容忍切片增长的超额分配）。
			if maxCap := 2 * (pendingEntryLimit + pendingCompactOff); peak > maxCap {
				t.Fatalf("底层数组峰值 cap=%d 超出设计上界 %d（条目上界 %d + 压缩阈值 %d 的同阶范围）",
					peak, maxCap, pendingEntryLimit, pendingCompactOff)
			}
			// 判据 2（与传输量无关）：数组同时只持有「常驻条目」与「自上次压缩以来被消费的条目
			// （< pendingCompactOff）」⇒ 峰值必须与常驻量/阈值同阶，而不随 rounds 增长。
			// ×3 是给 append 超额分配与边界留的余量（实测值见日志）。
			if maxCap := 3 * (liveEnd + pendingCompactOff); peak > maxCap {
				t.Fatalf("底层数组峰值 cap=%d 超出 %d=3×（常驻 %d + 压缩阈值 %d）⇒ 已消费前缀未被压缩，"+
					"数组会随传输总量线性增长", peak, maxCap, liveEnd, pendingCompactOff)
			}
		})
	}
}

// driveSustainedSpill 让流进入「持续溢出」模式（dataCh 满 + pending 非空，且此后不排空），
// 然后跑 rounds 轮「取走一帧 + 推入一帧」，返回底层数组容量峰值与结束时的常驻条目数。
//
// 队列形状（已由用例断言）：前 cap(dataCh) 轮的取帧来自 dataCh（被取空），而推入帧一律进 pending
// ⇒ 结束时常驻条目数 = live + cap(dataCh)，全程不变。
//
// 为何直接驱动 tryPull 而不调 Read：tryPull 就是 Read 的取帧函数（含 pendingOff 推进与压缩），
// 本用例只需覆盖该路径；用 Read 会额外产生数十万条窗口更新帧，而测试替身 observingConn 会
// **逐帧留存**它们（`:43-45`）⇒ 那是替身开销，不是被测语义。这里同步补上 Read 对 buffered 的
// 记账（`Read`：`s.buffered.Add(-int64(n))`），保持违约判据的账一致。
func driveSustainedSpill(t *testing.T, ss *stream, live, rounds, frameLen int) (peak, liveEnd int) {
	t.Helper()
	// 先填满 dataCh（容量 64），再把 live 帧推入溢出缓冲（此后 pending 非空 ⇒ 一律追加 pending）。
	for range cap(ss.dataCh) {
		ss.pushData(make([]byte, frameLen))
	}
	for len(ss.pending)-ss.pendingOff < live {
		ss.pushData(make([]byte, frameLen))
	}
	if ss.pendingOff >= len(ss.pending) {
		t.Fatalf("前置失败：应已进入溢出模式（pending 非空），len=%d off=%d", len(ss.pending), ss.pendingOff)
	}

	for i := range rounds {
		_, kind := ss.tryPull()
		if kind == pullNone {
			t.Fatalf("第 %d 轮：队列不应为空（本用例必须全程处于溢出模式）", i)
		}
		ss.buffered.Add(-int64(frameLen))
		ss.pushData(make([]byte, frameLen))
		if c := cap(ss.pending); c > peak {
			peak = c
		}
	}

	// 队列形状校验：保持「常驻条目恒定」才说明本用例真的落在了预期的持续溢出模式里。
	liveEnd = len(ss.pending) - ss.pendingOff
	if want := live + cap(ss.dataCh); liveEnd != want {
		t.Fatalf("结束时常驻条目=%d want %d（驱动必须保持队列形状，否则未覆盖预期场景）", liveEnd, want)
	}
	return peak, liveEnd
}
