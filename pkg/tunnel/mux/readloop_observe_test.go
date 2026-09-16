// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
)

// 本文件钉住 2026-09-16 审计「readLoop 头阻塞」的两项改造与配套观测：
//
//   - **同源阻塞口②**：`handlePingFrame` 原先在 readLoop 内**同步** `conn.Send` 回 Pong ——
//     发送一慢/一卡就停摆 readLoop，本侧随即读不到对端 Pong，被自己的 pingLoop 以 90s 心跳
//     超时拆掉整条连接（含其它健康流）。现在经 writeCh 交给单写者 writeLoop；writeCh 满时
//     只记账（pendingPong），由 50ms ticker 补送，**不丢、不阻塞 readLoop、不产生无界 goroutine**。
//   - **观测**：readLoop 内三条「可能阻塞路径」（push / datagram handler / pong）的进入次数与
//     耗时、以及单流 dataCh 峰值占用，必须真的记数——它们是后续 readLoop 治理取舍的证据基线。
//
// 所有用例都不依赖固定 sleep：帧输入由测试经 channel 精确投喂，等待用 testutil.WaitFor；
// 「阻塞中也能观测到」这点由 enter/leave 两步记账保证（Waits 在进入时即 +1）。

// observingConn 是「帧由测试投喂、发出的帧全部记录」的 mock 连接。
type observingConn struct {
	*mockxfer.MockConn

	in   chan []byte
	mu   sync.Mutex
	sent [][]byte
}

func newObservingConn() *observingConn {
	c := &observingConn{in: make(chan []byte, 64)}
	c.MockConn = &mockxfer.MockConn{
		SendFn: func(_ context.Context, msg []byte) error {
			c.mu.Lock()
			c.sent = append(c.sent, append([]byte(nil), msg...))
			c.mu.Unlock()
			return nil
		},
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			select {
			case raw := <-c.in:
				return raw, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	return c
}

// feed 把一帧投喂给 mux 的 readLoop（测试是唯一输入源；缓冲非阻塞，无时序假设）。
func (c *observingConn) feed(t *testing.T, ftype FrameType, payload []byte) {
	t.Helper()
	raw, err := EncodeFrame(0, ftype, payload)
	if err != nil {
		t.Fatalf("EncodeFrame(%v): %v", ftype, err)
	}
	select {
	case c.in <- raw:
	default:
		t.Fatal("输入缓冲已满（测试投喂过快）")
	}
}

// countOf 返回已发出帧中指定帧类型的条数。
func (c *observingConn) countOf(ftype FrameType) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, raw := range c.sent {
		if _, ft, _, err := DecodeFrame(raw); err == nil && ft == ftype {
			n++
		}
	}
	return n
}

// countFramesOf 统计 blockingSendConn 已发出帧中指定帧类型的条数（其 sent 由 SendFn 记录）。
func countFramesOf(c *blockingSendConn, ftype FrameType) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, raw := range c.sent {
		if _, ft, _, err := DecodeFrame(raw); err == nil && ft == ftype {
			n++
		}
	}
	return n
}

// newTestMux 建立「测试可投喂帧」的 mux，并返回 mux 与观测连接。
func newTestMux(t *testing.T) (*Mux, *observingConn) {
	t.Helper()
	c := newObservingConn()
	m := New(c, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	return m, c
}

// waitClockTick 自旋等待单调时钟前进至少一格后返回。
//
// 为何需要：`BlockStat` 的耗时是 leave 时的 `time.Since(enter)`，而本机（Windows）单调时钟
// 粒度**实测为 512µs**（连读 20 万次 100% 同值）；上述两个用例放行阻塞的时机可能紧接「进入阻塞」
// （WaitFor 首次 cond 检查即命中）⇒ 实测耗时在 µs 级、会被记成 0ns，使「Nanos > 0」在粗粒度时钟
// 平台上假红（CI 的 windows job 会跑 `go test -race`）。等一格后测得耗时必然非零。
// 这不是固定 sleep：通常 <1ms，并以 1s 为上限防时钟异常。
func waitClockTick(t *testing.T) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(time.Second)
	for time.Now().Equal(start) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
}

