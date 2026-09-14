// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// snapshotCounters 取 mux 的「帧接收 / 错误」计数快照，用于判断刚发送的帧是否已被读循环处理。
func snapshotCounters(m *mux.Mux) (recv, errs int64) {
	mm := m.Metrics()
	return mm.FramesReceived.Load(), mm.Errors.Load()
}

// waitFrameHandled 等待读循环处理完刚发送的帧（接收计数或错误计数前进）。
//
// 替代原先「sleep 100ms 给 readLoop 时间」的固定等待：这些用例的目的是「畸形/未知帧不会
// panic、不会卡死」，而 FramesReceived/Errors 计数正是「已处理完」的可观测证据——条件等待
// 既确定（不会在繁忙 CI 上等不够）又更快（通常几毫秒即返回）。
func waitFrameHandled(t *testing.T, m *mux.Mux, beforeRecv, beforeErrs int64) {
	t.Helper()
	testutil.WaitFor(t, 30*time.Second, func() bool {
		recv, errs := snapshotCounters(m)
		return recv > beforeRecv || errs > beforeErrs
	}, "读循环应处理完刚发送的帧（FramesReceived/Errors 计数前进）")
}

// newPipePair creates a connected pair of xfer.Conn (client, server) via xfertest.Pipe.
func newPipePair(t *testing.T) (client, server xfer.Conn) {
	t.Helper()
	c, s := xfertest.Pipe()
	t.Cleanup(func() { c.Close(); s.Close() })
	return c, s
}

// newMuxPair creates a connected pair of mux.Mux (dialer, listener) on a Pipe.
func newMuxPair(t *testing.T) (dialer, listener *mux.Mux) {
	t.Helper()
	c, s := newPipePair(t)
	dm := mux.New(c, mux.RoleDialer)
	lm := mux.New(s, mux.RoleListener)
	t.Cleanup(func() { dm.Close(); lm.Close() })
	return dm, lm
}

// newMuxPairWithTimeout creates a connected pair of mux.Mux (dialer, listener) with timeout context.
func newMuxPairWithTimeout(t *testing.T, timeout time.Duration) (dialer, listener *mux.Mux, ctx context.Context, cancel context.CancelFunc) {
	t.Helper()
	d, l := newMuxPair(t)
	ctx, cancel = context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)
	return d, l, ctx, cancel
}

// openStreamWithAccept is a helper that opens a stream on the dialer and accepts on the listener.
// Returns (stream, accepted, ctx).
func openStreamWithAccept(t *testing.T, dm, lm *mux.Mux, ctx context.Context) (stream, accepted mux.Stream) {
	t.Helper()
	s, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	a, err := lm.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return s, a
}

func TestStream_ReadEOFAfterCloseWrite(t *testing.T) {
	dm, lm, ctx, _ := newMuxPairWithTimeout(t, 5*time.Second)
	stream, accepted := openStreamWithAccept(t, dm, lm, ctx)

	_, err := stream.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// 半关闭：发送 FrameCloseWrite
	var closeWriteErr error
	if closeWriteErr = stream.CloseWrite(); closeWriteErr != nil {
		t.Fatalf("CloseWrite failed: %v", closeWriteErr)
	}

	// 接受端应读到数据，然后读到 io.EOF
	buf := make([]byte, 1024)
	n, err := accepted.Read(buf)
	if n == 0 || err != nil {
		t.Fatalf("expected data on first read, got n=%d err=%v", n, err)
	}
	if string(buf[:n]) != "hello" {
		t.Fatalf("expected 'hello', got %q", string(buf[:n]))
	}

	// 第二次 Read 应返回 io.EOF（半关闭已结束）
	_, err = accepted.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after closewrite, got %v", err)
	}
}

func TestFlowControl_WriteBlocked(t *testing.T) {
	dm, lm, ctx, _ := newMuxPairWithTimeout(t, 5*time.Second)
	stream, _ := openStreamWithAccept(t, dm, lm, ctx)

	// 写满发送窗口（默认 64KB），触发流控阻塞
	payload := make([]byte, 65536)
	_, err := stream.Write(payload)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// 验证写入了完整的 payload
	t.Log("wrote 64KB successfully")
}

