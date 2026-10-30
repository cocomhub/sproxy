// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import "testing"

// fakeXferMetricsProvider 是装配层 XferMetricsProvider 的测试替身：
// 固定返回构造时的快照，用来锁定「提供者 → 渲染层」的取值路径。
type fakeXferMetricsProvider struct {
	snapshot XferMetrics
}

func (f fakeXferMetricsProvider) XferMetrics() XferMetrics { return f.snapshot }

func TestXferMetricsProvider_ValueSnapshot(t *testing.T) {
	t.Parallel()

	wantWS := XferConnMetrics{
		ConnsOpened:  2,
		ConnsClosed:  1,
		MessagesSent: 30,
		MessagesRecv: 20,
		BytesSent:    5000,
		BytesRecv:    8000,
	}
	wantQUIC := XferConnMetrics{ConnsOpened: 1, MessagesSent: 1, BytesSent: 100}

	var provider XferMetricsProvider = fakeXferMetricsProvider{
		snapshot: XferMetrics{WS: wantWS, QUIC: wantQUIC},
	}
	got := provider.XferMetrics()
	if got.WS != wantWS {
		t.Errorf("WS = %+v, want %+v", got.WS, wantWS)
	}
	if got.QUIC != wantQUIC {
		t.Errorf("QUIC = %+v, want %+v", got.QUIC, wantQUIC)
	}

	// 快照是值拷贝：调用方修改不影响提供者的计数器（/metrics 读取无副作用）。
	got.WS.BytesSent = 0
	if again := provider.XferMetrics(); again.WS.BytesSent != wantWS.BytesSent {
		t.Errorf("修改快照后提供者值被污染: BytesSent = %d, want %d", again.WS.BytesSent, wantWS.BytesSent)
	}
}

func TestXferMetrics_ZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	var xm XferMetrics
	if xm.WS != (XferConnMetrics{}) || xm.QUIC != (XferConnMetrics{}) {
		t.Errorf("零值 XferMetrics 应全 0，got %+v", xm)
	}

	// 装配层按连接事件累加（字段为普通计数器，快照即读即取）。
	xm.WS.ConnsOpened++
	xm.WS.MessagesSent += 3
	xm.WS.BytesSent += 4096
	if xm.WS.ConnsOpened != 1 || xm.WS.MessagesSent != 3 || xm.WS.BytesSent != 4096 {
		t.Errorf("WS 累加结果 = %+v, want {ConnsOpened:1 MessagesSent:3 BytesSent:4096}", xm.WS)
	}
	if xm.QUIC != (XferConnMetrics{}) {
		t.Errorf("WS 累加不应影响 QUIC，got %+v", xm.QUIC)
	}
}
