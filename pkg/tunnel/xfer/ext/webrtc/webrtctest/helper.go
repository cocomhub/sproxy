// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package webrtctest 提供 webrtc 传输的测试辅助工具。
//
// 目的（Windows 防火墙弹窗）：pion/ice 在每个 PeerConnection 上默认对全部接口
// net.ListenUDP 收集 host 候选，多次运行测试（mesh.test.exe / hub.test.exe）会反复弹
// 授权框。本包把新建连接的 UDP 候选收集收敛到 loopback 单端口多路复用 socket；代价
// 是仅收集 loopback host 候选，跨机器直连会失效，因此本包仅供测试使用。
//
// 用法：需要真实 WebRTC 连接的测试开头调用 New(t)：
//
//	env := webrtctest.New(t)
//	env.Close() // 可选：提前释放（幂等）；t 结束也会自动清理
//
// 本包不替调用方决定 t.Parallel 等行为——共享 loopback mux 并发安全（ufrag 区分
// 不同连接），调用方可按场景自由组合；本包只负责收敛 + 收尾，把选择权留给测试。
package webrtctest

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/internal/icecfg"
)

// Tester 是一次 ICE loopback 收敛会话。
// 方法零状态，主要承载 cleanup 语义与可读名字。
type Tester struct{}

// New 开启 loopback 候选收敛并注册收尾（t 结束自动恢复）。
// 之后的 webrtc.Dial/Listen 均只收集 loopback host 候选。
// 返回的 Tester.Close() 可提前释放；重复调用幂等。
// 注意：本测试启用期间，同进程其它并行测试也会走 loopback 收敛（共享单例状态）；
// 串行/并行均可，但不要在同一进程里同时开启+关闭而不经 t.Cleanup。
func New(t *testing.T) *Tester {
	t.Helper()
	icecfg.SetLoopbackOnly(true)
	t.Cleanup(icecfg.Reset)
	return &Tester{}
}

// Close 提前释放 loopback 收敛（幂等）。
// t 结束时无用显式调用；仅在测试中途需要切回默认行为时调用。
func (tc *Tester) Close() {
	icecfg.Reset()
}
