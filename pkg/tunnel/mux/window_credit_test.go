// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"encoding/binary"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
)

// 本文件钉住「窗口信用不得静默丢失」。
//
// 动机（2026-09-16 独立审计的已确认代码路径）：`sendWindowUpdateUnsafe` 在 `writeCh` 打满时
// 走 `default:` **直接丢弃**窗口更新帧，且没有任何记账。而信用只会在「本侧再次消费到新负载」
// 时才补送 ⇒ 若本侧已读完全部数据（无更多负载可读），对端就永久卡在 `stream.Write` 的
// `for s.windowSize.Load() <= 0` 上：单流活性永久丧失，且无日志、无指标，极难排查。
//
// 修法见 retransmit.go：投递失败只记账（`stream.pendingWindowUpdate`），由 writeLoop 的 50ms
// ticker 经 `flushPendingWindowUpdates` 补送；补送用 CAS 取走并归零，投递再失败时**原样加回**
// ⇒ 每个字节的信用恰好交付一次（可延迟，不丢失、不重复）。
//
// 两个用例都不依赖固定 sleep：第一个把 writeLoop 确定性卡在 `conn.Send` 上以制造「writeCh 满」，
// 再用有界等待观测补送；第二个直接驱动 flushPendingWindowUpdates，无任何时序假设。

// blockingSendConn 是「可在测试指定时刻卡住 Send」的 mock 连接，并记录全部已发送帧。
// block 置真后 writeLoop 会停在 SendFn 内（entered 通知测试「已经卡住」），release 关闭后恢复。
type blockingSendConn struct {
	*mockxfer.MockConn

	block   atomic.Bool
	entered chan struct{}
	release chan struct{}

	releaseOnce sync.Once
	mu          sync.Mutex
	sent        [][]byte
}

