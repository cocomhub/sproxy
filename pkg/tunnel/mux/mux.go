// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// DefaultWindowSize 是每条流的初始发送窗口大小。
const DefaultWindowSize = 65536 // 64 KB

// maxRetries 是帧重传的最大次数。
const maxRetries = 3

// retryBaseDelay 是重传的基础延迟。
const retryBaseDelay = 100 * time.Millisecond

// Sentinel errors.
var (
	ErrStreamRejected = errors.New("mux: stream rejected") // acceptCh 满或超出 maxStreams
	ErrMaxStreams     = errors.New("mux: max streams reached")
)

// Role 标识 Mux 的角色。
type Role int

const (
	RoleDialer Role = iota
	RoleListener
)

// StreamMetrics 收集流的统计信息。
type StreamMetrics struct {
	Opened       atomic.Int64
	Closed       atomic.Int64
	BytesRead    atomic.Int64
	BytesWritten atomic.Int64
	Errors       atomic.Int64
}

// Metrics 收集 mux 级别的统计信息。
type Metrics struct {
	Streams         StreamMetrics
	PingsSent       atomic.Int64
	PongsReceived   atomic.Int64
	FramesReceived  atomic.Int64
	FramesSent      atomic.Int64
	Errors          atomic.Int64
	StreamsRejected atomic.Int64 // 因 acceptCh 满或 maxStreams 限制被拒绝的流数
}

// Option 配置 Mux 的函数选项。
type Option func(*Mux)

// WithMaxStreams 设置最大并发流数。
// 当活跃流数达到此上限时，后续的 Open 会被拒绝。
// 默认值 0 表示不限制。
func WithMaxStreams(n int) Option {
	return func(m *Mux) {
		m.maxStreams = int32(n)
	}
}

// Stream 代表一条虚拟流，实现 io.ReadWriteCloser。
type Stream struct {
	id   StreamID
	mux  *Mux
	rBuf []byte
	rOff int
	rMu  sync.Mutex

	dataCh chan []byte
	done   chan struct{}

	// 流控：窗口计数器
	windowSize     atomic.Int32  // 剩余可发送字节数
	windowUpdateCh chan struct{} // 窗口补充时唤醒 Write

	// rejected 标记服务端拒绝了此流（acceptCh 满或 maxStreams 限制）。
	rejected atomic.Bool
}

func newStream(id StreamID, m *Mux) *Stream {
	s := &Stream{
		id:     id,
		mux:    m,
		dataCh: make(chan []byte, 64),
		done:   make(chan struct{}),
	}
	s.windowSize.Store(DefaultWindowSize)
	s.windowUpdateCh = make(chan struct{}, 8)
	return s
}

func (s *Stream) ID() StreamID { return s.id }

func (s *Stream) Read(p []byte) (n int, err error) {
	s.rMu.Lock()
	defer s.rMu.Unlock()

	for s.rOff >= len(s.rBuf) {
		// 优先检查是否是拒绝流 — 返回明确的哨兵错误。
		if s.rejected.Load() {
			return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
		}

		select {
		case data, ok := <-s.dataCh:
			if !ok {
				if s.rejected.Load() {
					return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
				}
				return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
			}
			if data == nil {
				return 0, io.EOF
			}
			s.rBuf = data
			s.rOff = 0

			// 流控：消费数据后发送 WindowUpdate
			s.mux.sendWindowUpdateUnsafe(s.id, int32(len(data)))
		case <-s.done:
			if s.rejected.Load() {
				return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
			}
			return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
		}
	}

	n = copy(p, s.rBuf[s.rOff:])
	s.rOff += n
	s.mux.metrics.Streams.BytesRead.Add(int64(n))
	return n, nil
}

func (s *Stream) Write(p []byte) (n int, err error) {
	if s.rejected.Load() {
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
	}

	select {
	case <-s.done:
		if s.rejected.Load() {
			return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
		}
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
	default:
	}

	// 流控：等待窗口可用
	for s.windowSize.Load() <= 0 {
		select {
		case <-s.windowUpdateCh:
		case <-s.done:
			if s.rejected.Load() {
				return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
			}
			return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
		}
	}

	// 不超过窗口大小
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
		if s.rejected.Load() {
			return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
		}
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, xfer.ErrConnClosed)
	}
}

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

var closeMarker = make([]byte, 0)

type writeMsg struct {
	streamID StreamID
	data     []byte // nil=CloseWrite, empty([]byte{})=Close
	isRaw    bool   // true=已经是编码好的完整帧，直接发送
}

