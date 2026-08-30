// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"encoding/binary"
	"errors"
)

// FrameDatagram 是 UDP 数据报帧类型（0x08）：负载 = [4B flowID big-endian][datagram]。
// 与流（FrameData）正交：数据报按消息边界传输（UDP 语义），不走流式窗口/重传。
// flowID 标识 UDP 会话（sclient udp map 单端口映射用 0；未来可扩多流）。
const FrameDatagram FrameType = 0x08

const (
	// datagramFlowLen 是帧负载中 flowID 的字节数。
	datagramFlowLen = 4
	// MaxDatagramPayload 是单条 UDP 数据报负载上限
	// （帧负载上限 65535 - flowID 4 字节；UDP 理论最大 65507，均满足）。
	MaxDatagramPayload = 65535 - datagramFlowLen
)

// ErrDatagramTooLarge 是数据报超过 MaxDatagramPayload 时的错误。
var ErrDatagramTooLarge = errors.New("mux: datagram too large")

// ErrDatagramDrop 是发送缓冲（writeCh）满、数据报被丢弃时的错误（UDP 语义：
// 背压自然丢包，调用方应 log+continue 而非终止）。
var ErrDatagramDrop = errors.New("mux: datagram dropped")

// DatagramHandler 处理收到的 UDP 数据报（flowID 标识会话，data 为完整数据报负载）。
// 由 readLoop 单线程调用，handler 内不得阻塞（如需耗时操作自行 go）。
type DatagramHandler func(flowID uint32, data []byte)

// SendDatagram 发送一条 UDP 数据报。经 writeCh 串行化（与流写共享单写者，防并发
// 写底层 xfer.Conn）；isRaw 直发（UDP 尽力而为，不重传，符合数据报语义）。
func (m *Mux) SendDatagram(flowID uint32, data []byte) error {
	if len(data) > MaxDatagramPayload {
		return ErrDatagramTooLarge
	}
	payload := make([]byte, datagramFlowLen+len(data))
	binary.BigEndian.PutUint32(payload, flowID)
	copy(payload[datagramFlowLen:], data)
	frame := EncodeFrame(0, FrameDatagram, payload)
	select {
	case m.writeCh <- writeMsg{data: frame, isRaw: true, datagram: true}:
		return nil
	case <-m.done:
		return ErrMuxClosed
	default:
		// writeCh 满（拥塞/对端不消费）→ 丢弃该数据报（UDP 语义，背压自然丢包），
		// 不让单条数据报阻塞整个映射（防双向头阻塞）。
		return ErrDatagramDrop
	}
}

// SetDatagramHandler 注册 UDP 数据报处理函数（运行期可调；传 nil 清除）。
func (m *Mux) SetDatagramHandler(h DatagramHandler) {
	m.datagramMu.Lock()
	m.datagramHandler = h
	m.datagramMu.Unlock()
}

// getDatagramHandler 返回当前数据报处理函数（readLoop 调用）。
func (m *Mux) getDatagramHandler() DatagramHandler {
	m.datagramMu.RLock()
	defer m.datagramMu.RUnlock()
	return m.datagramHandler
}
