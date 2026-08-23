// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"sort"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// peerLinkInfo 描述一条已建立的对等直连链路（mesh status --gateway 拓扑可观测）。
type peerLinkInfo struct {
	// Peer 是对端 mesh node 的稳定 node-id。
	Peer string `json:"peer"`
	// Link 是链路类型（当前恒为 "webrtc-direct"——自动对等发现的打洞直连）。
	Link string `json:"link"`
	// Since 是链路建立时间（RFC3339）。
	Since time.Time `json:"since"`
}

// linkPool 是 mesh node 已建立的对等直连链路存储：
// 自动对等发现环写入（dialPeer 拨号成功后 set），本地网关与状态查询读取
// （GatewayConnect 按 peer 找已建链路并复用）。同一把锁保护并发读写。
type linkPool struct {
	mu        sync.Mutex
	peerMux   map[string]*mux.Mux
	peerSince map[string]time.Time
}

func newLinkPool() *linkPool {
	return &linkPool{
		peerMux:   map[string]*mux.Mux{},
		peerSince: map[string]time.Time{},
	}
}

// set 记录一条到 peer 的已建链路（mux 心跳保活；拨号成功后调用）。
func (p *linkPool) set(peer string, m *mux.Mux) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peerMux[peer] = m
	p.peerSince[peer] = time.Now()
}

// get 返回到 peer 的已建链路（若存在）。
func (p *linkPool) get(peer string) (*mux.Mux, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m, ok := p.peerMux[peer]
	return m, ok
}

// removeIf 仅当链路池中该 peer 仍指向 m 时才移除（accept 侧 serve 结束后调用：
// 若期间对端已重连、链路池已 set 新 mux，则不误删新链路）。
func (p *linkPool) removeIf(peer string, m *mux.Mux) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cur, ok := p.peerMux[peer]; ok && cur == m {
		delete(p.peerMux, peer)
		delete(p.peerSince, peer)
	}
}

// sweep 清理已断开（m.Done 已关闭）的链路，返回被移除的 peer 列表。
// 每周期开头调用：对端离线/链路中断后自动移除，下一轮发现周期重新拨号。
func (p *linkPool) sweep() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var gone []string
	for peer, m := range p.peerMux {
		select {
		case <-m.Done():
			delete(p.peerMux, peer)
			delete(p.peerSince, peer)
			gone = append(gone, peer)
		default:
		}
	}
	return gone
}

// snapshot 返回全部已建链路的排序快照（mesh status --gateway / 测试观测）。
func (p *linkPool) snapshot() []peerLinkInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]peerLinkInfo, 0, len(p.peerMux))
	for peer, since := range p.peerSince {
		if _, ok := p.peerMux[peer]; !ok {
			continue
		}
		out = append(out, peerLinkInfo{Peer: peer, Link: "webrtc-direct", Since: since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Peer < out[j].Peer })
	return out
}

// closeAll 关闭全部已建链路（发现环退出时调用；mux.Close 幂等，与中继/网关
// 收尾共享同一底层连接无副作用）。
func (p *linkPool) closeAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for peer, m := range p.peerMux {
		_ = m.Close()
		delete(p.peerMux, peer)
		delete(p.peerSince, peer)
	}
}
