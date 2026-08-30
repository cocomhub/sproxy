// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"net"
	"sync"
	"time"
)

// deadlineConn 包装 net.Conn，提供两层超时兜底：
//
//  1. 活跃读写超时（readTimeout/writeTimeout）：每次 Read/Write 调用用 timer 监督，
//     若在该方向上阻塞超过时长则强制关闭底层连接。这是 mesh 流（SetDeadline no-op）
//     下的关键兜底——Go 1.26 http.Transport（HTTP/1.1）从不调用 conn 的
//     SetReadDeadline/SetWriteDeadline（超时全走内部 timer + pc.close()），写路径对端
//     停读时 TCP 缓冲满、Write 永久阻塞（审查 I-1）；活跃写超时到点关闭使 Write 返回
//     错误，闭环 DoD 8「对端停读→超时失败」（审查 I-2）。
//  2. 显式 deadline（SetReadDeadline/SetWriteDeadline/SetDeadline）：与 net.Conn 语义
//     一致，调用方显式设置时同样到点关闭（兼容原生 deadline 生效的连接）。
//
// 关闭路径探测 Abort() 接口：mesh 流 MuxStreamConn.Close 经 writeCh 发 FrameClose，
// 写通道打满且 done 未关时会永久阻塞；Abort 是非阻塞强制释放（审查 I-3）。
type deadlineConn struct {
	net.Conn
	mu           sync.Mutex
	rd, wd       time.Time // 显式 deadline
	readTimeout  time.Duration
	writeTimeout time.Duration
	timer        *time.Timer
	closed       bool
}

// wrapDeadline 返回 deadline-aware 连接包装。
// readTimeout/writeTimeout <= 0 表示不启用对应方向的活跃超时。
func wrapDeadline(c net.Conn, readTimeout, writeTimeout time.Duration) net.Conn {
	if c == nil {
		return nil
	}
	return &deadlineConn{Conn: c, readTimeout: readTimeout, writeTimeout: writeTimeout}
}

// Read 读取并受读方向活跃超时/显式 deadline 约束。
func (d *deadlineConn) Read(p []byte) (int, error) {
	if err := d.arm(true); err != nil {
		return 0, err
	}
	n, err := d.Conn.Read(p)
	d.disarm()
	return n, err
}

// Write 写入并受写方向活跃超时/显式 deadline 约束。
func (d *deadlineConn) Write(p []byte) (int, error) {
	if err := d.arm(false); err != nil {
		return 0, err
	}
	n, err := d.Conn.Write(p)
	d.disarm()
	return n, err
}

// arm 启动一次 Read/Write 的超时监督。read=true 时取读方向配置。
// 已关闭时返回 net.ErrClosed（快速失败）。
func (d *deadlineConn) arm(read bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return net.ErrClosed
	}
	d.clearTimerLocked()
	var deadline time.Time
	var timeout time.Duration
	if read {
		deadline, timeout = d.rd, d.readTimeout
	} else {
		deadline, timeout = d.wd, d.writeTimeout
	}
	var dur time.Duration
	hasDur := false
	if !deadline.IsZero() {
		dur = time.Until(deadline)
		hasDur = true
	}
	if timeout > 0 && (!hasDur || timeout < dur) {
		dur = timeout
		hasDur = true
	}
	if !hasDur {
		return nil
	}
	if dur <= 0 {
		d.timer = time.AfterFunc(0, d.expire)
	} else {
		d.timer = time.AfterFunc(dur, d.expire)
	}
	return nil
}

// disarm 停止本轮 Read/Write 的超时 timer（正常返回后调用，防止到点误关连接）。
func (d *deadlineConn) disarm() {
	d.mu.Lock()
	d.clearTimerLocked()
	d.mu.Unlock()
}

// SetDeadline 同时设置读写截止时间（与 net.Conn 语义一致；生效于下一次 arm）。
func (d *deadlineConn) SetDeadline(t time.Time) error {
	d.mu.Lock()
	d.rd, d.wd = t, t
	d.mu.Unlock()
	return nil
}

// SetReadDeadline 设置读截止时间。
func (d *deadlineConn) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.rd = t
	d.mu.Unlock()
	return nil
}

// SetWriteDeadline 设置写截止时间。
func (d *deadlineConn) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	d.wd = t
	d.mu.Unlock()
	return nil
}

// Close 停止 timer 并关闭底层连接（幂等）。
func (d *deadlineConn) Close() error {
	d.mu.Lock()
	d.clearTimerLocked()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()
	return d.forceClose()
}

// expire 是超时到点回调：标记已关闭并强制关闭底层连接。
func (d *deadlineConn) expire() {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	d.mu.Unlock()
	_ = d.forceClose()
}

// forceClose 关闭底层连接；mesh 流（MuxStreamConn）优先 Abort（非阻塞）避免 Close
// 在 writeCh 打满时永久阻塞（审查 I-3）。
func (d *deadlineConn) forceClose() error {
	if a, ok := d.Conn.(interface{ Abort() error }); ok {
		return a.Abort()
	}
	return d.Conn.Close()
}

// clearTimerLocked 停止并丢弃当前 timer。调用方须持有 d.mu。
func (d *deadlineConn) clearTimerLocked() {
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
}