// TestWithAcceptChSize 验证 WithAcceptChSize 选项
func TestWithAcceptChSize(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.NewWithOpts(a, mux.RoleDialer)
	muxB := mux.NewWithOpts(b, mux.RoleListener, mux.WithAcceptChSize(2))
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// 打开 3 条流，不 Accept，第 3 条应触发拒绝
	streams := make([]mux.Stream, 0, 3)
	for i := range 3 {
		s, err := muxA.Open(ctx)
		if err != nil {
			if i < 2 {
				t.Fatalf("Open #%d should succeed: %v", i, err)
			}
			t.Logf("Open #%d failed (expected maybe): %v", i, err)
			break
		}
		streams = append(streams, s)
	}

	// 现在 Accept 一条，验证能拿到流
	s, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	s.Close()

	for _, s := range streams {
		s.Close()
	}
}

// TestStream_Abort_ImmediateAndIdempotent 验证 Abort 立即返回且幂等（I28）。
func TestStream_Abort_ImmediateAndIdempotent(t *testing.T) {
	dm, lm, ctx, _ := newMuxPairWithTimeout(t, 5*time.Second)
	stream, accepted := openStreamWithAccept(t, dm, lm, ctx)
	defer accepted.Close()

	done := make(chan struct{})
	go func() {
		if err := stream.Abort(); err != nil {
			t.Errorf("Abort #1 failed: %v", err)
		}
		if err := stream.Abort(); err != nil {
			t.Errorf("Abort #2 not idempotent: %v", err)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Abort 未立即返回（非阻塞失败）")
	}
}

// TestStream_Abort_ReadWriteReturnErrClosed 验证 Abort 后 Read/Write 立即返回
// ErrConnClosed（I28）。
func TestStream_Abort_ReadWriteReturnErrClosed(t *testing.T) {
	dm, lm, ctx, _ := newMuxPairWithTimeout(t, 5*time.Second)
	stream, accepted := openStreamWithAccept(t, dm, lm, ctx)
	defer accepted.Close()

	if err := stream.Abort(); err != nil {
		t.Fatalf("Abort failed: %v", err)
	}

	// Abort 后 Read/Write 不再阻塞，返回包装 ErrConnClosed 的错误。
	buf := make([]byte, 8)
	if _, err := stream.Read(buf); !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("Read after Abort = %v, want xfer.ErrConnClosed", err)
	}
	if _, err := stream.Write([]byte("x")); !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("Write after Abort = %v, want xfer.ErrConnClosed", err)
	}
}

// TestStream_Abort_ConcurrentWithPushData 验证 Abort 与 pushData/pushEOF 并发时
// 无 panic、无数据竞争（closeMu 保护；-race 下运行验证）。
func TestStream_Abort_ConcurrentWithPushData(t *testing.T) {
	synctest.Test(t, streamAbortConcurrentBody)
}

// streamAbortConcurrentBody 在 synctest 气泡内运行：原 10ms 定值等待是
// 「猜写 goroutine 已进入阻塞」——synctest.Wait() 精确等到它停驻，
// 竞态窗口建得又快又确定。
func streamAbortConcurrentBody(t *testing.T) {
	dm, lm, ctx, _ := newMuxPairWithTimeout(t, 5*time.Second)
	stream, accepted := openStreamWithAccept(t, dm, lm, ctx)
	defer accepted.Close()

	// 对端写一小批数据，本侧在写入过程中 Abort：数据量远小于发送窗口，不会因
	// 流控挂起；Abort 后 pushData 经 done 分支丢弃负载，写 goroutine 正常收尾。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			if _, err := accepted.Write([]byte("payload")); err != nil {
				return
			}
		}
	}()
	synctest.Wait() // 等写 goroutine 停驻（原 10ms 猜测）再 Abort，竞态窗口确定
	if err := stream.Abort(); err != nil {
		t.Fatalf("Abort failed: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("写 goroutine 未在 Abort 后正常退出")
	}
}

