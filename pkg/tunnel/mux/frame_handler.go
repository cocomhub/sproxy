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
	FramePadding:      handlePaddingFrame,
	FramePong:         handlePongFrame,
	FrameWindowUpdate: handleWindowUpdateFrame,
	FrameDatagram:     handleDatagramFrame,
}

// handleDatagramFrame 处理 UDP 数据报帧：解析 flowID + 数据报，交给注册的 handler。
//
// handler 由**本函数同步调用**（DatagramHandler 契约：handler 内不得阻塞，慢操作自行 go）。
// 因此这里把「handler 吃掉 readLoop 多久」记入 ReadLoopDatagram：2026-09-16 审计确认
// relay 的 UDP 出口曾在此同步 `WriteToUDP`（慢/阻塞的 UDP 写会停摆整个 mux，同源阻塞口①）。
// 该指标能把「有没有真的阻塞、阻塞多久」变成可见事实（修复后应≈0）。
func handleDatagramFrame(m *Mux, sid StreamID, payload []byte) {
	if len(payload) < datagramFlowLen {
		m.metrics.Errors.Add(1)
		return
	}
	flowID := binary.BigEndian.Uint32(payload[:datagramFlowLen])
	data := payload[datagramFlowLen:]
	h := m.getDatagramHandler()
	if h == nil {
		return
	}
	start := m.metrics.ReadLoopDatagram.enter()
	h(flowID, data)
	m.metrics.ReadLoopDatagram.leave(start)
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
		m.streamActiveOpened()
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
		m.streamActiveClosed()
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

// handlePaddingFrame 处理空闲填充帧（roadmap §5.3 P1 被动伪装层）。
// 填充帧无业务语义，仅保持连接活跃形态（DPI 难判断空闲）；对端忽略 + 计数。
func handlePaddingFrame(m *Mux, sid StreamID, payload []byte) {
	m.metrics.PaddingReceived.Add(1)
}

// handlePingFrame 处理 Ping 帧：回复 Pong。
//
// **不得在 readLoop 内同步发送**（2026-09-16 审计的同源阻塞口②）：原实现直接
// `m.conn.Send`，而 readLoop 是单 goroutine 串行处理所有帧 —— 发送一慢/一卡就停摆
// readLoop，本侧随即读不到对端的 Pong，被自己的 pingLoop 以 90s 心跳超时**拆掉整条
// 连接**（含该连接上所有健康流；hub 拓扑下等于一个节点的全部用户流量）。
// 现改为经 writeCh 交给单写者 writeLoop（与 rejectStream 同一约定），writeCh 满时
// **不丢账**：置 pendingPong 由 50ms ticker 补送（Pong 幂等，多次 Ping 可合并为一次）。
func handlePingFrame(m *Mux, sid StreamID, payload []byte) {
	start := m.metrics.ReadLoopPong.enter()
	defer m.metrics.ReadLoopPong.leave(start)
	if m.trySendPong() {
		return
	}
	m.pendingPong.Store(true)
	m.metrics.PongsCoalesced.Add(1)
}

// trySendPong 把一条 Pong 帧交给 writeLoop；返回 false 表示 writeCh 已满（未投递，
// 调用方必须置 pendingPong 让 ticker 补送，否则该 Pong 丢失 ⇒ 对端心跳超时拆连接）。
//
// 为何用 msg.pong 而非直接 conn.Send：所有出向字节经单写者 writeLoop 串行化（防并发写
// 底层 xfer.Conn）；代价是该 Pong 会排在 writeCh **已有帧之后（至多 256 帧 = writeCh 容量）**，
// 而 Pong 幂等且下一轮**对端 Ping**（本侧 pingLoop 只发 Ping/收 Pong）会再触发一次回复（对端窗口 90s）⇒ 最坏只是晚一个心跳周期，
// 而改造前「readLoop 内直接 conn.Send」的代价是整条连接停摆（本改造要消除的正是它）。
func (m *Mux) trySendPong() bool {
	pong, encErr := EncodeFrame(0, FramePong, nil)
	if encErr != nil { // 不可达：负载为 nil
		return true
	}
	select {
	case <-m.done:
		return true // mux 已终止：对端不会再等这次心跳
	default:
	}
	select {
	case m.writeCh <- writeMsg{data: pong, isRaw: true, pong: true}:
		return true
	default:
		return false
	}
}

// flushPendingPong 补送被 writeCh 打满挤掉的 Pong（由 writeLoop 的 50ms ticker 驱动）。
//
// 有界性：pendingPong 是单个布尔（Pong 幂等 ⇒ 多次 Ping 合并为一次），故不增长、
// 不产生无界 goroutine；Swap 取走后投递再失败要**原样放回**，成功则本次只投一帧。
func (m *Mux) flushPendingPong() {
	if !m.pendingPong.Swap(false) {
		return
	}
	if !m.trySendPong() {
		m.pendingPong.Store(true)
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
