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
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// DefaultWindowSize 是每条流的初始发送窗口大小。
const DefaultWindowSize = 65536 // 64 KB

// maxRetries 是帧重传的最大次数。
const maxRetries = 5

// retryBaseDelay 是重传的基础延迟。
const retryBaseDelay = 100 * time.Millisecond

// retryMaxDelay 是重传的最大延迟。
const retryMaxDelay = 3 * time.Second

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

	ctxOnce   sync.Once
	ctx       context.Context // NOSONAR S8242 - mux 生命周期 context, 非请求级, sync.Once 懒初始化
	ctxCancel context.CancelFunc

	retransmitMu sync.Mutex
	retransmitQ  []retransmitEntry
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
	m.Context()
	return m
}

// Metrics 返回指向 mux 统计信息的指针。
func (m *Mux) Metrics() *Metrics { return &m.metrics }

// Done 返回一个 channel，当 mux 关闭时关闭（用于测试）。
func (m *Mux) Done() <-chan struct{} {
	return m.done
}

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
	if m.ctxCancel != nil {
		m.ctxCancel()
	}
	for id, s := range m.streams {
		delete(m.streams, id)
		s.closeChannels()
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

func (m *Mux) Context() context.Context {
	m.ctxOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		m.ctx = ctx
		m.ctxCancel = cancel
		go func() {
			<-m.done
			cancel()
		}()
	})
	return m.ctx
}
