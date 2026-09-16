// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"errors"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// maxRetransmitQ 是重传队列容量。
//
// 队列满不再静默丢最旧帧（drop-oldest），而是关闭 mux——见 enqueueRetransmit 的说明。
//
// 取值依据（2026-09-16 复核量化，勿当随手取的数）：入队前提是 Send **失败**，而 tcp/quic/
// webrtc/ws 在写错误后即置 closed，后续 Send 立即返回 `ErrConnClosed` 走快速关闭分支 ⇒ 队列
// 很难涨到 256。**gRPC 是例外**：`ext/grpc/grpc.go` 直接透传 `client.Send` 的错误、不映射
// `ErrConnClosed` ⇒ 会持续入队，此时上层快速写 >256 帧（≈16 MiB）即可在「单条耗尽路径约
// 3.1s」（100+200+400+800+1600ms 退避）之前触界。即本路径最坏只是比「重试耗尽」版**早约
// 3 秒**关掉连接，而旧行为（drop-oldest）同期还在静默损坏该流。
const maxRetransmitQ = 256

// sendWindowUpdateUnsafe 汇报「本侧已消费 size 字节」，以恢复对端的发送窗口。
//
// 信用**不得静默丢失**（2026-09-16 审计确认的代码路径）：writeCh 打满时原先直接丢弃该帧
// （`default:` 空分支，无记账），而信用只在本侧**再次消费到新负载**时才补送——若本侧已
// 读完全部数据（无更多负载可读），对端就永久卡在 `stream.Write` 的
// `for s.windowSize.Load() <= 0` 上：单流活性永久丧失，且无日志、无指标，极难排查。
//
// 现在改为：投递失败只**记账**（`stream.pendingWindowUpdate`），由 writeLoop 的 50ms ticker
// 经 flushPendingWindowUpdates 补送。
//
// 名字沿用历史（审计报告亦按此名引用）：早期实现需要在 readLoop goroutine 中或持 `m.mu`
// 下调用才能查流表；现在只做原子记账 + 非阻塞投递，**任何 goroutine 均可调用**。
func (m *Mux) sendWindowUpdateUnsafe(s *stream, size int32) {
	if size <= 0 {
		return
	}
	if !m.trySendWindowUpdate(s.id, size) {
		s.pendingWindowUpdate.Add(size)
	}
}

// trySendWindowUpdate 尝试把一条窗口更新帧交给 writeLoop；返回 false 表示 writeCh 已满
// （未投递，调用方必须记账补送，否则对端窗口永久少这一笔）。
func (m *Mux) trySendWindowUpdate(sid StreamID, size int32) bool {
	payload := make([]byte, windowUpdateLen)
	binary.BigEndian.PutUint32(payload, uint32(size))
	frame, encErr := EncodeFrame(sid, FrameWindowUpdate, payload)
	if encErr != nil { // 不可达：负载为固定 windowUpdateLen 字节
		return true
	}
	select {
	case <-m.done:
		return true // mux 已终止：对端不会再等这笔信用
	default:
	}
	select {
	case m.writeCh <- writeMsg{streamID: sid, data: frame, isRaw: true}:
		return true
	default:
		return false
	}
}

// flushPendingWindowUpdates 补送被 writeCh 打满挤掉的窗口信用（由 writeLoop 的 50ms ticker 驱动）。
//
// 有界性（前提：**守协议**的对端）：本侧只为**实际消费过**的字节发信用，而窗口不超过
// DefaultWindowSize(65536)，因此单流待补送量 ≤ 65536；队列满也只是把补送推到下一次 tick，
// 不会无限增长。若对端违约持续超窗口灌数据、且本侧出向长期拥塞，累计量理论上可让 int32
// 溢出为负 ⇒ 该流信用不再补发（只伤违约对端自身，无 panic、无内存增长）；饱和钳制属后续片。
// 安全性：补送按 sid 编码，若该流已注销/已关，对端收到未知流的帧会直接丢弃（无副作用）。
func (m *Mux) flushPendingWindowUpdates() {
	m.mu.Lock()
	pending := make([]*stream, 0, len(m.streams))
	for _, s := range m.streams {
		if s.pendingWindowUpdate.Load() > 0 {
			pending = append(pending, s)
		}
	}
	m.mu.Unlock()

	for _, s := range pending {
		size := s.takePendingWindowUpdate()
		if size <= 0 {
			continue
		}
		if !m.trySendWindowUpdate(s.id, size) {
			s.pendingWindowUpdate.Add(size) // 仍未送达：原样加回，等下一次 tick
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
		if f, fErr := EncodeFrame(msg.streamID, FrameCloseWrite, nil); fErr == nil {
			frame = f
		} else {
			return
		}
	case len(msg.data) == 0:
		if f, fErr := EncodeFrame(msg.streamID, FrameClose, nil); fErr == nil {
			frame = f
		} else {
			return
		}
	default:
		f, fErr := EncodeFrame(msg.streamID, FrameData, msg.data)
		if fErr != nil {
			// 负载超限说明上游未按 MaxFramePayload 收敛（编程错误）：记指标并关闭该 mux，
			// 绝不截断发送（截断=静默丢字节 → 对端定界错位）。
			m.metrics.Errors.Add(1)
			m.logger.Error("mux: frame payload too large, closing mux", "stream", msg.streamID, "len", len(msg.data), "err", fErr)
			go m.Close()
			return
		}
		frame = f
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
//
// 队列满时**不得**静默丢弃最旧的帧（2026-09-16 审计确认的代码路径）：入队者都是**数据帧**，
// 丢弃它等于上层已 `Write` 的字节永久消失（上层分块加密表现为 AEAD 认证失败，重传无法纠正）；
// 而且后续帧已按 writeCh 顺序发出，补发的旧帧必然**晚于**其后帧到达 ⇒ 同流乱序。
// 改为与本文件其它失败取舍一致的做法：宁可让整条连接**显式失败**（Close），也不产出静默错位的
// 字节流。队列满意味着已有 maxRetransmitQ 帧 Send 失败，连接实际上已不可用；
// 与「重传重试耗尽」（maxRetries）路径后果一致，但**可能更早**（队列满即关，而耗尽路径要等完
// 100+200+400+800+1600ms 退避，最坏早约 3.1s），且**最可能在 gRPC 传输上触达**（不映射
// ErrConnClosed，量化依据见 maxRetransmitQ）。
func (m *Mux) enqueueRetransmit(frame []byte, retries int) {
	entry := retransmitEntry{
		frame:    frame,
		retries:  retries,
		deadline: time.Now().Add(retryBaseDelay),
	}
	m.retransmitMu.Lock()
	if len(m.retransmitQ) >= maxRetransmitQ {
		m.retransmitMu.Unlock()
		m.metrics.Errors.Add(1)
		m.logger.Error("mux: retransmit queue full, closing mux", "queued", maxRetransmitQ)
		go m.Close()
		return
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
