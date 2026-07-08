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
		close(s.dataCh)
		close(s.done)
	}
	s.closeMu.Unlock()
}

func (s *stream) pushData(payload []byte) {
	s.closeMu.Lock()
	select {
	case s.dataCh <- payload:
	case <-s.done:
	}
	s.closeMu.Unlock()
}

func (s *stream) pushEOF() {
	s.closeMu.Lock()
	select {
	case s.dataCh <- nil:
	case <-s.done:
	}
	s.closeMu.Unlock()
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
		select {
		case data, ok := <-s.dataCh:
			if !ok {
				return 0, s.rejectedOrClosedErr()
			}
			if data == nil {
				return 0, io.EOF
			}
			s.rBuf = data
			s.rOff = 0
			s.mux.sendWindowUpdateUnsafe(s.id, int32(len(data)))
		case <-s.done:
			return 0, s.rejectedOrClosedErr()
		}
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

var closeMarker = make([]byte, 0)

type writeMsg struct {
	streamID StreamID
	data     []byte // nil=CloseWrite, empty([]byte{})=Close
	isRaw    bool
}