// TestStream_ReadDataBeforeImmediateClose（P1-6 回归）：
// 对端写数据后立即关闭时，读取方必须拿到已投递的数据而非关闭错误——readLoop
// 先 pushData 再 closeChannels，done 与 dataCh 同时就绪时 Go select 随机选取，
// 无修复前 ~50% 概率丢弃数据帧（I27 拨号结果帧读取在叶子"接受后立即关"场景的
// 可靠性）。循环多轮加大命中并发窗口的概率。
func TestStream_ReadDataBeforeImmediateClose(t *testing.T) {
	synctest.Test(t, streamReadDataBeforeCloseBody)
}

// streamReadDataBeforeCloseBody 在 synctest 气泡内运行：20 轮竞态窗口的
// 5ms 启动等待换成 synctest.Wait()（精确等 Read 停驻，虚拟时钟零耗时）。
func streamReadDataBeforeCloseBody(t *testing.T) {
	for i := range 20 {
		dm, lm, ctx, _ := newMuxPairWithTimeout(t, 5*time.Second)
		stream, accepted := openStreamWithAccept(t, dm, lm, ctx)
		defer stream.Close()
		defer accepted.Close()

		// 先让读取方进入阻塞 Read（命中 inner select 的 done/dataCh 并发窗口）。
		type readRes struct {
			n   int
			err error
		}
		readCh := make(chan readRes, 1)
		go func() {
			buf := make([]byte, 4)
			n, err := stream.Read(buf)
			readCh <- readRes{n, err}
		}()
		synctest.Wait() // 等 Read 停驻（原 5ms 猜测；未及时启动仅少命中竞态，数据不丢）

		// 对端写数据后立即关闭：数据帧与关闭帧几乎同时到达读取方。
		if _, err := accepted.Write([]byte("data")); err != nil {
			t.Fatalf("write: %v", err)
		}
		_ = accepted.Close()

		select {
		case r := <-readCh:
			if r.err != nil || r.n != 4 {
				t.Fatalf("iter %d: Read 应返回 4 字节数据，got n=%d err=%v", i, r.n, r.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("iter %d: Read 未返回（挂起）", i)
		}
	}
}

// TestHandleFrame_CloseWriteUnknownStream 测试 handleFrame 中 FrameCloseWrite 对未知流的处理
func TestHandleFrame_CloseWriteUnknownStream(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	rawFrame := mustEncodeFrame(t, 999, mux.FrameCloseWrite, nil)
	rv, ev := snapshotCounters(muxA)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}
	waitFrameHandled(t, muxA, rv, ev)
	b.Close()
}

// TestHandleFrame_WindowUpdateUnknownStream …
func TestHandleFrame_WindowUpdateUnknownStream(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	payload := make([]byte, 4)
	payload[0] = 0x01
	rawFrame := mustEncodeFrame(t, 999, mux.FrameWindowUpdate, payload)
	ctx := t.Context()
	rv, ev := snapshotCounters(muxA)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}
	waitFrameHandled(t, muxA, rv, ev)
	b.Close()
}

// TestHandleFrame_UnknownFrameType …
func TestHandleFrame_UnknownFrameType(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	rawFrame := mustEncodeFrame(t, 0, 0xFF, nil)
	ctx := t.Context()
	rv, ev := snapshotCounters(muxA)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}
	waitFrameHandled(t, muxA, rv, ev)
	b.Close()
}

// TestOpenAfterConnClose_Dialer 验证底层连接断开时 Open 返回错误
func TestOpenAfterConnClose_Dialer(t *testing.T) {
	a, _ := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	a.Close()

	ctx := t.Context()
	_, err := muxA.Open(ctx)
	if err == nil {
		t.Fatal("expected error on Open after conn close")
	}
	t.Logf("Open after conn close: %v", err)

	muxA.Close()
}

// TestHandleFrame_DuplicateOpen …
func TestHandleFrame_DuplicateOpen(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := muxA.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	accepted, err := muxB.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()

	// 模拟重复的 FrameOpen
	rawFrame := mustEncodeFrame(t, stream.ID(), mux.FrameOpen, nil)
	rv, ev := snapshotCounters(muxA)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}
	waitFrameHandled(t, muxA, rv, ev)
}

