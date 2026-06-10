// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// Role 标识 Mux 的角色。
type Role int

const (
	RoleDialer   Role = iota // 主动拨号端（sclient），分配奇数 StreamID
	RoleListener             // 监听接受端（sproxy Hub），分配偶数 StreamID
)

// Stream 代表一条虚拟流。
type Stream struct {
	id     StreamID
	mux    *Mux
	rBuf   []byte
	rOff   int
	rMu    sync.Mutex
	dataCh chan []byte
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
	data := make([]byte, len(p))
	copy(data, p)
	select {
	case s.mux.writeCh <- writeReq{streamID: s.id, data: data}:
		return len(p), nil
	case <-s.done:
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
	}
}

func (s *Stream) Close() error {
	return s.mux.closeStream(s.id)
}

type writeReq struct {
	streamID StreamID
	data     []byte
}

// Mux 在一条 xfer.Conn 上多路复用多条虚拟流。
type Mux struct {
	conn     xfer.Conn
	role     Role
	logger   *slog.Logger

	mu       sync.Mutex
	streams  map[StreamID]*Stream
	nextID   StreamID
	acceptCh chan *Stream
	writeCh  chan writeReq
	done     chan struct{}
	closed   bool

	errOnce sync.Once
	loopErr error

	lastPong time.Time
}

// New 创建 Mux，启动事件循环 goroutine。
func New(conn xfer.Conn, role Role) *Mux {
	m := &Mux{
		conn:     conn,
		role:     role,
		logger:   slog.Default(),
		streams:  make(map[StreamID]*Stream),
		nextID:   0,
		acceptCh: make(chan *Stream, 64),
		writeCh:  make(chan writeReq, 256),
		done:     make(chan struct{}),
		lastPong: time.Now(),
	}
	if role == RoleDialer {
		m.nextID = 1
	}
	go m.loop()
	return m
}

// Open 创建一条新流（Dialer 端使用）。
func (m *Mux) Open(ctx context.Context) (*Stream, error) {
	m.mu.Lock()
	if m.closed {
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

	m.logger.Debug("mux: stream opened", "streamID", id)
	return s, nil
}

// Accept 阻塞等待接受一条新流（Listener 端使用）。
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

// Close 关闭所有流和底层连接。
func (m *Mux) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	close(m.done)
	for id, s := range m.streams {
		close(s.dataCh)
		close(s.done)
		delete(m.streams, id)
	}
	m.mu.Unlock()
	return m.conn.Close()
}

func (m *Mux) closeStream(id StreamID) error {
	m.mu.Lock()
	s, ok := m.streams[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.streams, id)
	close(s.dataCh)
	close(s.done)
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame := EncodeFrame(id, FrameClose, nil)
	return m.conn.Send(ctx, frame)
}

// 事件循环 goroutine。
//
// 职责：
//   - 从底层 conn 读取帧并分发
//   - 从 writeCh 取写入请求并发送帧
//   - 定期心跳
func (m *Mux) loop() {
	pingTick := time.NewTicker(30 * time.Second)
	defer pingTick.Stop()

	// recvDone 用于协调读写 goroutine
	recvDone := make(chan struct{})
	defer close(recvDone)

	// 写 goroutine
	go func() {
		for {
			select {
			case req := <-m.writeCh:
				frame := EncodeFrame(req.streamID, FrameData, req.data)
				if err := m.conn.Send(context.Background(), frame); err != nil {
					m.errOnce.Do(func() { m.loopErr = fmt.Errorf("mux: send: %w", err) })
					m.Close()
					return
				}
			case <-m.done:
				return
			case <-recvDone:
				return
			}
		}
	}()

	// 读 + 心跳 goroutine（主循环）
	for {
		select {
		case <-m.done:
			return
		case <-pingTick.C:
			m.sendPing()
			if time.Since(m.lastPong) > 90*time.Second {
				m.logger.Warn("mux: heartbeat timeout")
				m.errOnce.Do(func() { m.loopErr = errors.New("mux: heartbeat timeout") })
				m.Close()
				return
			}
		default:
		}

		msg, err := m.conn.Receive(context.Background())
		if err != nil {
			m.errOnce.Do(func() { m.loopErr = fmt.Errorf("mux: recv: %w", err) })
			m.Close()
			return
		}
		m.handleFrame(msg)
	}
}

func (m *Mux) handleFrame(raw []byte) {
	sid, ftype, payload, err := DecodeFrame(raw)
	if err != nil {
		m.logger.Warn("mux: invalid frame", "err", err)
		return
	}

	switch ftype {
	case FrameData:
		m.dispatchData(sid, payload)
	case FrameOpen:
		m.handleOpen(sid)
	case FrameClose:
		m.handleRemoteClose(sid)
	case FramePing:
		m.sendPong()
	case FramePong:
		m.lastPong = time.Now()
	}
}

func (m *Mux) dispatchData(sid StreamID, payload []byte) {
	m.mu.Lock()
	s, ok := m.streams[sid]
	m.mu.Unlock()
	if !ok {
		m.logger.Debug("mux: data for unknown stream", "streamID", sid)
		return
	}
	select {
	case s.dataCh <- payload:
	case <-s.done:
	}
}

func (m *Mux) handleOpen(sid StreamID) {
	m.mu.Lock()
	if _, exists := m.streams[sid]; exists {
		m.mu.Unlock()
		m.logger.Debug("mux: duplicate stream", "streamID", sid)
		return
	}
	s := newStream(sid, m)
	m.streams[sid] = s
	if sid >= m.nextID {
		m.nextID = sid + 2
	}
	m.mu.Unlock()

	select {
	case m.acceptCh <- s:
	default:
		m.logger.Warn("mux: accept channel full")
		m.mu.Lock()
		delete(m.streams, sid)
		m.mu.Unlock()
	}
}

func (m *Mux) handleRemoteClose(sid StreamID) {
	m.mu.Lock()
	s, ok := m.streams[sid]
	if ok {
		delete(m.streams, sid)
		close(s.dataCh)
		close(s.done)
	}
	m.mu.Unlock()
}

func (m *Mux) sendPing() {
	frame := EncodeFrame(0, FramePing, nil)
	m.conn.Send(context.Background(), frame)
}

func (m *Mux) sendPong() {
	frame := EncodeFrame(0, FramePong, nil)
	m.conn.Send(context.Background(), frame)
}

// Context 返回一个取消上下文，当 mux 关闭时取消。
func (m *Mux) Context() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-m.done
		cancel()
	}()
	return ctx
}