// Mux 在一条 xfer.Conn 上多路复用多条虚拟流。
type Mux struct {
	conn    xfer.Conn
	role    Role
	logger  *slog.Logger
	metrics Metrics

	mu       sync.Mutex
	streams  map[StreamID]*Stream
	nextID   StreamID
	acceptCh chan *Stream
	writeCh  chan writeMsg
	done     chan struct{}

	// activeStreams 当前活跃流数，跨 removeStream / Open / handleFrame 无锁原子访问。
	activeStreams atomic.Int32

	// maxStreams 最大并发流数，0 表示不限制。
	maxStreams int32

	errOnce      sync.Once
	lastPongNano atomic.Int64

	ctx     context.Context
	ctxOnce sync.Once
}

// New 创建 Mux，启动事件循环 goroutine。
func New(conn xfer.Conn, role Role) *Mux {
	return NewWithOpts(conn, role)
}

// NewWithOpts 创建 Mux 并应用选项。
func NewWithOpts(conn xfer.Conn, role Role, opts ...Option) *Mux {
	m := &Mux{
		conn:     conn,
		role:     role,
		logger:   slog.Default(),
		streams:  make(map[StreamID]*Stream),
		acceptCh: make(chan *Stream, 64),
		writeCh:  make(chan writeMsg, 256),
		done:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(m)
	}
	m.metrics = Metrics{}
	m.lastPongNano.Store(time.Now().UnixNano())
	if role == RoleDialer {
		m.nextID = 1
	}
	go m.readLoop()
	go m.writeLoop()
	go m.pingLoop()
	return m
}

// Metrics 返回指向 mux 统计信息的指针。
func (m *Mux) Metrics() *Metrics {
	return &m.metrics
}

func (m *Mux) Open(ctx context.Context) (*Stream, error) {
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		m.metrics.Streams.Errors.Add(1)
		return nil, fmt.Errorf("mux: %w", xfer.ErrConnClosed)
	}
	if m.maxStreams > 0 && m.activeStreams.Load() >= m.maxStreams {
		m.mu.Unlock()
		m.metrics.Streams.Errors.Add(1)
		return nil, ErrMaxStreams
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
		m.metrics.Streams.Errors.Add(1)
		return nil, fmt.Errorf("mux: send open: %w", err)
	}
	m.activeStreams.Add(1)
	m.metrics.Streams.Opened.Add(1)
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
		delete(m.streams, id)
		close(s.dataCh)
		close(s.done)
	}
	m.mu.Unlock()
	return m.conn.Close()
}

// sendWindowUpdateUnsafe 发送窗口更新帧。
// 必须在 readLoop goroutine 中或已持有 m.mu 锁时调用，否则可能 dataCh 被关闭后仍写入。
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

func (m *Mux) isClosed() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

func (m *Mux) removeStream(id StreamID, closeChannels bool) {
	m.mu.Lock()
	s, ok := m.streams[id]
	if ok {
		delete(m.streams, id)
	}
	m.mu.Unlock()
	if ok && closeChannels {
		m.activeStreams.Add(-1)
		close(s.dataCh)
		close(s.done)
	}
}

// rejectStream 向 dialer 发送 FrameReject 拒绝流的创建请求。
// acceptChFull 为 true 表示因 acceptCh 满而拒绝，false 表示因 maxStreams 限制。
// 调用来自 handleFrame（readLoop goroutine），writeCh 满时静默丢弃拒绝帧
// 以防止 readLoop 阻塞影响后续帧处理。调用方已清理流，丢弃拒绝帧是安全的。
func (m *Mux) rejectStream(sid StreamID, acceptChFull bool) {
	var reason byte = 0x01
	if !acceptChFull {
		reason = 0x02
	}
	frame := EncodeFrame(sid, FrameReject, []byte{reason})
	select {
	case m.writeCh <- writeMsg{streamID: sid, data: frame, isRaw: true}:
	default:
		// writeCh 满：静默丢弃，调用方已通过 removeStream 清理
	}
}