// TestPong_NotSentFromReadLoopWhileWriteChFull：writeCh 满时回复 Pong **不得**在调用方
// （生产中即 readLoop）goroutine 内同步发送，否则整条 mux 停摆。
//
// 红灯形态：修复前 handlePingFrame 直接 `m.conn.Send`，而本用例把 writeLoop 确定性卡在
// `conn.Send` 上、writeCh 已填满 ⇒ 那次 Send 会一直阻塞到测试放行，`returned` 永不关闭
// ⇒ WaitFor 失败并给出可读消息（不是挂死：cleanup 会 releaseGate 让阻塞的 goroutine 退出）。
func TestPong_NotSentFromReadLoopWhileWriteChFull(t *testing.T) {
	t.Parallel()
	c := newBlockingSendConn()
	m, _ := fullWriteChMux(t, c)

	raw, err := EncodeFrame(0, FramePing, nil)
	if err != nil {
		t.Fatalf("EncodeFrame(FramePing): %v", err)
	}
	returned := make(chan struct{})
	go func() {
		// 两次 Ping：第二次证明「合并」而不是「各回一条」。
		m.handleFrame(raw)
		m.handleFrame(raw)
		close(returned)
	}()

	testutil.WaitFor(t, 10*time.Second, func() bool {
		select {
		case <-returned:
			return true
		default:
			return false
		}
	}, "回复 Pong 不得在 readLoop 内同步阻塞：writeCh 满时应只记账并由 ticker 补送（否则整条 mux 停摆、心跳超时拆连接）")

	if got, want := m.Metrics().ReadLoopPong.Waits.Load(), int64(2); got != want {
		t.Errorf("readLoop 回复 Pong 的进入次数=%d want %d（观测缺失说明未走记账路径）", got, want)
	}
	if got := countFramesOf(c, FramePong); got != 0 {
		t.Errorf("writeCh 满时不得有 Pong 出线, got %d", got)
	}
	if got, want := m.Metrics().PongsCoalesced.Load(), int64(2); got != want {
		t.Errorf("被挤掉的 Pong 应记 coalesced=%d, got %d", want, got)
	}
}

// TestPong_DeferredCreditIsResentOnce：被挤掉的 Pong 必须由 ticker 补送，且因 Pong **幂等**
// 合并为**恰好一次**出线（不得静默丢失、也不得重复）。
func TestPong_DeferredCreditIsResentOnce(t *testing.T) {
	t.Parallel()
	c := newBlockingSendConn()
	m, _ := fullWriteChMux(t, c)

	raw, err := EncodeFrame(0, FramePing, nil)
	if err != nil {
		t.Fatalf("EncodeFrame(FramePing): %v", err)
	}
	for range 3 {
		m.handleFrame(raw) // 3 次 Ping ⇒ pendingPong 被合并为单个布尔
	}

	c.releaseGate() // 放行 writeLoop：此后 ticker 才有机会补送

	testutil.WaitFor(t, 10*time.Second, func() bool {
		return countFramesOf(c, FramePong) >= 1
	}, "被 writeCh 打满挤掉的 Pong 必须补送：否则对端心跳超时会拆掉整条连接")

	// 给 ticker 再跑几轮的机会：出线数必须稳定在 1（合并语义，不得重复补送）。
	testutil.WaitFor(t, time.Second, func() bool {
		return m.Metrics().PongsSent.Load() == 1 && countFramesOf(c, FramePong) == 1
	}, "Pong 幂等：3 次 Ping 被合并后应恰好出线 1 次")

	if got := m.Metrics().PongsSent.Load(); got != 1 {
		t.Errorf("PongsSent=%d want 1（成功出线才计数）", got)
	}
}

// TestPong_DeliveredThroughWriteLoopWhenIdle：正常路径（writeCh 不满）下 Pong 仍必须经
// writeLoop 出线且计数正确——证明改造没有把心跳回复弄丢。
func TestPong_DeliveredThroughWriteLoopWhenIdle(t *testing.T) {
	t.Parallel()
	m, c := newTestMux(t)

	c.feed(t, FramePing, nil)

	testutil.WaitFor(t, 10*time.Second, func() bool {
		return c.countOf(FramePong) == 1
	}, "正常路径下 Pong 必须经 writeLoop 出线")

	if got := m.Metrics().ReadLoopPong.Waits.Load(); got != 1 {
		t.Errorf("ReadLoopPong.Waits=%d want 1", got)
	}
	if got := m.Metrics().PongsCoalesced.Load(); got != 0 {
		t.Errorf("writeCh 未满时不应产生 coalesced 计数, got %d", got)
	}
	if got := m.Metrics().PongsSent.Load(); got != 1 {
		t.Errorf("PongsSent=%d want 1", got)
	}
}

