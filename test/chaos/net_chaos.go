// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

// net_chaos.go 是应用层 TCP proxy：127.0.0.1 监听转发到目标，可 Pause/Resume/Delay
// 复现网络分区/延迟（透明于应用，TLS 层断开即复现分区）。

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// NetChaos 是应用层 TCP proxy。
type NetChaos struct {
	ln      net.Listener
	target  string
	pauseMu sync.RWMutex
	paused  bool
	delay   time.Duration
	closeCh chan struct{}
	wg      sync.WaitGroup
	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
}

// NewNetChaos 启动 proxy（监听 127.0.0.1:0 转发到 target）。
func NewNetChaos(target string) (*NetChaos, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("proxy listen: %w", err)
	}
	p := &NetChaos{ln: ln, target: target, closeCh: make(chan struct{}), conns: map[net.Conn]struct{}{}}
	go p.acceptLoop()
	return p, nil
}

// Addr 返回 proxy 监听地址（供客户端连接）。
func (p *NetChaos) Addr() string { return p.ln.Addr().String() }

// Close 关闭 proxy。
func (p *NetChaos) Close() {
	close(p.closeCh)
	_ = p.ln.Close()
	p.connsMu.Lock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.connsMu.Unlock()
	p.wg.Wait()
}

// Pause 暂停转发（网络分区：在途连接断开，新连接挂起）。
func (p *NetChaos) Pause() {
	p.pauseMu.Lock()
	p.paused = true
	p.pauseMu.Unlock()
	// 断开在途连接（复现分区断链）。
	p.connsMu.Lock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.connsMu.Unlock()
}

// Resume 恢复转发。
func (p *NetChaos) Resume() {
	p.pauseMu.Lock()
	p.paused = false
	p.pauseMu.Unlock()
}

// Delay 设置转发延迟（注入延迟）。
func (p *NetChaos) Delay(d time.Duration) {
	p.pauseMu.Lock()
	p.delay = d
	p.pauseMu.Unlock()
}

func (p *NetChaos) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			select {
			case <-p.closeCh:
				return
			default:
				continue
			}
		}
		p.wg.Add(1)
		go p.handle(conn)
	}
}

func (p *NetChaos) handle(client net.Conn) {
	defer p.wg.Done()
	defer client.Close()
	p.pauseMu.RLock()
	paused := p.paused
	delay := p.delay
	p.pauseMu.RUnlock()
	if paused {
		// 分区：客户端连接建立但无转发（挂起直至 Resume）。
		for {
			p.pauseMu.RLock()
			still := p.paused
			p.pauseMu.RUnlock()
			if !still {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	p.connsMu.Lock()
	p.conns[client] = struct{}{}
	p.conns[upstream] = struct{}{}
	p.connsMu.Unlock()
	defer func() {
		p.connsMu.Lock()
		delete(p.conns, client)
		delete(p.conns, upstream)
		p.connsMu.Unlock()
	}()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}
