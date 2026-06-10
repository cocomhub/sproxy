// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// Role 标识 Mux 的角色。
type Role int

const (
	RoleDialer   Role = iota // 主动拨号端，分配奇数 StreamID
	RoleListener             // 监听接受端，分配偶数 StreamID
)

// Stream 代表一条虚拟流，实现 io.ReadWriteCloser。
type Stream struct {
	id   StreamID
	mux  *Mux
	rBuf []byte
	rOff int
	rMu  sync.Mutex

	dataCh chan []byte // 接收的数据帧；nil 表示对方 Close
	done   chan struct{}
}

func newStream(id StreamID, m *Mux) *Stream {
	return &Stream{
		id:     id,
		mux:    m,
		dataCh: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
}

func (s *Stream) ID() StreamID { return s.id }

func (s *Stream) Read(p []byte) (n int, err error) {
	s.rMu.Lock()
	defer s.rMu.Unlock()

	for s.rOff >= len(s.rBuf) {
		select {
		case data, ok := <-s.dataCh:
			if !ok {
				return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
			}
			if data == nil {
				return 0, io.EOF
			}
			s.rBuf = data
			s.rOff = 0
		case <-s.done:
			return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
		}
	}

	n = copy(p, s.rBuf[s.rOff:])
	s.rOff += n
	return n, nil
}

func (s *Stream) Write(p []byte) (n int, err error) {
	// 先检查 mux 是否已关闭，避免和 done channel 的竞态
	select {
	case <-s.done:
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
	default:
	}
	cp := make([]byte, len(p))
	copy(cp, p)
	select {
	case s.mux.writeCh <- writeMsg{streamID: s.id, data: cp}:
		return len(p), nil
	case <-s.done:
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
	}
}

// CloseWrite 发送写半关闭信号。
func (s *Stream) CloseWrite() error {
	select {
	case s.mux.writeCh <- writeMsg{streamID: s.id, data: nil}:
		return nil
	case <-s.done:
		return fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
	}
}

func (s *Stream) Close() error {
	select {
	case s.mux.writeCh <- writeMsg{streamID: s.id, data: closeMarker}:
		return nil
	case <-s.done:
		return fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
	}
}

// closeMarker 是 Close 的哨兵值。
var closeMarker = make([]byte, 0)

type writeMsg struct {
	streamID StreamID
	data     []byte // nil=CloseWrite, empty([]byte{})=Close
}

// Mux 在一条 xfer.Conn 上多路复用多条虚拟流。
type Mux struct {
	conn   xfer.Conn
	role   Role
	logger *slog.Logger

	mu       sync.Mutex
	streams  map[StreamID]*Stream
	nextID   StreamID
	acceptCh chan *Stream
	writeCh  chan writeMsg
	done     chan struct{}

	errOnce  sync.Once
	lastPong time.Time
}

// New 创建 Mux，启动事件循环 goroutine。
func New(conn xfer.Conn, role Role) *Mux {
	m := &Mux{
		conn:     conn,
		role:     role,
		logger:   slog.Default(),
		streams:  make(map[StreamID]*Stream),
		acceptCh: make(chan *Stream, 64),
		writeCh:  make(chan writeMsg, 256),
		done:     make(chan struct{}),
		lastPong: time.Now(),
	}
	if role == RoleDialer {
		m.nextID = 1
	}
	go m.readLoop()
	go m.writeLoop()
	go m.pingLoop()
	return m
}

func (m *Mux) Open(ctx context.Context) (*Stream, error) {
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		return nil, fmt.Errorf("mux: %w", xfer.ErrConnClosed)
	}
	id := m.nextID
	m.nextID += 2
	s := newStream(id, m)
	m.streams[id] = s
	m.mu.Unlock()

	frame := EncodeFrame(id, FrameOpen, nil)
	if err := m.conn.Send(ctx, frame); err != nil {
		m.mu.Lock()
		delete(m.streams, id)
		m.mu.Unlock()
		return nil, fmt.Errorf("mux: send open: %w", err)
	}
	return s, nil
}

