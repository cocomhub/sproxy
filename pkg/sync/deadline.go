// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"net"
	"sync"
	"time"
)

// deadlineConn 包装 net.Conn，让 SetReadDeadline/SetWriteDeadline/SetDeadline
// 真正生效（到点强制 Close 底层连接）。
//
// 背景：mesh 流（pkg/tunnel/mux.MuxStreamConn）的 SetDeadline 系列是 no-op，
// 而 http.Transport 依赖这些 deadline 实现 ResponseHeaderTimeout / IdleConnTimeout。
// 若直接以 mesh 流作为 DialContext 的返回连接，对端停读时请求可无限挂起
// （审查 I-4 / DoD 8）。本包装用 time.Timer 追踪最近读写 deadline，到点 Close
// 底层连接，使阻塞的 Read/Write 返回错误，从而让 http.Transport 超时真正生效。
type deadlineConn struct {
	net.Conn
	mu    sync.Mutex
	rd    time.Time
	wd    time.Time
	timer *time.Timer
}

// wrapDeadline 返回 deadline-aware 连接包装。
//
// 对原生 deadline 已生效的连接（TCP）也安全（行为等价，多一层 timer 兜底）；
// 主要用途是把 mesh 流（deadline no-op）包装成 http.Transport 可依赖超时的连接。
func wrapDeadline(c net.Conn) net.Conn {
	if c == nil {
		return nil
	}
	return &deadlineConn{Conn: c}
}

// SetDeadline 同时设置读写截止时间（与 net.Conn 语义一致）。
func (d *deadlineConn) SetDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rd, d.wd = t, t
	return d.armLocked()
}

// SetReadDeadline 设置读截止时间。
func (d *deadlineConn) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rd = t
	return d.armLocked()
}

// SetWriteDeadline 设置写截止时间。
func (d *deadlineConn) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.wd = t
	return d.armLocked()
}

// Close 停止 deadline timer 并关闭底层连接。
func (d *deadlineConn) Close() error {
	d.mu.Lock()
	d.clearTimerLocked()
	d.mu.Unlock()
	return d.Conn.Close()
}

// armLocked 根据当前 rd/wd 重新武装 timer（取最近的未过期 deadline）。
// 调用方须持有 d.mu。
func (d *deadlineConn) armLocked() error {
	d.clearTimerLocked()
	next := d.nextDeadlineLocked()
	if next.IsZero() {
		return nil
	}
	dur := time.Until(next)
	if dur <= 0 {
		// 已过期：立即关闭（AfterFunc 异步执行，避免持锁调用 Close）
		d.timer = time.AfterFunc(0, d.expire)
		return nil
	}
	d.timer = time.AfterFunc(dur, d.expire)
	return nil
}

// clearTimerLocked 停止并丢弃当前 timer。调用方须持有 d.mu。
func (d *deadlineConn) clearTimerLocked() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}

// nextDeadlineLocked 返回 rd/wd 中更早的非零时间；都为 0 时返回零值。
// 调用方须持有 d.mu。
func (d *deadlineConn) nextDeadlineLocked() time.Time {
	var next time.Time
	if !d.rd.IsZero() && (next.IsZero() || d.rd.Before(next)) {
		next = d.rd
	}
	if !d.wd.IsZero() && (next.IsZero() || d.wd.Before(next)) {
		next = d.wd
	}
	return next
}

// expire 是 deadline 到点后的回调：强制关闭底层连接，使阻塞的 Read/Write 返回错误。
// 不持有 d.mu（回调内加锁会与 armLocked 相互死锁）；底层 net.Conn.Close 并发安全，
// 即使与 Close() 竞态也仅产生无害的重复关闭错误。
func (d *deadlineConn) expire() {
	_ = d.Conn.Close()
}
