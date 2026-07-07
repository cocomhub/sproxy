// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"time"
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
		// 数据帧：尝试发送，失败时入重传队列
		if err := m.conn.Send(m.Context(), frame); err != nil {
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
