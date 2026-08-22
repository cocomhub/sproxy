// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// stream 是 Stream 接口的内部实现。
type stream struct {
	id   StreamID
	mux  *Mux
	rBuf []byte
	rOff int
	rMu  sync.Mutex

	closeMu sync.Mutex
	dataCh  chan []byte
	done    chan struct{}

	windowSize     atomic.Int32
	windowUpdateCh chan struct{}

	rejected atomic.Bool
}

func newStream(id StreamID, m *Mux) *stream {
	s := &stream{
		id:     id,
		mux:    m,
		dataCh: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	s.windowSize.Store(DefaultWindowSize)
	s.windowUpdateCh = make(chan struct{}, 8)
	return s
}

func (s *stream) ID() StreamID { return s.id }

func (s *stream) closeChannels() {
	s.closeMu.Lock()
	select {
	case <-s.done:
	default:
		// 只关 done 不关 dataCh。Read 通过 done 分支返回关闭错误，而
		// pushData/pushEOF 的 select（dataCh send vs done）在 done 关闭后
		// 可选中 done 分支丢弃负载；若同时关闭 dataCh，select 可能选中对
		// 已关闭 dataCh 的 send 操作而 panic（Abort 本地关流后对端仍可能发数据）。
		close(s.done)
	}
	s.closeMu.Unlock()
}

func (s *stream) pushData(payload []byte) {
	select {
	case s.dataCh <- payload:
	case <-s.done:
	}
}

func (s *stream) pushEOF() {
	select {
	case s.dataCh <- nil:
	case <-s.done:
	}
}

func (s *stream) reject() {
	s.rejected.Store(true)
	s.closeChannels()
}

func (s *stream) rejectedOrClosedErr() error {
	if s.rejected.Load() {
		return fmt.Errorf(errFmtMuxStreamErr, s.id, ErrStreamRejected)
	}
	return fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
}

func (s *stream) Read(p []byte) (n int, err error) {
	if s.rejected.Load() {
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
	}

	s.rMu.Lock()
	defer s.rMu.Unlock()

	for s.rOff >= len(s.rBuf) {
		// 优先消费已缓冲的数据：对端可能已发送数据后立即关闭（FrameClose），
		// 若 select 随机选中 done 分支，已缓冲数据会被跳过误报关闭——I27 拨号
		// 结果帧读取在叶子「接受后立即关」场景的可靠性依赖此行为。仅当无缓冲
		// 数据时才等待新数据或关闭信号。
		var data []byte
		var ok bool
		select {
		case data, ok = <-s.dataCh:
		default:
			select {
			case data, ok = <-s.dataCh:
			case <-s.done:
				return 0, s.rejectedOrClosedErr()
			}
		}
		if !ok {
			return 0, s.rejectedOrClosedErr()
		}
		if data == nil {
			return 0, io.EOF
		}
		s.rBuf = data
		s.rOff = 0
		s.mux.sendWindowUpdateUnsafe(s.id, int32(len(data)))
	}

	n = copy(p, s.rBuf[s.rOff:])
	s.rOff += n
	s.mux.metrics.Streams.BytesRead.Add(int64(n))
	return n, nil
}

func (s *stream) Write(p []byte) (n int, err error) {
	if s.rejected.Load() {
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
	}
	if len(p) == 0 {
		return 0, nil
	}

	select {
	case <-s.done:
		return 0, s.rejectedOrClosedErr()
	default:
	}

	for s.windowSize.Load() <= 0 {
		select {
		case <-s.windowUpdateCh:
		case <-s.done:
			return 0, s.rejectedOrClosedErr()
		}
	}

	writeLen := len(p)
	ws := s.windowSize.Load()
	if int32(writeLen) > ws {
		writeLen = int(ws)
	}

	cp := make([]byte, writeLen)
	copy(cp, p[:writeLen])

	select {
	case s.mux.writeCh <- writeMsg{streamID: s.id, data: cp}:
		s.windowSize.Add(-int32(writeLen))
		s.mux.metrics.Streams.BytesWritten.Add(int64(writeLen))
		return writeLen, nil
	case <-s.done:
		return 0, s.rejectedOrClosedErr()
	}
}

func (s *stream) CloseWrite() error {
	select {
	case s.mux.writeCh <- writeMsg{streamID: s.id, data: nil}:
		return nil
	case <-s.done:
		return fmt.Errorf(errFmtMuxStreamErr, s.id, xfer.ErrConnClosed)
	}
}

func (s *stream) Close() error {
	select {
	case s.mux.writeCh <- writeMsg{streamID: s.id, data: closeMarker}:
		return nil
	case <-s.done:
		return fmt.Errorf(errFmtMuxStreamErr, s.id, xfer.ErrConnClosed)
	}
}

// Abort 立即放弃该流，不经 writeCh（非阻塞）。
//
// 与 Close 的区别：
//   - Close 经 writeCh 向对端发送 FrameClose 优雅关闭；writeCh 打满且 done 未关闭时
//     会永久阻塞（对端停读导致流控窗口耗尽、重传积压的收尾路径）。
//   - Abort 直接关闭本地 done 通道（closeChannels，不经 writeCh），立即解除
//     Read/Write 阻塞，用于收尾/超时强制释放（对齐 meshForwardListen 的非阻塞关闭范本）。
//
// 语义注意：Abort 不向对端发送关闭帧——对端感知本侧已放弃流依赖其自身的
// 半关闭传播或超时机制。Abort 是幂等的（closeChannels 内部有 closeMu + select
// 防重入），重复调用、与 pushData/pushEOF/reject 并发调用均安全。与 retransmitLoop
// 的交互：流被 Abort 后若其数据帧仍在重传队列，对端收到未知流的数据帧会直接丢弃，
// 无副作用。
func (s *stream) Abort() error {
	s.closeChannels()
	return nil
}

var closeMarker = make([]byte, 0)

type writeMsg struct {
	streamID StreamID
	data     []byte // nil=CloseWrite, empty([]byte{})=Close
	isRaw    bool
}
