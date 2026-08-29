// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package icecfg 控制 webrtc 传输的 ICE UDP 候选收集行为，供 webrtc 主包读取、
// 供 webrtctest 测试工具包设置。默认关闭，生产路径无任何行为变化。
//
// 目的：Windows 下 pion 默认在每个 PeerConnection 上对全部接口 net.ListenUDP 收集
// host 候选，多次运行测试（mesh.test.exe / hub.test.exe）会反复弹防火墙授权框。
// loopback-only 模式让每个 PeerConnection 只绑定 loopback 接口（127.0.0.1）收集候选，
// 不触及非本机接口，Windows 不再弹窗。代价是仅收集 loopback host 候选，跨机器直连
// 会失效，故仅测试专用；生产（mesh node / relay / p2p 打洞）保持默认 false 不受影响。
//
// 注意：这里不做「共享单 socket 的多 PeerConnection 复用」——两个独立 PeerConnection
// 共享同一 UDP socket 时，pion 的 ICE 无法区分包归属（ufrag 串扰，username mismatch），
// 连通性检查必然失败。因此每个 PeerConnection 使用独立的 loopback socket。
package icecfg

import (
	"sync"
)

var (
	mu           sync.Mutex
	loopbackOnly bool
)

// LoopbackOnly 返回是否启用 loopback-only 候选收敛（newPC 据此只绑 loopback）。
func LoopbackOnly() bool {
	mu.Lock()
	defer mu.Unlock()
	return loopbackOnly
}

// SetLoopbackOnly 开/关 loopback-only 候选收敛。开启后新建的连接仅收集 loopback
// host 候选（跨机器直连失效），用于测试。在创建任何连接前调用。
func SetLoopbackOnly(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	loopbackOnly = enabled
}

// Reset 恢复 loopback-only 默认（false）。测试收尾调用。
func Reset() { SetLoopbackOnly(false) }
