// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// sendWindowUpdateUnsafe 发送窗口更新帧。
// 必须在 readLoop goroutine 中或已持有 m.mu 锁时调用。
func (m *Mux) sendWindowUpdateUnsafe(sid StreamID, size int32) {
	if size <= 0 {
		return
	}
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, uint32(size))
	frame := EncodeFrame(sid, FrameWindowUpdate, payload)
	select {
	case <-m.done:
	default:
		select {
		case m.writeCh <- writeMsg{streamID: sid, data: frame, isRaw: true}:
		default:
		}
	}
}

// retransmitEntry 存储待重传的帧。
type retransmitEntry struct {
	frame    []byte
	retries  int
	deadline time.Time
}

func (m *Mux) sendFrame(msg writeMsg) {
	m.metrics.FramesSent.Add(1)
	if msg.isRaw {
		if err := m.conn.Send(m.Context(), msg.data); err != nil {
			m.metrics.Errors.Add(1)
			if msg.datagram {
				// UDP 数据报：发送瞬时失败只丢弃（尽力而为），不关闭 mux——
				// 避免单条数据报失败连带杀掉同 mux 的 TCP 流/HTTP 中继。
				m.logger.Debug("mux: datagram send dropped", "err", err)
				return
			}
			m.logger.Error("mux: send error", "err", err)
			m.Close()
		}
		return
	}

	var frame []byte
	switch {
	case msg.data == nil:
		frame = EncodeFrame(msg.streamID, FrameCloseWrite, nil)
	case len(msg.data) == 0:
		frame = EncodeFrame(msg.streamID, FrameClose, nil)
	default:
		frame = EncodeFrame(msg.streamID, FrameData, msg.data)
	}

	if len(msg.data) > 0 {
		// 数据帧：尝试发送，失败时入重传队列。
		//
		// **重传的前提（issue #215）**：传输必须保证「消息边界由实现保证」（xfer.Conn 契约）——
		// 即 Send 失败时该帧**一个字节都不在线上**。各传输实现经 iostream.WriteFull 写足，
		// 且写错误一律关闭连接（见 pkg/tunnel/xfer/internal/tcp/tcp.go 的注释；门禁见
		// internal/archcheck 的 TestXferSendUsesWriteFull）。若某传输违反该前提（半截帧留在
		// 线上），重传会把整帧再投一次 ⇒ 对端的长度前缀定界**永久错位** ⇒ 字节流污染
		// （上层隧道流是分块加密的，表现为 GCM 认证失败，且重传无法纠正）。
		if err := m.conn.Send(m.Context(), frame); err != nil {
			if errors.Is(err, xfer.ErrConnClosed) {
				// 连接已关：重传必然失败（实现已按全或无关闭连接），立即收口，
				// 不必等 maxRetries 的退避窗口。
				m.metrics.Errors.Add(1)
				m.logger.Error("mux: send error（连接已关）", "stream", msg.streamID, "err", err)
				go m.Close()
				return
			}
			m.logger.Warn("mux: send failed, queued for retransmit", "stream", msg.streamID, "err", err)
			m.enqueueRetransmit(frame, 0)
			return
		}
		return
	}

	// 控制帧（CloseWrite/Close）：不重传，失败直接关闭
	if err := m.conn.Send(m.Context(), frame); err != nil {
		m.metrics.Errors.Add(1)
		m.logger.Error("mux: send error, closing mux", "stream", msg.streamID, "err", err)
		m.Close()
		return
	}
	if len(msg.data) == 0 && msg.data != nil {
		m.removeStream(msg.streamID, true)
	}
}

// enqueueRetransmit 将失败帧加入重传队列。
func (m *Mux) enqueueRetransmit(frame []byte, retries int) {
	entry := retransmitEntry{
		frame:    frame,
		retries:  retries,
		deadline: time.Now().Add(retryBaseDelay),
	}
	m.retransmitMu.Lock()
	if len(m.retransmitQ) >= 256 {
		m.retransmitQ = m.retransmitQ[1:]
	}
	m.retransmitQ = append(m.retransmitQ, entry)
	m.retransmitMu.Unlock()
}

// scanRetransmitQ 扫描重传队列，重试到期的条目。
func (m *Mux) scanRetransmitQ() {
	m.retransmitMu.Lock()
	if len(m.retransmitQ) == 0 {
		m.retransmitMu.Unlock()
		return
	}

	now := time.Now()
	remaining := make([]retransmitEntry, 0, len(m.retransmitQ))

	for _, entry := range m.retransmitQ {
		if entry.deadline.After(now) {
			remaining = append(remaining, entry)
			continue
		}
		if err := m.conn.Send(m.Context(), entry.frame); err == nil {
			continue
		}
		entry.retries++
		if entry.retries >= maxRetries {
			m.metrics.Errors.Add(1)
			m.logger.Error("mux: retransmit exhausted", "retries", entry.retries)
			m.retransmitMu.Unlock()
			go m.Close()
			return
		}
		entry.deadline = now.Add(backoffDuration(entry.retries))
		remaining = append(remaining, entry)
	}
	m.retransmitQ = remaining
	m.retransmitMu.Unlock()
}

func backoffDuration(retries int) time.Duration {
	return min(retryBaseDelay<<min(retries-1, 5), retryMaxDelay)
}