// TestPong_SendFailureCountsDroppedNotErrors：Pong 出线失败必须只计入 PongsDropped，
// **不得**计入 Errors —— `sproxy_mux_errors` 是告警信号，而 Pong 丢失是幂等可自愈的
// （下一轮 Ping 会再回一次，对端 90s 心跳窗口足够）。改造前 Pong 失败被忽略（仅 Debug 日志），
// 出向字节改经单写者 writeLoop 后，sendFrame 错误分支里既有的 Errors.Add(1) 会把它误计成告警。
//
// 红灯形态：把 sendFrame 的 Pong 分支改回 Errors.Add(1) ⇒ 本用例红。
func TestPong_SendFailureCountsDroppedNotErrors(t *testing.T) {
	t.Parallel()
	c := newObservingConn()
	// 覆写嵌入替身的 SendFn：让 Pong 出线必然失败（字段经 *mockxfer.MockConn 提升）。
	c.SendFn = func(context.Context, []byte) error { return errors.New("send boom") }
	m := New(c, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	c.feed(t, FramePing, nil)
	// 等待门必须等**出线结果已落账**，而不能只等 FramesSent：后者在 sendFrame 入口即 +1，
	// 早于真正判定结果的那几次计数 ⇒ 会在写者处理完之前读到 0 而**假红**（低概率窗口，
	// Windows 粗时钟下更易命中；与本文件 waitClockTick 的成因同类）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		mt := m.Metrics()
		return mt.PongsDropped.Load()+mt.PongsSent.Load()+mt.Errors.Load() >= 1
	}, "Pong 必须被 writeLoop 处理完（出线结果已落账）")

	if got := m.Metrics().PongsDropped.Load(); got != 1 {
		t.Errorf("PongsDropped=%d want 1（出线失败应单列丢弃计数）", got)
	}
	if got := m.Metrics().Errors.Load(); got != 0 {
		t.Errorf("Errors=%d want 0（Pong 丢失经下一轮心跳自愈，不得污染告警信号）", got)
	}
	if got := m.Metrics().PongsSent.Load(); got != 0 {
		t.Errorf("PongsSent=%d want 0（未出线不得计为已回 Pong）", got)
	}
}

// TestReadLoopObserve_DatagramHandlerTimeCounted：readLoop **同步**调用数据报 handler 的
// 耗时必须被观测到（同源阻塞口①的证据基线），且阻塞中也能看到进入次数。
func TestReadLoopObserve_DatagramHandlerTimeCounted(t *testing.T) {
	t.Parallel()
	m, c := newTestMux(t)

	release := make(chan struct{})
	handlerDone := make(chan struct{})
	m.SetDatagramHandler(func(_ uint32, _ []byte) {
		<-release
		close(handlerDone)
	})

	c.feed(t, FrameDatagram, []byte{0, 0, 0, 0, 'x'})

	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.Metrics().ReadLoopDatagram.Waits.Load() == 1
	}, "进入阻塞路径时 Waits 应立即 +1（若等阻塞结束才记账，则停摆期间看不到任何进入次数）")

	if got := m.Metrics().ReadLoopDatagram.Nanos.Load(); got != 0 {
		t.Errorf("handler 尚未返回，累计耗时应为 0, got %d", got)
	}

	// 放行前先等时钟走一格：否则这次阻塞可能短到被测成 0ns（粒度 512µs，见 waitClockTick）。
	waitClockTick(t)
	close(release)
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("handler 未被放行")
	}

	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.Metrics().ReadLoopDatagram.Nanos.Load() > 0
	}, "handler 返回后应记录本次耗时（该指标是后续 readLoop 治理取舍的证据基线）")
}

// TestReadLoopObserve_PushWaitsOnlyWhenBufferFull：pushData 的阻塞观测只在 dataCh 真的满时
// 才记数，并且**阻塞中**即可观测到（Waits 进入即 +1）；同时钉住 dataCh 峰值水位。
func TestReadLoopObserve_PushWaitsOnlyWhenBufferFull(t *testing.T) {
	t.Parallel()
	c := newObservingConn()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	m := New(c, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	s, err := m.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ss := s.(*stream)

	// 未满时不得记数（避免把每一次投递都算成阻塞，使指标失去判别力）。
	ss.pushData([]byte{1})
	if got := m.Metrics().ReadLoopPush.Waits.Load(); got != 0 {
		t.Errorf("缓冲未满时不应记 readLoop 阻塞, got %d", got)
	}
	if got, want := m.Metrics().DataChMaxFrames.Load(), int64(1); got != want {
		t.Errorf("dataCh 峰值水位=%d want %d", got, want)
	}

	// 填满（容量 64）后，下一次投递必然阻塞 readLoop。
	for len(ss.dataCh) < cap(ss.dataCh) {
		ss.pushData([]byte{1})
	}
	if got, want := m.Metrics().DataChMaxFrames.Load(), int64(cap(ss.dataCh)); got != want {
		t.Fatalf("dataCh 峰值水位=%d want %d（应为容量）", got, want)
	}

	done := make(chan struct{})
	go func() {
		handleDataFrame(m, ss.ID(), []byte{2}) // 生产中由 readLoop 调用
		close(done)
	}()

	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.Metrics().ReadLoopPush.Waits.Load() == 1
	}, "dataCh 满时 pushData 必须记一次阻塞（且阻塞中即可见）")
	select {
	case <-done:
		t.Fatal("前置失败：dataCh 已满，pushData 不应立即返回")
	default:
	}

	// 排空一个槽位前先等时钟走一格：否则这次阻塞可能短到被测成 0ns（粒度 512µs，见 waitClockTick）。
	waitClockTick(t)

	// 排空一个槽位：阻塞的投递应随之完成并记录耗时。
	if _, err := ss.Read(make([]byte, 1)); err != nil {
		t.Fatalf("Read: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("排空槽位后 pushData 仍阻塞")
	}
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.Metrics().ReadLoopPush.Nanos.Load() > 0
	}, "阻塞结束后应记录本次耗时")
}
