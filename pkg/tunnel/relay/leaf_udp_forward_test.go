// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件钉住审计确认的**同源阻塞口①**：UDP 出口的转发 handler 由 mux readLoop **同步**调用，
// 而 readLoop 是单 goroutine 串行处理所有帧 —— 原先 handler 内同步 `WriteToUDP`，一旦出口变慢
// 或卡住（对端不消费、内核发送缓冲打满），整条 mux 停摆：其它流全部不推进，本侧也读不到对端
// Pong，最终被自己的 pingLoop 以 90s 心跳超时拆掉整条连接（含其它健康流；hub 拓扑下等于一个
// 节点的全部用户流量）。
//
// 修法（leaf.go 的 newUDPForwardHandler）：照抄 cmd/sclient/udp.go 的「信号量 + goroutine」——
// 出口写异步化、信号量有界、饱和即丢包并计数、绝不阻塞 readLoop。
//
// 用例都不依赖固定 sleep：帧由测试经 pipe 精确投喂，写者是否已卡住由替身的 entered channel 通知，
// 等待一律 testutil.WaitFor / 有界 select。

// blockingEgress 是「进入 WriteToUDP 即卡住、放行后记录内容」的 UDP 出口替身。
type blockingEgress struct {
	entered chan struct{}
	release chan struct{}

	releaseOnce sync.Once
	mu          sync.Mutex
	written     [][]byte
}

func newBlockingEgress() *blockingEgress {
	return &blockingEgress{
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

func (e *blockingEgress) WriteToUDP(b []byte, _ *net.UDPAddr) (int, error) {
	select {
	case e.entered <- struct{}{}:
	default:
	}
	<-e.release
	e.mu.Lock()
	e.written = append(e.written, append([]byte(nil), b...))
	e.mu.Unlock()
	return len(b), nil
}

// releaseAll 放行所有在途写（幂等）。
func (e *blockingEgress) releaseAll() { e.releaseOnce.Do(func() { close(e.release) }) }

// payloads 返回已放行的写内容快照。
func (e *blockingEgress) payloads() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, len(e.written))
	copy(out, e.written)
	return out
}

