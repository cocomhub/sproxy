// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"time"
)

func (m *Mux) handleFrame(raw []byte) {
	m.metrics.FramesReceived.Add(1)
	sid, ftype, payload, err := DecodeFrame(raw)
	if err != nil {
		m.metrics.Errors.Add(1)
		m.logger.Warn("mux: invalid frame", "err", err)
		return
	}
	handler, ok := frameHandlers[ftype]
	if !ok {
		m.metrics.Errors.Add(1)
		m.logger.Warn("mux: unknown frame type", "type", ftype)
		return
	}
	handler(m, sid, payload)
}

// frameHandler 是帧处理函数的类型。
type frameHandler func(m *Mux, sid StreamID, payload []byte)

// frameHandlers 是帧类型到处理函数的分发表。
var frameHandlers = map[FrameType]frameHandler{
	FrameData:         handleDataFrame,
	FrameOpen:         handleOpenFrame,
	FrameReject:       handleRejectFrame,
	FrameClose:        handleCloseFrame,
	FrameCloseWrite:   handleCloseWriteFrame,
	FramePing:         handlePingFrame,
	FramePong:         handlePongFrame,
	FrameWindowUpdate: handleWindowUpdateFrame,
}

// handleDataFrame 处理 Data 帧：将负载推送到对应流。
func handleDataFrame(m *Mux, sid StreamID, payload []byte) {
	m.mu.Lock()
	s, ok := m.streams[sid]
	m.mu.Unlock()
	if !ok {
		return
	}
	s.pushData(payload)
}

// handleOpenFrame 处理 Open 帧：创建新流并通过 acceptCh 通知。
func handleOpenFrame(m *Mux, sid StreamID, payload []byte) {
	m.mu.Lock()
	if _, exists := m.streams[sid]; exists {
		m.mu.Unlock()
		return
	}
	if m.maxStreams > 0 && m.activeStreams.Load() >= m.maxStreams {
		m.mu.Unlock()
		m.rejectStream(sid, false)
		m.metrics.StreamsRejected.Add(1)
		m.metrics.StreamsRejectedMaxStr.Add(1)
		return
	}
	s := newStream(sid, m)
	m.streams[sid] = s
	m.mu.Unlock()
	select {
	case m.acceptCh <- s:
		m.activeStreams.Add(1)
		m.metrics.Streams.Opened.Add(1)
	default:
		m.rejectStream(sid, true)
		m.metrics.StreamsRejected.Add(1)
		m.metrics.StreamsRejectedAccCh.Add(1)
		m.mu.Lock()
		delete(m.streams, sid)
		s.reject()
		m.mu.Unlock()
	}
}

// handleRejectFrame 处理 Reject 帧：移除流并标记为已拒绝。
func handleRejectFrame(m *Mux, sid StreamID, payload []byte) {
	m.mu.Lock()
	s, ok := m.streams[sid]
	if ok {
		delete(m.streams, sid)
	}
	m.mu.Unlock()
	if ok {
		m.activeStreams.Add(-1)
		s.reject()
	}
	m.metrics.StreamsRejected.Add(1)
}

// handleCloseFrame 处理 Close 帧：移除流并关闭通道。
func handleCloseFrame(m *Mux, sid StreamID, payload []byte) {
	m.removeStream(sid, true)
	m.metrics.Streams.Closed.Add(1)
}

// handleCloseWriteFrame 处理 CloseWrite 帧：推送 EOF 到对应流。
func handleCloseWriteFrame(m *Mux, sid StreamID, payload []byte) {
	m.mu.Lock()
	s, ok := m.streams[sid]
	if !ok {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	s.pushEOF()
}

// handlePingFrame 处理 Ping 帧：立即回复 Pong。
func handlePingFrame(m *Mux, sid StreamID, payload []byte) {
	_ = m.conn.Send(m.Context(), EncodeFrame(0, FramePong, nil))
}

// handlePongFrame 处理 Pong 帧：记录最后 Pong 时间。
func handlePongFrame(m *Mux, sid StreamID, payload []byte) {
	m.lastPongNano.Store(time.Now().UnixNano())
	m.metrics.PongsReceived.Add(1)
}

// handleWindowUpdateFrame 处理 WindowUpdate 帧：更新流发送窗口并通知写入 goroutine。
func handleWindowUpdateFrame(m *Mux, sid StreamID, payload []byte) {
	m.mu.Lock()
	s, ok := m.streams[sid]
	m.mu.Unlock()
	if !ok {
		return
	}
	s.windowSize.Add(int32(binary.BigEndian.Uint32(payload)))
	select {
	case s.windowUpdateCh <- struct{}{}:
	default:
	}
}
