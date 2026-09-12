// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package iostream 提供双向字节流泵送与通用 IO 工具，供 cmd/sclient（mesh/relay/
// p2p 端口转发与 stdio 会话）与 pkg/tunnel/relay/leaf.go、pkg/server/relay_stream.go
// 复用，消除各处重复的半关闭/宽限期泵送实现。
//
// 不依赖任何外部传输模块，保持主 go.mod 最小。
package iostream

import (
	"errors"
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

// ForceClose 强制关闭一端，解除阻塞中的 Read/Write，尽力而为且非阻塞：
//   - 实现了 Abort() 的类型（如 mux.Stream）用 Abort——它直接关本地 done 通道，
//     不经 writeCh；Close 在 writeCh 打满（对端停读导致流控窗口耗尽）时会永久
//     阻塞（P0-3），收尾/超时路径必须用 Abort；
//   - 其余（net.Conn 等）用 Close。
func ForceClose(end io.ReadWriteCloser) {
	if a, ok := end.(interface{ Abort() error }); ok {
		_ = a.Abort()
		return
	}
	_ = end.Close()
}

// WriteFull 循环写满整个 buf，处理 io.Writer 的部分写（mux 流在发送窗口小于
// buf 长度时返回 n<len 的短写）。小帧（长度前缀 + 元数据）与数据面泵送都必须走
// 本函数或 CopyFull——**不得**改用 io.Copy（见 CopyFull 说明）。
//
// 若 w 返回 (n<=0, nil) 或 n > len(buf)（违反 io.Writer 契约），返回 io.ErrShortWrite
// 而不是死循环或切片越界 panic。
func WriteFull(w io.Writer, buf []byte) error {
	for len(buf) > 0 {
		n, err := w.Write(buf)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(buf) {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

// CopyFull 把 src 全量拷贝到 dst（等价于 io.Copy，但**绝不因短写而静默截断**），
// 返回已写字节数与错误。dst 实现了 io.ReaderFrom 时直接委托（与 io.Copy 行为完全
// 一致：保留 net.TCPConn/*os.File 的 splice/sendfile 快路径）；否则按 WriteFull 语义
// 循环写足每个读到的分片。
//
// 返回值语义：失败时返回**已完整写完**的字节数——失败分片内部的部分写入不计入
// （WriteFull 不回报部分进度，故该计数严格小于真实落纸字节数）。短写**不算失败**
// （会被循环写足），只有 src 读错误或 dst 写错误才返回 error。
//
// 为什么不能用 io.Copy：mux.Stream.Write 是**窗口受限的短写**语义——发送窗口小于
// len(p) 时只投递窗口允许的一段并返回 (n<len(p), nil)。io.Copy 的通用循环遇到这种
// 短写会返回 io.ErrShortWrite 并**立即停止**，于是：
//   - 调用方丢弃返回值（本仓库多处曾如此）→ 明文隧道响应体在 ~64 KB 处**静默截断**；
//   - 调用方保留返回值但把方向当作"已正常结束"→ 半关闭传播出去，对端也只见短流。
//
// 实测（明文隧道、响应体 70000/200000/1000000 B）：旧 io.Copy 调用点全部只回传
// 65466 B（= 65536 流控窗口 − 70 B 元数据）且 Do() 报成功。任何以 mux 流为一端的
// 数据面泵送（tunnel 响应体、leaf 转发、中继泵送、Pump）都必须用本函数。
func CopyFull(dst io.Writer, src io.Reader) (int64, error) {
	if rf, ok := dst.(io.ReaderFrom); ok {
		// 与 io.Copy 同一分派：ReaderFrom 的实现（net.TCPConn / *os.File 等）内部
		// 自行处理部分写；树内唯一的短写目标 mux.Stream 未实现 ReaderFrom。
		return rf.ReadFrom(src)
	}
	buf := make([]byte, 32*1024) // 与 io.Copy 默认缓冲一致
	var written int64
	for {
		nr, rErr := src.Read(buf)
		if nr > 0 {
			if wErr := WriteFull(dst, buf[:nr]); wErr != nil {
				return written, wErr // 不计入失败分片内的部分写入（见函数文档）
			}
			written += int64(nr)
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				return written, nil
			}
			return written, rErr
		}
	}
}

// Pump 双向泵送两个流端（本地 socket <-> 隧道远端连接 / mux 流）。
//
// 拷贝语义：两个方向都用 CopyFull 而非 io.Copy——任一端是 mux 流时，io.Copy 会在
// 首次窗口受限短写上返回 io.ErrShortWrite 并提前结束，随后 CloseWrite 把**截断**
// 当作正常半关闭传播给对端（静默截断，见 CopyFull 文档）。
//
// 关闭语义（C1 范本）：每个方向 CopyFull 完成后向对端 CloseWrite 传播半关闭，
// 而非立即全关——让在途响应仍可被另一方向读回（不截断）。首方向完成后武装
// grace 宽限期：宽限期内另一方向完成则正常收尾；超时视为对端非合作（对 FIN
// 不回应），强制关闭两端解除 Read 阻塞，防 goroutine / FD 泄漏。长连接（双向
// 持续活跃）期间两方向都未完成，计时器不会启动，不误断。正常路径不在此显式
// 关闭任一端的完整连接，由调用方 defer 收尾。
func Pump(a io.ReadWriteCloser, b io.ReadWriteCloser, grace time.Duration) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = CopyFull(b, a)
		CloseWrite(b)
		done <- struct{}{}
	}()
	go func() {
		_, _ = CopyFull(a, b)
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
			// 顺序关键（P0-3 + 半关闭传播）：先 Close 非 Abort 端（net.Conn 等），让其
			// 在途 CopyFull 完成并传播 CloseWrite 到对端；再 Abort Abort 端（mux.Stream）。
			// 若先 Abort mux.Stream，其 done 已关闭，对端 CopyFull 收尾时的 s.CloseWrite()
			// 发送失败，半关闭传播丢失（leaf 原实现即 remote.Close 先、s.Abort 后）。
			for _, end := range []io.ReadWriteCloser{a, b} {
				if _, isAbortable := end.(interface{ Abort() error }); !isAbortable {
					_ = end.Close()
				}
			}
			for _, end := range []io.ReadWriteCloser{a, b} {
				if ab, isAbortable := end.(interface{ Abort() error }); isAbortable {
					_ = ab.Abort()
				}
			}
			for remaining > 0 { // 关闭后 Read/Write 立即返回，等待 goroutine 退出
				<-done
				remaining--
			}
			return
		}
	}
}

// NormalizeListenAddr 将裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防
// LAN 暴露 + Windows 防火墙弹窗）；显式 IP/主机名/通配地址 保持原样。
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