// frameSink 在裸连接上持续收帧并把帧类型推入 channel（测试据此观测对端响应）。
func frameSink(ctx context.Context, c xfer.Conn) <-chan mux.FrameType {
	out := make(chan mux.FrameType, 256)
	go func() {
		defer close(out)
		for {
			raw, err := c.Receive(ctx)
			if err != nil {
				return
			}
			if _, ft, _, derr := mux.DecodeFrame(raw); derr == nil {
				select {
				case out <- ft:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// sendRawFrame 向裸连接投喂一帧（模拟对端）。
func sendRawFrame(ctx context.Context, c xfer.Conn, ftype mux.FrameType, payload []byte) error {
	raw, err := mux.EncodeFrame(0, ftype, payload)
	if err != nil {
		return err
	}
	return c.Send(ctx, raw)
}

// datagramPayload 构造数据报帧负载：[4B flowID][数据报]。
func datagramPayload(data []byte) []byte {
	return append([]byte{0, 0, 0, 0}, data...)
}

// waitForFrame 有界等待 sink 上出现指定帧类型（会丢弃途中其它帧，测试只关心目标帧）。
func waitForFrame(t *testing.T, sink <-chan mux.FrameType, want mux.FrameType, msg string) {
	t.Helper()
	testutil.WaitFor(t, 10*time.Second, func() bool {
		for {
			select {
			case ft, ok := <-sink:
				if !ok {
					return false
				}
				if ft == want {
					return true
				}
			default:
				return false
			}
		}
	}, msg)
}

// TestNewUDPForwardHandler_BlockedWriteDoesNotStallReadLoop：**红灯用例**——UDP 出口写者卡住时，
// readLoop 必须仍能处理其它帧（这里用 Ping→Pong 作为「readLoop 活着」的可观测量）。
//
// 变异验证（修复改回同步写）下本用例必红：那次同步 WriteToUDP 会一直停在替身里，readLoop 卡在
// handler 内 ⇒ 后续 Ping 永不处理、收不到 Pong ⇒ WaitFor 超时失败。
func TestNewUDPForwardHandler_BlockedWriteDoesNotStallReadLoop(t *testing.T) {
	t.Parallel()
	egress := newBlockingEgress()
	t.Cleanup(egress.releaseAll)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	pipeA, pipeB := xfertest.Pipe()
	m := mux.New(pipeA, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	sink := frameSink(ctx, pipeB)

	raddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:9")
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	m.SetDatagramHandler(newUDPForwardHandler(egress, raddr, testLogger(), m, 0))

	// ① 投喂一个数据报 ⇒ 出口写者卡住（entered 通知「已进入写、且写未返回」）。
	if serr := sendRawFrame(ctx, pipeB, mux.FrameDatagram, datagramPayload([]byte("udp-1"))); serr != nil {
		t.Fatalf("投喂数据报帧: %v", serr)
	}
	select {
	case <-egress.entered:
	case <-ctx.Done():
		t.Fatal("UDP 出口写者未进入写（测试前置失败）")
	}

	// ② 写者仍卡住：投喂 Ping，readLoop 必须仍能处理并回复 Pong。
	if serr := sendRawFrame(ctx, pipeB, mux.FramePing, nil); serr != nil {
		t.Fatalf("投喂 Ping 帧: %v", serr)
	}
	waitForFrame(t, sink, mux.FramePong,
		"UDP 出口写者卡住时 readLoop 必须仍能处理其它帧（否则整条 mux 停摆、心跳超时拆掉全部流）")
}

// TestNewUDPForwardHandler_SaturationDropsAndCounts：信号量饱和时必须**丢弃并计数**，
// 且仍不阻塞 readLoop；放行后只有被接受的（第一笔）真正写出。
//
// maxInFlight 由参数注入（生产用 udpForwardMaxInFlight=64），使饱和可被确定性构造。
func TestNewUDPForwardHandler_SaturationDropsAndCounts(t *testing.T) {
	t.Parallel()
	egress := newBlockingEgress()
	t.Cleanup(egress.releaseAll)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)

	pipeA, pipeB := xfertest.Pipe()
	m := mux.New(pipeA, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	sink := frameSink(ctx, pipeB)

	raddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:9")
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	m.SetDatagramHandler(newUDPForwardHandler(egress, raddr, testLogger(), m, 1))

	// ① 第一笔占用唯一槽位并卡在写里。
	if serr := sendRawFrame(ctx, pipeB, mux.FrameDatagram, datagramPayload([]byte("first"))); serr != nil {
		t.Fatalf("投喂第一笔: %v", serr)
	}
	select {
	case <-egress.entered:
	case <-ctx.Done():
		t.Fatal("UDP 出口写者未进入写（测试前置失败）")
	}

	// ② 第二笔：槽位已满 ⇒ 必须丢弃并计数（而不是阻塞 readLoop 等第一笔完成）。
	if serr := sendRawFrame(ctx, pipeB, mux.FrameDatagram, datagramPayload([]byte("second"))); serr != nil {
		t.Fatalf("投喂第二笔: %v", serr)
	}
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return m.Metrics().DatagramHandlerDrops.Load() == 1
	}, "信号量饱和必须丢弃并计数（UDP 语义：背压丢包），不得阻塞 readLoop")

	// ③ readLoop 仍活着：Ping 必须得到 Pong。
	if serr := sendRawFrame(ctx, pipeB, mux.FramePing, nil); serr != nil {
		t.Fatalf("投喂 Ping 帧: %v", serr)
	}
	waitForFrame(t, sink, mux.FramePong, "饱和丢弃后 readLoop 仍必须能处理其它帧")

	// ④ 放行：只有被接受的第一笔真正写出，且内容未被改动。
	egress.releaseAll()
	testutil.WaitFor(t, 10*time.Second, func() bool {
		return len(egress.payloads()) == 1
	}, "放行后应恰好写出被接受的那一笔（第二笔已被饱和丢弃）")
	if got := string(egress.payloads()[0]); got != "first" {
		t.Errorf("写出的数据报内容=%q want %q", got, "first")
	}
	if got := m.Metrics().DatagramHandlerDrops.Load(); got != 1 {
		t.Errorf("丢弃计数=%d want 1", got)
	}
}

// TestNewUDPForwardHandler_DefaultsInFlightLimit：未指定（或非正）时使用生产默认上限，
// 避免误传 0 导致「任何一笔都丢弃」的静默退化。
func TestNewUDPForwardHandler_DefaultsInFlightLimit(t *testing.T) {
	t.Parallel()
	egress := newBlockingEgress()
	t.Cleanup(egress.releaseAll)

	pipeA, _ := xfertest.Pipe()
	m := mux.New(pipeA, mux.RoleDialer)
	raddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:9")
	if err != nil {
		t.Fatalf("ResolveUDPAddr: %v", err)
	}
	h := newUDPForwardHandler(egress, raddr, testLogger(), m, 0)
	t.Cleanup(func() { _ = m.Close() })

	// 连续投递 udpForwardMaxInFlight 笔：都必须在途（不阻塞），且尚未产生丢弃计数。
	for i := range udpForwardMaxInFlight {
		h(0, []byte{byte(i)})
	}
	if got := m.Metrics().DatagramHandlerDrops.Load(); got != 0 {
		t.Errorf("上限内不应丢弃, got %d", got)
	}
	// 第 上限+1 笔：必然饱和丢弃。
	h(0, []byte{'x'})
	if got := m.Metrics().DatagramHandlerDrops.Load(); got != 1 {
		t.Errorf("超过上限必须丢弃并计数, got %d", got)
	}
}