func (m *Mux) sendFrame(msg writeMsg) {
	m.metrics.FramesSent.Add(1)
	if msg.isRaw {
		if err := m.conn.Send(context.Background(), msg.data); err != nil {
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
		m.removeStream(msg.streamID, true)
	default:
		frame = EncodeFrame(msg.streamID, FrameData, msg.data)
	}

	if len(msg.data) > 0 {
		// 数据帧重传：指数退避 100ms → 200ms → 400ms
		for i := 0; i < maxRetries; i++ {
			if err := m.conn.Send(context.Background(), frame); err == nil {
				return
			}
			time.Sleep(retryBaseDelay << i)
		}
		m.metrics.Errors.Add(1)
		m.logger.Error("mux: send error, retries exhausted", "stream", msg.streamID)
		m.Close()
		return
	}

	// 非数据帧（控制帧）直接发送，失败即关闭连接
	if err := m.conn.Send(context.Background(), frame); err != nil {
		m.metrics.Errors.Add(1)
		m.logger.Error("mux: send error, closing mux", "stream", msg.streamID, "err", err)
		m.Close()
	}
}

func (m *Mux) writeLoop() {
	for {
		select {
		case <-m.done:
			return
		case msg := <-m.writeCh:
			m.sendFrame(msg)
		}
	}
}

func (m *Mux) readLoop() {
	for {
		raw, err := m.conn.Receive(m.Context())
		if err != nil {
			// readLoop 退出时触发 close（幂等）
			m.metrics.Errors.Add(1)
			m.logger.Error("mux: recv error", "err", err)
			m.Close()
			return
		}
		m.handleFrame(raw)
	}
}

func (m *Mux) pingLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
			frame := EncodeFrame(0, FramePing, nil)
			m.metrics.PingsSent.Add(1)
			if err := m.conn.Send(context.Background(), frame); err != nil {
				m.metrics.Errors.Add(1)
				return
			}
			lastPong := time.Unix(0, m.lastPongNano.Load())
			if time.Since(lastPong) > 90*time.Second {
				m.metrics.Errors.Add(1)
				m.logger.Warn("mux: heartbeat timeout, closing")
				m.Close()
				return
			}
		}
	}
}

func (m *Mux) handleFrame(raw []byte) {
	m.metrics.FramesReceived.Add(1)
	sid, ftype, payload, err := DecodeFrame(raw)
	if err != nil {
		m.metrics.Errors.Add(1)
		m.logger.Warn("mux: invalid frame", "err", err)
		return
	}

	switch ftype {
	case FrameData:
		m.mu.Lock()
		s, ok := m.streams[sid]
		if !ok {
			m.mu.Unlock()
			return
		}
		select {
		case s.dataCh <- payload:
		case <-s.done:
		}
		m.mu.Unlock()
	case FrameOpen:
		m.mu.Lock()
		if _, exists := m.streams[sid]; exists {
			m.mu.Unlock()
			return
		}
		if m.maxStreams > 0 && m.activeStreams.Load() >= m.maxStreams {
			m.mu.Unlock()
			m.rejectStream(sid, false)
			m.metrics.StreamsRejected.Add(1)
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
			m.removeStream(sid, true)
			m.metrics.StreamsRejected.Add(1)
		}
	case FrameReject:
		m.mu.Lock()
		s, ok := m.streams[sid]
		if ok {
			s.rejected.Store(true)
			delete(m.streams, sid)
		}
		m.mu.Unlock()
		if ok {
			m.activeStreams.Add(-1)
			close(s.dataCh)
			close(s.done)
		}
		m.metrics.StreamsRejected.Add(1)
	case FrameClose:
		m.removeStream(sid, true)
		m.metrics.Streams.Closed.Add(1)
	case FrameCloseWrite:
		m.mu.Lock()
		s, ok := m.streams[sid]
		if !ok {
			m.mu.Unlock()
			return
		}
		select {
		case s.dataCh <- nil:
		case <-s.done:
		}
		m.mu.Unlock()
	case FramePing:
		_ = m.conn.Send(context.Background(), EncodeFrame(0, FramePong, nil))
	case FramePong:
		m.lastPongNano.Store(time.Now().UnixNano())
		m.metrics.PongsReceived.Add(1)
	case FrameWindowUpdate:
		m.mu.Lock()
		s, ok := m.streams[sid]
		m.mu.Unlock()
		if !ok {
			return
		}
		// 窗口更新：补充额度
		s.windowSize.Add(int32(binary.BigEndian.Uint32(payload)))
		select {
		case s.windowUpdateCh <- struct{}{}:
		default:
		}
	default:
		m.metrics.Errors.Add(1)
		m.logger.Warn("mux: unknown frame type", "type", ftype)
	}
}

func (m *Mux) Context() context.Context {
	m.ctxOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		m.ctx = ctx
		go func() {
			<-m.done
			cancel()
		}()
	})
	return m.ctx
}

var ErrMuxClosed = errors.New("mux: closed")