func newBlockingSendConn() *blockingSendConn {
	c := &blockingSendConn{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	c.MockConn = &mockxfer.MockConn{
		SendFn: func(ctx context.Context, msg []byte) error {
			if c.block.Load() {
				select {
				case c.entered <- struct{}{}:
				default:
				}
				select {
				case <-c.release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			c.mu.Lock()
			c.sent = append(c.sent, append([]byte(nil), msg...))
			c.mu.Unlock()
			return nil
		},
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			// 连接空闲：readLoop 阻塞在 ctx 上，不消耗 CPU（也不与测试争抢流表）。
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	return c
}

// releaseGate 放行被卡住的 Send（幂等）。
func (c *blockingSendConn) releaseGate() { c.releaseOnce.Do(func() { close(c.release) }) }

// windowUpdateTotal 返回已发送帧中、针对 sid 的窗口更新**总量**（字节）。
func (c *blockingSendConn) windowUpdateTotal(sid StreamID) int32 {
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

// fullWriteChMux 建立 mux，并把 writeLoop 确定性卡在 `conn.Send` 上、随后把 `writeCh` 填满。
// 由于此时没有任何消费者，填满后队列保持满 ⇒ 调用方对 `sendWindowUpdateUnsafe` 的每一次调用
// 都必然走「投递失败」路径（这正是要测的分支）。
func fullWriteChMux(t *testing.T, c *blockingSendConn) (*Mux, *stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	m := New(c, RoleDialer)
	t.Cleanup(func() {
		c.releaseGate()
		_ = m.Close()
	})

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// ① 卡住 writeLoop 的下一次 Send：此后 writeCh 无人消费。
	c.block.Store(true)
	filler, encErr := EncodeFrame(0, FramePing, nil)
	if encErr != nil {
		t.Fatalf("编码填充帧: %v", encErr)
	}
	select {
	case m.writeCh <- writeMsg{streamID: 0, data: filler, isRaw: true}:
	case <-ctx.Done():
		t.Fatal("writeCh 意外不可写（测试前置失败）")
	}
	select {
	case <-c.entered:
	case <-ctx.Done():
		t.Fatal("writeLoop 未在预期内卡住 Send（测试前置失败）")
	}

	// ② 填满 writeCh：writeLoop 已卡住 ⇒ 队列不会被消费，填满后保持满。
fill:
	for {
		select {
		case m.writeCh <- writeMsg{streamID: 0, data: filler, isRaw: true}:
		default:
			break fill
		}
	}
	return m, s.(*stream)
}

// TestWindowUpdate_DroppedCreditIsResent：writeCh 满时被挤掉的窗口信用必须补送
// （修复前：永久丢失 ⇒ 对端窗口永久少这一笔）。
func TestWindowUpdate_DroppedCreditIsResent(t *testing.T) {
	t.Parallel()
	c := newBlockingSendConn()
	_, s := fullWriteChMux(t, c)

	const payload = 4096
	for range 2 {
		s.pushData(make([]byte, payload)) // 模拟 readLoop 投递数据帧
		buf := make([]byte, payload)
		n, err := s.Read(buf)
		if err != nil {
			t.Fatalf("已投递负载的 Read 不应失败: %v", err)
		}
		if n != payload {
			t.Fatalf("Read n=%d want %d", n, payload)
		}
	}

	// 前置自检：队列仍是满的（无消费者）⇒ 两条窗口更新都只能走「记账」路径，尚未发出。
	if got := c.windowUpdateTotal(s.ID()); got != 0 {
		t.Fatalf("前置失败：writeCh 满时窗口更新不应已发出, got %d", got)
	}

	c.releaseGate() // 放行 writeLoop：此后 ticker 才有机会补送

	const want = 2 * payload
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return c.windowUpdateTotal(s.ID()) >= want
	}, "writeCh 满时被挤掉的窗口信用必须补送：否则对端窗口永久少这一笔，其 Write 会一直等到流被拆除")

	if got := c.windowUpdateTotal(s.ID()); got != want {
		t.Errorf("补送的信用总量=%d want %d（不得缺失，也不得重复计数）", got, want)
	}
}

// TestWindowUpdate_PendingCreditFlushedExactlyOnce：白盒钉住补送的两条性质——
// ① 多次被挤掉的信用**合并**为一条帧补送（不重复计数）；② 待补送量归零后不得再补送。
//
// 本用例驱动的是修复引入的内部函数（修复前不存在 ⇒ 红灯形态是编译失败），
// 因此不依赖 ticker，也完全没有时序假设。
func TestWindowUpdate_PendingCreditFlushedExactlyOnce(t *testing.T) {
	t.Parallel()
	c := newBlockingSendConn()
	m, s := fullWriteChMux(t, c)

	const payload = 1024
	for range 2 {
		s.pushData(make([]byte, payload))
		if n, err := s.Read(make([]byte, payload)); err != nil || n != payload {
			t.Fatalf("已投递负载的 Read n=%d err=%v want n=%d", n, err, payload)
		}
	}

	// 手工排空 writeCh（writeLoop 仍卡在 Send 上，不会与测试争抢）。
	drained := 0
empty:
	for {
		select {
		case <-m.writeCh:
			drained++
		default:
			break empty
		}
	}
	if drained == 0 {
		t.Fatal("前置失败：writeCh 应为满（被挤掉的信用才会走记账路径）")
	}

	if got, want := s.pendingWindowUpdate.Load(), int32(2*payload); got != want {
		t.Fatalf("被挤掉的信用应记成待补送 %d, got %d", want, got)
	}

	m.flushPendingWindowUpdates()

	if got := s.pendingWindowUpdate.Load(); got != 0 {
		t.Errorf("补送后待补送量应归零, got %d", got)
	}

	var flushed [][]byte
collected:
	for {
		select {
		case msg := <-m.writeCh:
			flushed = append(flushed, msg.data)
		default:
			break collected
		}
	}
	if len(flushed) != 1 {
		t.Fatalf("补送应合并为恰好一条帧, got %d 条", len(flushed))
	}
	sid, ftype, pl, err := DecodeFrame(flushed[0])
	if err != nil || ftype != FrameWindowUpdate || sid != s.ID() || len(pl) < windowUpdateLen {
		t.Fatalf("补送的帧应为流 %d 的窗口更新, got sid=%d type=%d err=%v", s.ID(), sid, ftype, err)
	}
	if got, want := int32(binary.BigEndian.Uint32(pl[:windowUpdateLen])), int32(2*payload); got != want {
		t.Errorf("补送金额=%d want %d", got, want)
	}

	// 已归零：再 flush 不得重复补送。
	m.flushPendingWindowUpdates()
	select {
	case msg := <-m.writeCh:
		t.Errorf("待补送量已为 0 时不得再补送, got 帧%v", msg.data)
	default:
	}
}

// TestWindowUpdate_ResendFailureKeepsCreditPending：**补送本身再次投递失败**时，该笔信用必须
// 原样加回待补送量（等下一次 tick），不得清零丢弃。
//
// 覆盖缺口（2026-09-16 独立复核的 M5 变异）：删掉 flushPendingWindowUpdates 里投递失败后的
// `s.pendingWindowUpdate.Add(size)`（即「原样加回」）时，其余用例**全部仍绿**——而那条路径
// 正是本修复承诺的「投递再失败也不丢账」。本用例是唯一守着该保证的用例：writeCh 仍满时
// 手工驱动 flush，断言待补送量**未被清零**，放行后仍恰好送达一次。
func TestWindowUpdate_ResendFailureKeepsCreditPending(t *testing.T) {
	t.Parallel()
	c := newBlockingSendConn()
	m, s := fullWriteChMux(t, c)

	const payload = 2048
	for range 2 {
		s.pushData(make([]byte, payload))
		if n, err := s.Read(make([]byte, payload)); err != nil || n != payload {
			t.Fatalf("已投递负载的 Read n=%d err=%v want n=%d", n, err, payload)
		}
	}

	// 前置：两笔信用都只能走记账路径（writeCh 满；writeLoop 卡在 Send 上 ⇒ ticker 不会并发补送）。
	if got, want := s.pendingWindowUpdate.Load(), int32(2*payload); got != want {
		t.Fatalf("前置失败：待补送量=%d want %d（writeCh 应为满）", got, want)
	}

	// writeCh 仍满 ⇒ 补送也投递不出去：必须原样加回，不得清零。
	m.flushPendingWindowUpdates()
	if got, want := s.pendingWindowUpdate.Load(), int32(2*payload); got != want {
		t.Fatalf("补送投递失败后待补送量=%d want %d：信用被清零即永久丢失该笔信用，对端窗口再也不会恢复", got, want)
	}
	if got := c.windowUpdateTotal(s.ID()); got != 0 {
		t.Fatalf("writeCh 满时不得发出任何信用, got %d", got)
	}

	// 放行 writeLoop 后必须最终送达，且恰好一次（不缺失、不重复计数）。
	c.releaseGate()
	const want = 2 * payload
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return c.windowUpdateTotal(s.ID()) >= want
	}, "补送再次失败后必须重试成功：否则对端窗口永久少这一笔")

	if got := c.windowUpdateTotal(s.ID()); got != want {
		t.Errorf("补送总量=%d want %d（不得缺失，也不得重复计数）", got, want)
	}
	if got := s.pendingWindowUpdate.Load(); got != 0 {
		t.Errorf("补送成功后待补送量应归零, got %d", got)
	}
}