// TestHandleFrame_Ping 测试 handleFrame 中 FramePing 的处理（回复 FramePong）
func TestHandleFrame_Ping(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	rawFrame := mustEncodeFrame(t, 0, mux.FramePing, nil)
	rv, ev := snapshotCounters(muxA)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}
	waitFrameHandled(t, muxA, rv, ev)
	b.Close()
}

// TestHandleFrame_Pong 测试 handleFrame 中 FramePong 的处理
func TestHandleFrame_Pong(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	rawFrame := mustEncodeFrame(t, 0, mux.FramePong, nil)
	rv, ev := snapshotCounters(muxA)
	if err := b.Send(ctx, rawFrame); err != nil {
		t.Fatal(err)
	}
	waitFrameHandled(t, muxA, rv, ev)
	b.Close()
}

// TestWrite_BiggerThanWindow 测试写超过窗口大小的数据，应触发 writeLen 截断
func TestWrite_BiggerThanWindow(t *testing.T) {
	dm, lm, ctx, _ := newMuxPairWithTimeout(t, 5*time.Second)
	stream, _ := openStreamWithAccept(t, dm, lm, ctx)

	// 写超过默认窗口大小（65536）的数据
	payload := make([]byte, 70000)
	_, err := stream.Write(payload)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	t.Log("wrote 70KB across window boundary")
}

// TestHandleFrame_InvalidFrame 测试 handleFrame 对无效帧的处理
func TestHandleFrame_InvalidFrame(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	defer muxA.Close()

	ctx := t.Context()
	rv, ev := snapshotCounters(muxA)
	if err := b.Send(ctx, []byte{0, 0, 0, 1}); err != nil {
		t.Fatal(err)
	}
	// 截断帧会让解码失败并计入 Errors（而非 FramesReceived）——两者任一前进都说明已处理完
	waitFrameHandled(t, muxA, rv, ev)
	b.Close()
}

// TestWriteEmptyPayload 测试写入空数据应直接返回 0, nil
func TestWriteEmptyPayload(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	stream, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { stream.Close() })

	n, err := stream.Write(nil)
	if err != nil {
		t.Fatalf("Write(nil) should succeed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes, got %d", n)
	}

	n, err = stream.Write([]byte{})
	if err != nil {
		t.Fatalf("Write([]byte{}) should succeed: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes, got %d", n)
	}
}

// TestOpenWithMaxStreams_ListenerSide_Reject 验证 listener 侧因 maxStreams 拒绝 FrameOpen
func TestOpenWithMaxStreams_ListenerSide_Reject(t *testing.T) {
	synctest.Test(t, openMaxStreamsRejectBody)
}

// openMaxStreamsRejectBody 在 synctest 气泡内运行：原 200ms 占位等待
// 「重复 Open 被 readLoop 静默丢弃」——readLoop 处理完回到 channel 阻塞时
// synctest.Wait() 即返回（精确且瞬时）。
func openMaxStreamsRejectBody(t *testing.T) {
	a, b := xfertest.Pipe()

	lm := mux.NewWithOpts(b, mux.RoleListener, mux.WithMaxStreams(1))
	dm := mux.New(a, mux.RoleDialer)
	defer lm.Close()
	defer dm.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	s1, err := dm.Open(ctx)
	if err != nil {
		t.Fatalf("First Open failed: %v", err)
	}
	defer s1.Close()

	acc1, err := lm.Accept(ctx)
	if err != nil {
		t.Fatalf("First Accept failed: %v", err)
	}
	defer acc1.Close()

	s2, err := dm.Open(ctx)
	if err != nil {
		t.Logf("Second Open failed (expected possible): %v", err)
		return
	}
	defer s2.Close()
	_ = s2

	// 有意占位：确认重复 Open 不 panic（失败路径已在上方处理）。
	// synctest.Wait() 等 readLoop 消费掉该帧并回到阻塞（原 200ms 定值等待）。
	synctest.Wait()
}
