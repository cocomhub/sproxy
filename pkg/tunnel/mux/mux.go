// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
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
	ErrMuxClosed      = errors.New("mux: closed")
	// ErrStreamIDExhausted 表示 StreamID 空间已耗尽（F7 回绕防护）：nextID 越过
	// 可用上限（uint32 回绕会撞上控制流专用 ID 0 或复用仍存活流的旧 ID），
	// Open fail-closed 拒绝而非产出错位流。理论触发：单条 mux 上累计打开
	// 超过 2^31 条流（长命 hub 中继/relay_stream 场景才可能逼近）。
	ErrStreamIDExhausted = errors.New("mux: stream id space exhausted")
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
	// Abort 立即放弃该流（非阻塞、幂等）：关闭本地 done 以解除 Read/Write 阻塞，
	// 并从流表**注销**（移除表项 + 递减 activeStreams，释放 maxStreams 额度），
	// 不经 writeCh 向对端发送关闭帧。与 Close 的区别见 stream.Abort 文档。
	// 收尾/超时强制释放场景应优先 Abort，避免 writeCh 打满时 Close 永久阻塞。
	Abort() error
}

// StreamMetrics 收集流的统计信息。
type StreamMetrics struct {
	Opened       atomic.Int64
	Closed       atomic.Int64
	BytesRead    atomic.Int64
	BytesWritten atomic.Int64
	Errors       atomic.Int64
	// Active 是**当前活跃流数**（观测用）：Open（含 acceptCh 入队）时 +1，removeStream /
	// removeStreamIf 实际注销时 -1。与 Mux.activeStreams（maxStreams 限额判据）同步增减，
	// 但语义独立——本字段让「acceptor 侧从不主动 Close ⇒ 对端失联时流表滞留」可被观测
	// （审计 F6 的修法②：只加可观测性，不做本地超时/自动回收）。
	Active atomic.Int64
	// MaxActive 是 Active 的峰值（CAS 更新），跨重启/聚合时反映历史上同时活跃的最大流数。
	MaxActive atomic.Int64
}

// BlockStat 记录 readLoop 内某条「可能阻塞路径」的进入次数与耗时（纳秒）。
//
// 动机（2026-09-16 审计 F2）：readLoop 是**单 goroutine 串行**处理所有帧（见 loop.go），
// 它内部任何同步阻塞都会停摆整条连接（该连接上所有流）；停摆还会让本侧收不到对端 Pong，
// 被本侧 pingLoop 以 90s 心跳超时把连接拆掉。本结构把「这三条路径到底有没有真的阻塞、
// 阻塞多久」变成可观测事实——**修复后的 datagram/pong 应≈0**，而 push 等待反映接收侧背压
// （对端超窗口灌数据时才会非零），作为后续 readLoop 头阻塞治理取舍的证据基线。
//
// **不得拷贝**（内含原子；与同包 Metrics 一样只经指针传递）。
type BlockStat struct {
	Waits    atomic.Int64 // 进入该路径的次数（进入时即 +1，阻塞中也能被观测到）
	Nanos    atomic.Int64 // 累计耗时（纳秒）
	MaxNanos atomic.Int64 // 单次最长耗时（纳秒）
}

// enter 记账「进入了这条可能阻塞路径」并返回进入时刻。
//
// 分 enter/leave 两步（而非结束后一次性记账）是为了让**正在阻塞中**的情况也可观测：
// 若等阻塞结束才 +1，则「连接停摆中」这段时间恰恰看不到任何进入次数，与观测目的相反。
func (b *BlockStat) enter() time.Time {
	b.Waits.Add(1)
	return time.Now()
}

// leave 记账本次耗时（配合 enter；负值按 0 计，time.Since 理论上不会为负）。
func (b *BlockStat) leave(start time.Time) {
	n := max(time.Since(start).Nanoseconds(), 0)
	b.Nanos.Add(n)
	for {
		cur := b.MaxNanos.Load()
		if n <= cur || b.MaxNanos.CompareAndSwap(cur, n) {
			return
		}
	}
}

// MergeFrom 把 o 并入 b：次数/累计耗时求和，单次峰值取两者最大。
// 供 /metrics 的多 mux 聚合使用（聚合语义与单 mux 一致）；导出是因为聚合发生在 pkg/server。
func (b *BlockStat) MergeFrom(o *BlockStat) {
	b.Waits.Add(o.Waits.Load())
	b.Nanos.Add(o.Nanos.Load())
	for {
		cur := b.MaxNanos.Load()
		n := o.MaxNanos.Load()
		if n <= cur || b.MaxNanos.CompareAndSwap(cur, n) {
			return
		}
	}
}

