// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package iostream 提供双向字节流泵送与通用 IO 工具，供 cmd/sclient（mesh/relay/
// p2p 端口转发与 stdio 会话）与 pkg/tunnel/relay/leaf.go、pkg/server/relay_stream.go
// 复用，消除各处重复的半关闭/宽限期泵送实现。
//
// 不依赖任何外部传输模块，保持主 go.mod 最小。
package iostream

import (
	"io"
	"net"
	"os"
	"strings"
	"time"
)

// PumpGrace 是双向泵送一方向完成后的半关闭宽限期：首方向完成后另一方向需在此
// 时间内完成收尾；超时视为对端非合作，强制关闭两端防 goroutine/FD 泄漏。
// 长连接（双向持续活跃）不触发计时器，不误断。
const PumpGrace = 60 * time.Second

// CloseWrite 向目标传播写半关闭（TCP FIN / 流 EOF），尽力而为：实现了
// CloseWrite() 的类型（*net.TCPConn、client.bufferedNetConn、mux.Stream 等）用
// CloseWrite；其余用 Close 退化，仍能解除对端 Read 阻塞。参数用 io.Closer 以
// 兼容 net.Conn 与 mux.Stream。
func CloseWrite(conn io.Closer) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

// WriteFull 循环写满整个 buf，处理 io.Writer 的部分写（mux 流在发送窗口小于
// buf 长度时返回 n<len 的短写）。仅用于小帧（长度前缀 + 元数据）；数据面泵送
// 用 io.Copy。
func WriteFull(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

// Pump 双向泵送两个流端（本地 socket <-> 隧道远端连接 / mux 流）。
//
// 关闭语义（C1 范本）：每个方向 io.Copy 完成后向对端 CloseWrite 传播半关闭，
// 而非立即全关——让在途响应仍可被另一方向读回（不截断）。首方向完成后武装
// grace 宽限期：宽限期内另一方向完成则正常收尾；超时视为对端非合作（对 FIN
// 不回应），强制关闭两端解除 Read 阻塞，防 goroutine / FD 泄漏。长连接（双向
// 持续活跃）期间两方向都未完成，计时器不会启动，不误断。正常路径不在此显式
// 关闭任一端的完整连接，由调用方 defer 收尾。
func Pump(a io.ReadWriteCloser, b io.ReadWriteCloser, grace time.Duration) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(b, a)
		CloseWrite(b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(a, b)
		CloseWrite(a)
		done <- struct{}{}
	}()

	remaining := 2
	var timeoutCh <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for remaining > 0 {
		select {
		case <-done:
			remaining--
			if remaining == 1 {
				// 一个方向完成：启动宽限期等待另一半完成半关闭收尾。
				timer = time.NewTimer(grace)
				timeoutCh = timer.C
			}
		case <-timeoutCh:
			// 非合作对端：强制关闭两端，解除阻塞中的 Read/Write。
			_ = a.Close()
			_ = b.Close()
			for remaining > 0 { // 关闭后 Read/Write 立即返回，等待 goroutine 退出
				<-done
				remaining--
			}
			return
		}
	}
}

// NormalizeListenAddr 将裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防
// LAN 暴露 + Windows 防火墙弹窗）；显式 IP/主机名/0.0.0.0 保持原样。
func NormalizeListenAddr(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// LocalHostname 返回本机主机名作为默认节点 ID；失败回退 fallback。
func LocalHostname(fallback string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return fallback
	}
	return host
}

// NetConnsAreDuplex 仅用于文档/断言：*net.TCPConn、client.bufferedNetConn 等
// 实现了 CloseWrite 且满足 io.ReadWriteCloser，故可传给 Pump。
var _ io.ReadWriteCloser = (*net.TCPConn)(nil)
