// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
)

// 本文件钉住「重传队列满时不得静默丢弃数据帧」。
//
// 动机（2026-09-16 独立审计的已确认代码路径）：入队者都是**数据帧**（sendFrame 的
// `m.enqueueRetransmit(frame, 0)`），而队列满时原实现是 `m.retransmitQ = m.retransmitQ[1:]`
// ——**静默丢掉最旧的帧**。丢弃它意味着上层已 `Write` 的字节永久消失 ⇒ 字节流定界错位 ⇒
// 上层分块 AEAD 认证失败（重传无法纠正）。此外重传帧必然晚于其后已按 writeCh 顺序发出的帧，
// 因此补发旧帧本身还会造成同流乱序。
//
// 修法（见 retransmit.go）：与本文件其它失败取舍一致——宁可让整条连接**显式失败**（Close），
// 也不产出静默错位的字节流。队列满意味着已有 256 帧 Send 失败，连接实际上已不可用，
// 关闭与「重传重试耗尽」路径（maxRetries）后果一致。
//
// 遗留（本用例不覆盖，需后续 PR）：即便不丢帧，重传帧仍可能晚于其后帧到达。要彻底满足同流
// 顺序，需要在重传完成前停止该流的新帧（或放弃该流）——那属于重传架构改动。

// TestRetransmitQueueFull_ClosesMuxInsteadOfDroppingOldest：第 maxRetransmitQ+1 帧入队时，
// 必须关闭 mux 且**保留**最旧帧；不得 drop-oldest 静默丢弃。
func TestRetransmitQueueFull_ClosesMuxInsteadOfDroppingOldest(t *testing.T) {
	t.Parallel()

	mc := &mockxfer.MockConn{
		SendFn: func(context.Context, []byte) error { return nil },
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	m := New(mc, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	queueCap := maxRetransmitQ
	frames := make([][]byte, 0, queueCap)
	for i := range queueCap {
		f, err := EncodeFrame(StreamID(2*i+1), FrameData, []byte{byte(i)})
		if err != nil {
			t.Fatalf("编码第 %d 帧: %v", i, err)
		}
		frames = append(frames, f)
		m.enqueueRetransmit(f, 0)
	}

	// 让已入队条目不会被 writeLoop 的 50ms 扫描发走（否则队列会自行排空，断言失去意义）。
	m.retransmitMu.Lock()
	for i := range m.retransmitQ {
		m.retransmitQ[i].deadline = time.Now().Add(time.Hour)
	}
	queued := len(m.retransmitQ)
	m.retransmitMu.Unlock()
	if queued != queueCap {
		t.Fatalf("前置失败：队列应恰好 %d 条, got %d", queueCap, queued)
	}
	select {
	case <-m.Done():
		t.Fatal("队列未满时不应关闭 mux")
	default:
	}

	extra, err := EncodeFrame(1, FrameData, []byte("extra"))
	if err != nil {
		t.Fatalf("编码附加帧: %v", err)
	}
	m.enqueueRetransmit(extra, 0)

	// 先取「最旧帧是否被静默丢弃」的证据（非致命断言，红灯时与下面的超时一起显形）。
	m.retransmitMu.Lock()
	got, first := len(m.retransmitQ), []byte(nil)
	if got > 0 {
		first = m.retransmitQ[0].frame
	}
	m.retransmitMu.Unlock()

	if got != queueCap {
		t.Errorf("队列长度=%d want %d（入队失败不得连带丢弃既有条目）", got, queueCap)
	}
	if !bytes.Equal(first, frames[0]) {
		t.Error("最旧帧不得被丢弃（drop-oldest）：它的字节已从上层 Write 返回，丢失即不可纠正的字节流错位")
	}

	testutil.WaitFor(t, 10*time.Second, func() bool {
		select {
		case <-m.Done():
			return true
		default:
			return false
		}
	}, "重传队列满时必须关闭 mux：静默丢弃数据帧会让上层已 Write 的字节永久丢失且无法纠正")
}