// Metrics 收集 mux 级别的统计信息。
type Metrics struct {
	Streams               StreamMetrics
	PingsSent             atomic.Int64
	PaddingSent           atomic.Int64 // 空闲填充帧发送数（roadmap §5.3 P1；默认关零回归）
	PaddingReceived       atomic.Int64 // 空闲填充帧接收数（对端忽略+计数）
	PongsReceived         atomic.Int64
	FramesReceived        atomic.Int64
	FramesSent            atomic.Int64
	Errors                atomic.Int64
	StreamsRejected       atomic.Int64 // 因 acceptCh 满或 maxStreams 限制被拒绝的流数
	RecvRetries           atomic.Int64 // 读取循环重试次数
	StreamsRejectedAccCh  atomic.Int64 // 因 acceptCh 满被拒绝的流数
	StreamsRejectedMaxStr atomic.Int64 // 因 maxStreams 被拒绝的流数

	// PongsSent 是本侧回复的 Pong 帧数；PongsCoalesced 是因 writeCh 满而**被合并**的 Pong 次数
	// （Pong 幂等，多次 Ping 只需回一次；见 handlePingFrame/flushPendingPong）。
	// PongsDropped 是 Pong **出线失败**而被丢弃的次数：这类丢失幂等可自愈（下一轮 Ping 再回一次，
	// 对端 90s 心跳窗口足够），故单列而不计入 Errors —— `sproxy_mux_errors` 是告警信号，
	// 把可自愈的 Pong 丢失计进去会虚增告警。
	PongsSent      atomic.Int64
	PongsCoalesced atomic.Int64
	PongsDropped   atomic.Int64

	// DatagramHandlerDrops 是数据报 handler **自行上报**的丢弃数（handler 侧并发写信号量饱和，
	// 如 relay 的 UDP 出口；UDP 语义下丢包）。mux 自身不产生该丢弃：为 0 只说明没有 handler
	// 报过这类丢弃（或未注册 handler）。之所以放在此处，是为了让「丢包」在 /metrics 上可见
	// （handler 侧没有独立的指标出口）。
	DatagramHandlerDrops atomic.Int64

	// readLoop 内三条同步路径的耗时观测（F2 证据基线；见 BlockStat 文档）。术语提醒：
	// `push` 是**仍然存在**的阻塞路径（F2 未修，见 DataChMaxFrames 的量纲提醒）；
	// `datagram`/`pong` 经 2026-09-16 改造后只做非阻塞投递，耗时**应≈0**，非零即回归。
	ReadLoopPush     BlockStat // handleDataFrame → stream.pushData 的等待（dataCh 满时为非零）
	ReadLoopDatagram BlockStat // handleDatagramFrame 内**同步**调用注册 handler 的耗时（改造后应≈0）
	ReadLoopPong     BlockStat // handlePingFrame 内回复 Pong 的耗时（改造后只做非阻塞投递，应≈0）

	// DataChMaxFrames 是观测到的**单流** dataCh 最大占用帧数（容量见 newStream：64 帧）。
	// 量纲提醒：窗口按**字节**计（DefaultWindowSize=65536）而 dataCh 按**帧**计 ⇒ 对端在窗口内
	// 用小帧（平均 ≤1024 B）写时第 65 帧即触发溢出（2026-09-16 F2 修复前该处会**阻塞 readLoop**）。
	// 修复后该值恒 ≤ cap(dataCh)：溢出部分进溢出缓冲（见 stream.go），故「堆积深度」要看
	// MaxBufferedBytes（字节量纲）。
	DataChMaxFrames atomic.Int64

	// StreamOverflowSpills 是「dataCh 满 ⇒ 帧改入溢出缓冲」的次数（修复前这类事件会阻塞 readLoop）。
	// 非零不代表故障：守协议对端用小帧写满窗口时必然发生；持续增长说明应用 Read 跟不上。
	StreamOverflowSpills atomic.Int64

	// StreamWindowViolations 是【对端超出其应守窗口】而被 Abort 的流数（fail-closed）。
	// 判据见 stream.go 的 pendingOverflowLimit（持有量上界 = 窗口 + 一帧）。该指标是此类违约的
	// **唯一可观测出口**（旧行为是静默堆积或阻塞 readLoop，两者都不可观测）。
	StreamWindowViolations atomic.Int64

	// MaxBufferedBytes 是单流「已从对端收到、应用尚未消费」字节数的峰值（**字节量纲**）。
	// 它才是与流控窗口可比的量（DataChMaxFrames 是帧数量纲，修复后不再反映堆积深度）。
	MaxBufferedBytes atomic.Int64

	// LongestIdleNanos 是采样时当前活跃流的最久空闲时长（纳秒；0 = 无活跃流）。
	// 非原子维护：由 StreamStats() 经 m.mu 遍历流表实时计算（见 Mux.StreamStats），
	// 因此本字段只在聚合时写入（aggregateMuxMetrics 对每个 mux 调 LongestIdle() 取最大）。
	// 语义：acceptor 侧流从不主动 Close（审计 F6），对端失联时流表滞留 ⇒ 空闲时长持续
	// 增长，正是「疑似泄漏流」的哨兵。
	LongestIdleNanos atomic.Int64

	// Retransmits 是重传成功的次数（数据帧首次 Send 失败入队、退避后重发成功）。
	// 非零说明传输层出现过瞬时故障但已自愈；持续增长提示连接质量劣化。
	Retransmits atomic.Int64
	// RetransmitQueueFull 是重传队列满（maxRetransmitQ=256）而关闭 mux 的次数。
	// 队列满意味着已有大量帧 Send 失败，连接实际不可用（fail-closed 显式失败）。
	RetransmitQueueFull atomic.Int64
	// RetransmitExhausted 是重传重试耗尽（maxRetries 退避用尽）而关闭 mux 的次数。
	RetransmitExhausted atomic.Int64
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

// WithLogger 注入日志器（默认 slog.Default()）。
// 用途：长生命周期/高频收尾场景（benchmark 每轮开合 mux、中继频繁重连）默认会把
// 「对端先关连接」的正常收尾噪音打为 ERROR；注入 DiscardLogger 可静音，同时保留
// 真正异常路径的可见性（静音是调用方显式选择）。
func WithLogger(l *slog.Logger) Option {
	return func(m *Mux) {
		m.logger = l
	}
}

// WithIdlePadding 开启空闲填充（roadmap §5.3 P1 被动伪装层：DPI 难判断连接空闲）。
// interval 是填充帧发送周期（如 10s；<=0 视为关闭零回归）。与 30s 心跳 Ping 独立共存。
func WithIdlePadding(interval time.Duration) Option {
	return func(m *Mux) {
		if interval > 0 {
			m.paddingInterval = interval
		}
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

	// datagramHandler 处理 UDP 数据报帧（FrameDatagram）。readLoop 读、relay
	// SetDatagramHandler 写（运行期异步设置），用 RWMutex 保护（勿在热路径持有写锁）。
	datagramMu      sync.RWMutex
	datagramHandler DatagramHandler

	activeStreams atomic.Int32
	maxStreams    int32

	lastPongNano atomic.Int64

	// paddingInterval 是空闲填充周期（roadmap §5.3 P1 被动伪装层）；0 = 不发送（默认零回归）。
	// 开启后 paddingLoop 周期发送 FramePadding（与 pingLoop 30s 心跳独立共存）。
	paddingInterval time.Duration
	// pendingPong 表示有一笔 Pong 因 writeCh 满而**未能投递**，需由 writeLoop 的 ticker
	// 补送（与 stream.pendingWindowUpdate 同思路：不丢、不阻塞 readLoop、不产生无界 goroutine）。
	// Pong 幂等，故用单个布尔合并多次 Ping 的回复需求。
	pendingPong atomic.Bool

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
	if m.paddingInterval > 0 {
		go m.paddingLoop()
	}
	m.Context()
	return m
}

// Metrics 返回指向 mux 统计信息的指针。
func (m *Mux) Metrics() *Metrics { return &m.metrics }

// StreamStats 是流级可观测性的**采样快照**（审计 F6 修法②：只加可观测性，不做本地超时）。
// ActiveStreams 反映「acceptor 侧从不主动 Close ⇒ 对端失联时流表滞留」的当前状态；
// LongestIdle 是当前活跃流的最久空闲时长（持续增长 ⇒ 疑似泄漏流，运维可介入）。
type StreamStats struct {
	ActiveStreams int
	LongestIdle   time.Duration
	MaxActive     int64
}

// StreamStats 返回流级观测快照：经 m.mu 遍历流表，统计当前活跃流数与最久空闲时长。
//
// 为什么经 m.mu 而非原子字段：Active/MaxActive 是原子计数（见 StreamMetrics），但 LongestIdle
// 需要「每个流自己的 lastActivity」——各流在 Read/Write 热路径上原子更新自己的 lastActivity，
// 聚合时需要读全表（不能每个流各配一个原子且每次读全部）。代价是短持 m.mu（流表遍历），
// 调用频率低（/metrics 聚合），可接受。
func (m *Mux) StreamStats() StreamStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var stats StreamStats
	stats.ActiveStreams = len(m.streams)
	stats.MaxActive = m.metrics.Streams.MaxActive.Load()
	for _, s := range m.streams {
		if s == nil {
			continue
		}
		idle := now.Sub(time.Unix(0, s.lastActivity.Load()))
		if idle > stats.LongestIdle {
			stats.LongestIdle = idle
		}
	}
	return stats
}

// LongestIdle 返回当前活跃流的最久空闲时长（0 = 无活跃流）。
// 供聚合层（pkg/server/aggregateMuxMetrics）跨 mux 取最大。
func (m *Mux) LongestIdle() time.Duration { return m.StreamStats().LongestIdle }

// Role 返回 mux 的角色（RoleDialer 或 RoleListener）。
func (m *Mux) Role() Role { return m.role }

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
	// F7 回绕防护：nextID 越过可用上限（+2 后回绕到 0，或撞上仍存活的旧 ID）时
	// fail-closed 拒绝。触发条件：单条 mux 累计打开 ≥ 2^31 条流（长命中继才可能），
	// 但一旦发生，id=0 会与控制流（Ping/Pong/Datagram 用 EncodeFrame(0,...)）冲突、
	// 且旧流表项被新流覆盖 ⇒ 静默丢字节，宁可显式失败（与重传队列满同取舍）。
	if m.nextID >= math.MaxUint32-2 {
		m.mu.Unlock()
		m.metrics.Streams.Errors.Add(1)
		return nil, ErrStreamIDExhausted
	}
	id := m.nextID
	m.nextID += 2
	s := newStream(id, m)
	m.streams[id] = s
	m.mu.Unlock()

	frame, encErr := EncodeFrame(id, FrameOpen, nil)
	if encErr != nil { // 不可达：负载为 nil
		m.mu.Lock()
		delete(m.streams, id)
		m.mu.Unlock()
		m.metrics.Streams.Errors.Add(1)
		return nil, encErr
	}
	if err := m.conn.Send(ctx, frame); err != nil {
		m.mu.Lock()
		delete(m.streams, id)
		m.mu.Unlock()
		m.metrics.Streams.Errors.Add(1)
		return nil, fmt.Errorf("mux: send open: %w", err)
	}
	m.activeStreams.Add(1)
	m.metrics.Streams.Opened.Add(1)
	// Active 观测计数（与 activeStreams 同步增减，语义独立：maxStreams 限额 vs 观测）。
	m.streamActiveOpened()
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
		m.streamActiveClosed()
		s.closeChannels()
	}
}

