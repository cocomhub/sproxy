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

// maxRecvRetries 是读取循环遇到临时错误时的最大重试次数。
const maxRecvRetries = 5

// errFmtMuxStreamErr 是流相关错误的格式化字符串。
const errFmtMuxStreamErr = "mux: stream %d: %w"

// errFmtMuxClosed 是 mux 关闭错误的格式化字符串。
const errFmtMuxClosed = "mux: %w"

// Sentinel errors.
var (
	ErrStreamRejected = errors.New("mux: stream rejected")
	ErrMaxStreams     = errors.New("mux: max streams reached")
)

// Role 标识 Mux 的角色。
type Role int

const (
	RoleDialer Role = iota
	RoleListener
)

// Stream 是虚拟流接口，实现 io.ReadWriteCloser 并支持半关闭。
type Stream interface {
	io.ReadWriteCloser
	ID() StreamID
	CloseWrite() error
}

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
	Streams               StreamMetrics
	PingsSent             atomic.Int64
	PongsReceived         atomic.Int64
	FramesReceived        atomic.Int64
	FramesSent            atomic.Int64
	Errors                atomic.Int64
	StreamsRejected       atomic.Int64 // 因 acceptCh 满或 maxStreams 限制被拒绝的流数
	RecvRetries           atomic.Int64 // 读取循环重试次数
	StreamsRejectedAccCh  atomic.Int64 // 因 acceptCh 满被拒绝的流数
	StreamsRejectedMaxStr atomic.Int64 // 因 maxStreams 被拒绝的流数
}

// Option 配置 Mux 的函数选项。
type Option func(*Mux)

// WithMaxStreams 设置最大并发流数。
func WithMaxStreams(n int) Option {
	return func(m *Mux) {
		m.maxStreams = int32(n)
	}
}

// WithAcceptChSize 设置 acceptCh 缓冲区大小，默认 64。
func WithAcceptChSize(n int) Option {
	return func(m *Mux) {
		m.acceptCh = make(chan Stream, n)
	}
}

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

// Mux 在一条 xfer.Conn 上多路复用多条虚拟流。
type Mux struct {
	conn    xfer.Conn
	role    Role
	logger  *slog.Logger
	metrics Metrics

	mu      sync.Mutex
	streams map[StreamID]*stream
	nextID  StreamID

	acceptCh chan Stream
	writeCh  chan writeMsg
	done     chan struct{}

	activeStreams atomic.Int32
	maxStreams    int32

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
		streams:  make(map[StreamID]*stream),
		acceptCh: make(chan Stream, 64),
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
func (m *Mux) Metrics() *Metrics { return &m.metrics }

// Open 创建一条新流。
func (m *Mux) Open(ctx context.Context) (Stream, error) {
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		m.metrics.Streams.Errors.Add(1)
		return nil, fmt.Errorf(errFmtMuxClosed, xfer.ErrConnClosed)
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

// Accept 等待并返回一条新流。
func (m *Mux) Accept(ctx context.Context) (Stream, error) {
	select {
	case s := <-m.acceptCh:
		return s, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.done:
		return nil, fmt.Errorf(errFmtMuxClosed, xfer.ErrConnClosed)
	}
}

// Close 关闭 mux 和所有流。
func (m *Mux) Close() error {
	m.mu.Lock()
	if m.isClosed() {
		m.mu.Unlock()
		return nil
	}
	close(m.done)
	for id, s := range m.streams {
		delete(m.streams, id)
		s.closeChannels()
	}
	m.mu.Unlock()
	return m.conn.Close()
}

// sendWindowUpdateUnsafe 发送窗口更新帧。
// 必须在 readLoop goroutine 中或已持有 m.mu 锁时调用。
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

func (m *Mux) removeStream(id StreamID, closeCh bool) {
	m.mu.Lock()
	s, ok := m.streams[id]
	if ok {
		delete(m.streams, id)
	}
	m.mu.Unlock()
	if ok && closeCh {
		m.activeStreams.Add(-1)
		s.closeChannels()
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
	}
}

func (m *Mux) sendFrame(msg writeMsg) {
	m.metrics.FramesSent.Add(1)
	if msg.isRaw {
		if err := m.conn.Send(m.Context(), msg.data); err != nil {
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
	default:
		frame = EncodeFrame(msg.streamID, FrameData, msg.data)
	}

	if len(msg.data) > 0 {
		for i := range maxRetries {
			if err := m.conn.Send(m.Context(), frame); err == nil {
				return
			}
			time.Sleep(retryBaseDelay << i)
		}
		m.metrics.Errors.Add(1)
		m.logger.Error("mux: send error, retries exhausted", "stream", msg.streamID)
		m.Close()
		return
	}

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
	retries := 0
	for {
		raw, err := m.conn.Receive(m.Context())
		if err != nil {
			if errors.Is(err, xfer.ErrConnClosed) || retries >= maxRecvRetries {
				m.metrics.Errors.Add(1)
				m.logger.Error("mux: recv error, closing", "err", err, "retries", retries)
				m.Close()
				return
			}
			retries++
			m.metrics.RecvRetries.Add(1)
			m.logger.Warn("mux: recv transient error, retrying", "err", err, "retries", retries)
			backoff := retryBaseDelay << min(retries-1, 3)
			select {
			case <-m.done:
				return
			case <-m.ctx.Done():
				m.Close()
				return
			case <-time.After(backoff):
			}
			continue
		}
		retries = 0
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
			if err := m.conn.Send(m.Context(), frame); err != nil {
				m.metrics.Errors.Add(1)
				m.logger.Error("mux: ping send error, closing", "err", err)
				m.Close()
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