func (m *Mux) Accept(ctx context.Context) (*Stream, error) {
	select {
	case s := <-m.acceptCh:
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return nil, fmt.Errorf("mux: %w", xfer.ErrConnClosed)
	}
}

func (m *Mux) Close() error {
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		return nil
	}
	close(m.done)
	for id, s := range m.streams {
		close(s.dataCh)
		close(s.done)
		delete(m.streams, id)
	}
	m.mu.Unlock()
	return m.conn.Close()
}

func (m *Mux) isClosed() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

// removeStream 从流表中移除流并清理。
func (m *Mux) removeStream(id StreamID, closeChannels bool) {
	m.mu.Lock()
	s, ok := m.streams[id]
	if ok {
		delete(m.streams, id)
	}
	m.mu.Unlock()
	if ok && closeChannels {
		close(s.dataCh)
		close(s.done)
	}
}

// ─── 读取循环 ────────────────────────────────────────

func (m *Mux) readLoop() {
	for {
		msg, err := m.conn.Receive(context.Background())
		if err != nil {
			m.errOnce.Do(func() { m.logger.Error("mux: recv error", "err", err) })
			m.Close()
			return
		}
		m.handleFrame(msg)
	}
}

// ─── 写入循环 ────────────────────────────────────────

func (m *Mux) writeLoop() {
	for {
		select {
		case msg := <-m.writeCh:
			m.sendFrame(msg)
		case <-m.done:
			return
		}
	}
}

func (m *Mux) sendFrame(msg writeMsg) {
	var frame []byte
	switch {
	case msg.data == nil:
		frame = EncodeFrame(msg.streamID, FrameCloseWrite, nil)
	case len(msg.data) == 0:
		frame = EncodeFrame(msg.streamID, FrameClose, nil)
		m.removeStream(msg.streamID, true)
	default:
		frame = EncodeFrame(msg.streamID, FrameData, msg.data)
	}
	if err := m.conn.Send(context.Background(), frame); err != nil {
		m.errOnce.Do(func() { m.logger.Error("mux: send error", "err", err) })
		m.Close()
	}
}

// ─── 心跳循环 ───────────────────────────────────────

func (m *Mux) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			frame := EncodeFrame(0, FramePing, nil)
			if err := m.conn.Send(context.Background(), frame); err != nil {
				return
			}
			if time.Since(m.lastPong) > 90*time.Second {
				m.Close()
				return
			}
		}
	}
}

// ─── 帧处理 ─────────────────────────────────────────

func (m *Mux) handleFrame(raw []byte) {
	sid, ftype, payload, err := DecodeFrame(raw)
	if err != nil {
		m.logger.Warn("mux: invalid frame", "err", err)
		return
	}
	switch ftype {
	case FrameData:
		m.mu.Lock()
		s, ok := m.streams[sid]
		m.mu.Unlock()
		if !ok {
			return
		}
		select {
		case s.dataCh <- payload:
		case <-s.done:
		}
	case FrameOpen:
		m.mu.Lock()
		if _, exists := m.streams[sid]; exists {
			m.mu.Unlock()
			return
		}
		s := newStream(sid, m)
		m.streams[sid] = s
		m.mu.Unlock()
		select {
		case m.acceptCh <- s:
		default:
			m.removeStream(sid, true)
		}
	case FrameClose:
		m.removeStream(sid, true)
	case FrameCloseWrite:
		m.mu.Lock()
		s, ok := m.streams[sid]
		m.mu.Unlock()
		if !ok {
			return
		}
		select {
		case s.dataCh <- nil: // nil 表示写半关闭
		case <-s.done:
		}
	case FramePing:
		_ = m.conn.Send(context.Background(), EncodeFrame(0, FramePong, nil))
	case FramePong:
		m.lastPong = time.Now()
	}
}

// Context 返回一个上下文，当 mux 关闭时取消。
func (m *Mux) Context() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-m.done
		cancel()
	}()
	return ctx
}

// Errors
var ErrMuxClosed = errors.New("mux: closed")