// removeStreamIf 是**按对象身份**的注销：仅当表中当前登记的就是 s 时才移除并递减计数。
//
// 为什么需要它（与按 id 的 removeStream 并存）：调用方持有的句柄可能已**失效**——对端可以
// 先发 Close/Reject 摘掉某个 sid 的表项，**再用同一 sid** 开一条新流（`handleOpenFrame` 对
// 对端任意 sid 只做 exists 检查，不校验角色/单调性；参考实现单调 +2 不复用，故只有不守协议/
// 恶意对端能触发）。此时旧句柄若走按 id 的注销，会摘掉并关闭**别人的活流**、并把 activeStreams
// 错误递减。
//
// 分工：`Abort`（持句柄、稍后才调用：pkg/server/relay_stream.go 的失败/超时路径、
// pkg/sync/httptransport 的强制释放）走本函数；readLoop 中按 sid 处理的路径
// （对端 Close/Reject、重传队列）保持 removeStream。
//
// 返回值报告本次是否真的注销了（幂等闸门：表里登记的是不是 s）。
func (m *Mux) removeStreamIf(id StreamID, s *stream, closeCh bool) bool {
	m.mu.Lock()
	cur, ok := m.streams[id]
	if ok && cur == s {
		delete(m.streams, id)
	} else {
		ok = false
	}
	m.mu.Unlock()
	if ok && closeCh {
		m.activeStreams.Add(-1)
		m.streamActiveClosed()
		s.closeChannels()
	}
	return ok
}

// streamActiveOpened 在流登记时更新观测计数（Active +1、MaxActive 峰值 CAS 更新）。
// 只在真正登记成功（Open 已入表 / handleOpenFrame 已入 acceptCh）时调用，与 activeStreams 同步。
func (m *Mux) streamActiveOpened() {
	cur := m.metrics.Streams.Active.Add(1)
	for {
		max := m.metrics.Streams.MaxActive.Load()
		if cur <= max || m.metrics.Streams.MaxActive.CompareAndSwap(max, cur) {
			return
		}
	}
}

// streamActiveClosed 在流注销时更新观测计数（Active -1）。与 activeStreams 同步调用。
func (m *Mux) streamActiveClosed() {
	m.metrics.Streams.Active.Add(-1)
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
	frame, encErr := EncodeFrame(sid, FrameReject, []byte{reason})
	if encErr != nil { // 不可达：负载为固定 1 字节
		m.metrics.Streams.Errors.Add(1)
		return
	}
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
