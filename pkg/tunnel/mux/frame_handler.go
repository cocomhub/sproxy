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
	FrameDatagram:     handleDatagramFrame,
}

// handleDatagramFrame 处理 UDP 数据报帧：解析 flowID + 数据报，交给注册的 handler。
func handleDatagramFrame(m *Mux, sid StreamID, payload []byte) {
	if len(payload) < datagramFlowLen {
		m.metrics.Errors.Add(1)
		return
	}
	flowID := binary.BigEndian.Uint32(payload[:datagramFlowLen])
	data := payload[datagramFlowLen:]
	if h := m.getDatagramHandler(); h != nil {
		h(flowID, data)
	}
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
	if pong, pErr := EncodeFrame(0, FramePong, nil); pErr == nil {
		_ = m.conn.Send(m.Context(), pong)
	}
}

// handlePongFrame 处理 Pong 帧：记录最后 Pong 时间。
func handlePongFrame(m *Mux, sid StreamID, payload []byte) {
	m.lastPongNano.Store(time.Now().UnixNano())
	m.metrics.PongsReceived.Add(1)
}

// handleWindowUpdateFrame 处理 WindowUpdate 帧：更新流发送窗口并通知写入 goroutine。
//
// 负载长度必须先校验：本处理器在 **readLoop goroutine** 中执行，`binary.BigEndian.Uint32`
// 对短负载会 panic ⇒ 整个进程崩溃（对端可控输入的远程 DoS，2026-09-16 审计确认）。
// 与 handleDatagramFrame 同一口径：短帧计为协议错误后丢弃，不改状态。
// 日志用 Debug：短帧是远端可控输入，Warn 会给对端制造日志刷屏的机会。
//
// 另外，增量必须 **> 0** 才生效（见下方注释）：≥2^31 的负载会被 int32 解释成负数 delta。
func handleWindowUpdateFrame(m *Mux, sid StreamID, payload []byte) {
	if len(payload) < windowUpdateLen {
		m.metrics.Errors.Add(1)
		m.logger.Debug("mux: short window update frame", "stream", sid, "len", len(payload))
		return
	}
	m.mu.Lock()
	s, ok := m.streams[sid]
	m.mu.Unlock()
	if !ok {
		return
	}
	// 增量必须 > 0：合法编码侧只发 size>0（窗口更新的构造见 retransmit.go），而
	// int32 会把 ≥2^31 的负载解释成**负数 delta**，把发送窗口打成负（后续 Write 会一直在
	// `for ws <= 0` 里等到 done）。非正增量按协议错误计数并丢弃，不改窗口、不惊动写者。
	// 注：对端保持沉默即可达到同样效果，故这不是新增 DoS，属纵深防御。
	delta := int32(binary.BigEndian.Uint32(payload[:windowUpdateLen]))
	if delta <= 0 {
		m.metrics.Errors.Add(1)
		m.logger.Debug("mux: non-positive window update", "stream", sid, "delta", delta)
		return
	}
	s.windowSize.Add(delta)
	select {
	case s.windowUpdateCh <- struct{}{}:
	default:
	}
}
