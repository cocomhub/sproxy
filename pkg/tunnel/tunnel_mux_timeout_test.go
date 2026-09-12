// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	builtin "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// TestServe_HandshakeTimeoutHonored 断言 WithHandshakeTimeout 对 listener 侧握手**有实效**：
// 对端不发任何字节（握手停滞）时，Serve 必须在约 d 内返回错误，而不是等默认 30s。
//
// 为什么需要这个选项：Serve 的握手与 accept 循环共用传入 ctx，调用方无法单独缩短握手
// 阶段——没有该选项时「握手停滞」只能等满 defaultHandshakeTimeout(30s)。
//
// 为什么用真 net.Pipe + builtin.FromNetConn：握手必须跑在真实 mux 之上才叫「实效」
// （纯 mock 只会验证 option 字段被赋值）。这同时是该适配器接真帧协议的第二重实证。
//
// 失败模式（时钟断言的右侧保险）：若 elapsed 远超 d，说明 option 未被消费——默认 30s
// 路径仍在生效，用例以 elapsed 断言显式报出（而不是靠 go test 全局超时）。
func TestServe_HandshakeTimeoutHonored(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	m := mux.New(builtin.FromNetConn(server), mux.RoleListener)
	defer func() { _ = m.Close() }()

	tun := NewTunnel(m, []byte(testutil.TestKey()), WithHandshakeTimeout(150*time.Millisecond))
	start := time.Now()
	err := tun.Serve(t.Context(), http.NotFoundHandler())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("对端不发握手帧时应超时报错（fail-closed）")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("应受 WithHandshakeTimeout 约束（~150ms）, 实际 %v", elapsed)
	}
	// 下界抓的是**与上界相反方向的退化**：配置被忽略而「有效超时远小于所配值」
	// （如默认值被调小、或选项被错误地钳到更短的值）——此时用例会太快返回，
	// 上界（>2s）反而恒绿。下界取 100ms（所配 150ms 的 2/3）：context 定时器不会
	// 早于 deadline 触发，故正常路径恒满足，不引入 flaky；变异证据见任务报告 M4。
	if elapsed < 100*time.Millisecond {
		t.Fatalf("应受 WithHandshakeTimeout(150ms) 约束，实际仅 %v——有效超时短于所配值（配置被忽略且默认值更短？）", elapsed)
	}
}

// TestWithHandshakeTimeout_NonPositiveIgnored 断言非正数被忽略（保持默认 30s）。
//
// 为什么值得单测：该守卫是「零回归」的实现机制——若哪天把 `d > 0` 写成无条件赋值，
// 传 0 或负数的调用方会把握手超时变成「立即超时」（0 表示无超时语义在
// context.WithTimeout 里是立刻过期），存量调用方的行为被静默改写。
func TestWithHandshakeTimeout_NonPositiveIgnored(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		tun := NewTunnel(nil, nil, WithHandshakeTimeout(d))
		if tun.handshakeTimeout != defaultHandshakeTimeout {
			t.Fatalf("d=%v 应被忽略（保持默认 %v），实际 %v", d, defaultHandshakeTimeout, tun.handshakeTimeout)
		}
	}
	// 正数必须被采纳（否则上一个断言在「option 整体失效」时也会误绿）。
	tun := NewTunnel(nil, nil, WithHandshakeTimeout(3*time.Second))
	if tun.handshakeTimeout != 3*time.Second {
		t.Fatalf("正数应被采纳为 3s，实际 %v", tun.handshakeTimeout)
	}
}
