// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// pendingOverflowLimit 是单流「已从对端收到、应用尚未消费」字节数（stream.buffered）的上界。
//
// 取值依据（为何在协议上安全）：本侧**只为被取走的帧**补发等长窗口信用（见 Read 里的
// sendWindowUpdateUnsafe），因此**守协议**对端在收到信用前的未确认字节 ≤ DefaultWindowSize；
// 而信用在「取帧入 rBuf」时即发放（该帧可能尚未被应用读完）⇒ 本侧持有量上界
// = DefaultWindowSize + 一帧上限（多出的那一帧即「已发信用但应用还没读完」的余量）。
// 越过上界只可能是对端**超窗口灌数据**（违约）⇒ 只 Abort 该流（见 abortWindowViolation）。
//
// 内存代价：**未消费量**每流最多约 128 KiB（字节上界 + 一帧上限），且**只在真的溢出时才分配**
// （守协议 + 读得快的流恒为 0）；条目数与底层数组分别由 pendingEntryLimit / pendingCompactOff
// 另行约束——「有界」指这三者，**不**意味着底层数组只在队列排空时才增长。
// 该上界也不再依赖「对端帧有多大」的假设（这正是 dataCh 按帧计 64 带来的量纲错配的修法）。
const pendingOverflowLimit = int64(DefaultWindowSize) + MaxFramePayload

// pendingEntryLimit 是溢出缓冲中**数据帧条目数**的上界（EOF 标记不参与该判据，见
// appendPendingLocked ⇒ 含 EOF 时总条目数上界为 pendingEntryLimit+1）。
//
// 为何字节上界不够：对端可以反复发 **0 长度 FrameData**（`DecodeFrame` 对 length==0 返回非 nil
// 空切片）或重复的 `FrameCloseWrite`，把条目数推高而**不增加任何 buffered 字节** ⇒ 只有字节判据
// 时该缓冲仍可被远程无界增长。
// 取值依据：这是**显式限制**，不是「守协议推论」——按字节计窗口的对端仍可发零长帧（零长帧不消耗
// 窗口信用）⇒ 只有显式条目上界能兜住它；取窗口字节数 + 2：守协议且每帧 ≥1 字节时刚好够用
// （最坏 65536 个 1 字节帧），+2 是留给 EOF 标记与边界的余量。
//
// interop 前提：本判据与 pendingOverflowLimit 都假定对端**按字节**计流控窗口（本仓实现如此）。
// 若某实现**按帧**计窗口，它可能在字节上仍「守协议」却因字节/条目上界被判违约 ⇒ 属行为差异，
// 需协议文档与对端实现确认（本仓 `stream.Write` 对 len(p)==0 直接返回不发帧，仓内对端不受影响）。
const pendingEntryLimit = int(DefaultWindowSize) + 2

// pendingCompactOff 是触发「压缩已消费前缀」的**绝对**阈值（已消费条目数）。
//
// 为何需要：`append` 不回收已消费前缀，`tryPull` 又只在整条队列**排空**时才整体释放
// （见 tryPull 末尾）⇒ 若对端长期保持溢出（读得比写得快，但队列从不排空），底层数组会随
// **传输总量**线性增长（实测 ~24 B/帧，20 万轮 ≈ 5.4 MB），与「溢出缓冲内存有界」的结论不符
// ——有界的是**未消费量**（pendingOverflowLimit / pendingEntryLimit），不是底层数组。
//
// 取值依据：压缩需搬运「未消费条目」（≤ pendingEntryLimit）个指针 ⇒ 最坏摊销
// ≈ pendingEntryLimit/pendingCompactOff ≈ 16 次指针拷贝/被消费条目；而队列浅的常见情形
// 只搬运数千个指针。取更小值会抬高摊销开销，取更大值会抬高数组峰值 ⇒ 4096 是两者折中，
// 且把峰值钉在「存活条目 + pendingCompactOff」同阶（实测见
// TestStreamOverflow_BackingArrayBoundedUnderSustainedSpill）。
const pendingCompactOff = 4096

// stream 是 Stream 接口的内部实现。
type stream struct {
	id   StreamID
	mux  *Mux
	rBuf []byte
	rOff int
	rMu  sync.Mutex

	// lastActivity 是最近一次「本地活动」的 UnixNano：创建（newStream）与 Read/Write 时更新。
	// 用途：采样当前流的最久空闲时长（见 Mux.StreamStats / Metrics.LongestIdle）——acceptor
	// 侧流从不主动 Close（审计 F6），对端失联时流表滞留 ⇒ 空闲时长持续增长即「疑似泄漏流」
	// 哨兵。原子更新（Read/Write 可能并发）。
	lastActivity atomic.Int64

	closeMu sync.Mutex
	dataCh  chan []byte
	done    chan struct{}

	// buffered 是「已从对端收到、应用尚未消费」的字节数，含 dataCh、溢出缓冲与 rBuf 未读余量。
	// 它是与流控窗口可比的量，也是违约判定的依据（见 pendingOverflowLimit）：在入队前比较，
	// 因此两条队列合计都受同一上界约束。
	buffered atomic.Int64

	// 溢出缓冲（2026-09-16 审计 F2 主体修复）：dataCh 满时帧改落此处，**绝不阻塞 readLoop**
	// （生产者就是 readLoop，阻塞它等于冻结整条 mux）。
	//   - 顺序不变式：一旦 pending 尚有未消费项（pendingOff < len(pending)），后续帧**一律**
	//     追加 pending；否则「先入 dataCh 的新帧」会在 Read 里排在「更早到达但已溢出」的旧帧
	//     之前（乱序）。读取只发生在 Read（单消费者），写入只发生在 readLoop（单生产者）；
	//     加锁是为了让「判断是否处于溢出模式」与「追加」成为**一次原子**判定。
	//   - nil 元素表示 EOF 标记（与 dataCh 中 nil=EOF 的既有约定一致）。
	//   - 与 dataCh 一样**从不关闭**（见 closeChannels 的说明）。
	//   - 条目数同样有界（pendingEntryLimit）：零字节帧不增字节只增条目，需单独判据。
	pendingMu  sync.Mutex
	pending    [][]byte
	pendingOff int
	// pendingEOF 表示已有一个 EOF 标记在溢出缓冲中（重复的 FrameCloseWrite 语义幂等，
	// Read 在首个标记处即返回 io.EOF）⇒ 去重以保住条目数有界。
	pendingEOF bool

	windowSize     atomic.Int32
	windowUpdateCh chan struct{}

	// pendingWindowUpdate 是因 writeCh 打满而**未能投递**的窗口信用（字节数）。
	// 由 Mux.sendWindowUpdateUnsafe 累加，由 writeLoop 的 ticker 经 flushPendingWindowUpdates
	// 补送（见 retransmit.go：信用不得静默丢失）。取走用 CAS，因此与并发的 Read 累加安全。
	pendingWindowUpdate atomic.Int32

	rejected atomic.Bool

	// writePool 复用小写入的发送缓冲（写入被 writeLoop 消费后归还）。
	// 仅用于单帧负载 ≤ maxCachedWriteLen 的常规小写入；大写入/直接模式走原路径。
	writePool sync.Pool
}

// maxCachedWriteLen 是 writePool 可复用的单帧负载上限。超过此值直接分配（大缓冲
// 无法从池中获益，反而挤占池容量）。
const maxCachedWriteLen = 65536

// getWriteBuf 从 writePool 取一块至少 size 字节的发送缓冲。
// 缓冲由 writeLoop 消费（EncodeFrame 会拷贝负载或直接出线）后归还，不要求调用方归还。
func (s *stream) getWriteBuf(size int) []byte {
	bp, _ := s.writePool.Get().(*[]byte)
	if bp != nil && cap(*bp) >= size {
		return (*bp)[:size]
	}
	if bp != nil {
		s.writePool.Put(bp)
	}
	return make([]byte, size)
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
	s.lastActivity.Store(time.Now().UnixNano())
	return s
}

// touch 更新流的 lastActivity（Read/Write 热路径调用；原子写，开销可忽略）。
func (s *stream) touch() { s.lastActivity.Store(time.Now().UnixNano()) }

func (s *stream) ID() StreamID { return s.id }

func (s *stream) closeChannels() {
	s.closeMu.Lock()
	select {
	case <-s.done:
	default:
		// 只关 done，**不关**收帧通道（dataCh 与溢出缓冲）。Read 通过 done 分支返回关闭错误，
		// 而 pushFrame 在 done 关闭后即丢弃负载（进入时先做非阻塞 done 检查）；若同时关闭
		// dataCh，对已关闭通道的 send 会 panic（Abort 本地关流后对端仍可能发数据）。
		close(s.done)
	}
	s.closeMu.Unlock()
}

// takePendingWindowUpdate CAS 取走待补送的窗口信用并归零，返回取到的字节数（0 = 无待补送）。
//
// 用 CAS 而非 Load+Store：Read 路径可能并发调用 sendWindowUpdateUnsafe 累加，读改写会丢信用。
// 取走但投递再失败时调用方**原样加回**（见 flushPendingWindowUpdates），因此每个字节的信用
// 恰好交付一次——可能延迟，但不丢失、不重复。
func (s *stream) takePendingWindowUpdate() int32 {
	for {
		cur := s.pendingWindowUpdate.Load()
		if cur <= 0 {
			return 0
		}
		if s.pendingWindowUpdate.CompareAndSwap(cur, 0) {
			return cur
		}
	}
}

// pushData 把一帧数据交给流（生产中由 readLoop 调用）。
func (s *stream) pushData(payload []byte) { s.pushFrame(payload) }

// pushEOF 把 EOF 标记（对端 FrameCloseWrite）交给流；顺序与数据帧一致（见 pushFrame）。
func (s *stream) pushEOF() { s.pushFrame(nil) }

// pushFrame 投递一帧（payload==nil 表示 EOF 标记）。**绝不阻塞调用方**。
//
// 生产中的调用方是 readLoop（单 goroutine 串行处理所有帧）⇒ 这里的任何阻塞都会冻结整条 mux
// （含其它健康流），并因收不到对端 Pong 而在 90–120s 后被本侧心跳超时拆掉整条连接。
// 改造前这里是**无 default、无超时**的通道投递，而 dataCh 容量按**帧**计（64）而窗口按**字节**
// 计（65536）⇒ 对端用小帧（平均 <1 KiB）在窗口内写就能让第 65 帧阻塞 readLoop（审计 F2）。
//
// 热路径开销（快路径：dataCh 未满、未进入溢出模式）——2 次原子 load（`buffered` 违约判定 +
// `trackBufferedMax`）+ 1 次原子 Add（记账）+ 一次**无竞争**的 `pendingMu` 配对，零分配；
// 只有真的溢出时才 append / 压缩（见 pendingCompactOff），且仅在**预测**会满时才取两次时钟观测。
//
// 与改造前的语义差异（均为有意）：
//   - dataCh 满 ⇒ 落**溢出缓冲**（FIFO、有界，见 pendingOverflowLimit），不再等待读者；
//   - 流已终止（done 关闭）⇒ 帧一律丢弃（与改造前 select 可能选中 done 分支的效果一致）；
//   - 对端超出窗口（buffered 越过上界）⇒ 丢弃该帧并 **Abort 该流**（fail-closed）。
func (s *stream) pushFrame(payload []byte) {
	select {
	case <-s.done:
		return
	default:
	}

	// 违约判定在**入队前**：这样 buffered 的上界对两条队列同时成立（不只在溢出路径）——
	// dataCh 有富余时也可能越过上界（大帧＋未消费）。
	if payload != nil && s.buffered.Load()+int64(len(payload)) > pendingOverflowLimit {
		s.abortWindowViolation()
		return
	}

	// 溢出模式：pending 尚有未消费项 ⇒ 必须继续追加（保住 FIFO，见 pending 字段的说明）。
	s.pendingMu.Lock()
	if s.pendingOff < len(s.pending) {
		ok := s.appendPendingLocked(payload)
		s.pendingMu.Unlock()
		if !ok {
			s.abortWindowViolation()
		}
		return
	}
	s.pendingMu.Unlock()

	// 观测（不改语义）：用 len()==cap() 预测「这次会不会被挤到溢出缓冲」，只有预测为满时才取时钟。
	// 修复后这里的等待时间应≈0；Waits 仍会记录「险些阻塞」的次数（背压信号，非故障）。
	full := len(s.dataCh) == cap(s.dataCh)
	var start time.Time
	if full {
		start = s.mux.metrics.ReadLoopPush.enter()
	}
	select {
	case s.dataCh <- payload:
		if payload != nil {
			s.buffered.Add(int64(len(payload)))
			s.trackBufferedMax()
		}
	case <-s.done: // 与流终止竞争：丢弃（改造前 select 亦可能选中该分支）
	default:
		// dataCh 满：落溢出缓冲——**不再阻塞 readLoop**。
		s.pendingMu.Lock()
		ok := s.appendPendingLocked(payload)
		s.pendingMu.Unlock()
		if !ok {
			s.abortWindowViolation()
		}
	}
	if full {
		s.mux.metrics.ReadLoopPush.leave(start)
	}
	if n := int64(len(s.dataCh)); n > s.mux.metrics.DataChMaxFrames.Load() {
		s.mux.metrics.DataChMaxFrames.Store(n)
	}
}

// appendPendingLocked 追加一帧到溢出缓冲（须持 pendingMu）。payload==nil 是 EOF 标记（不计字节）。
// 返回 false 表示**条目数越界**（对端在用零字节帧涌填充缓冲）⇒ 调用方按违约处理。
func (s *stream) appendPendingLocked(payload []byte) bool {
	if payload == nil {
		if s.pendingEOF { // 重复的 EOF 标记语义幂等（去重，保住条目数有界）
			return true
		}
		s.pendingEOF = true
	} else if len(s.pending)-s.pendingOff >= pendingEntryLimit {
		return false
	}
	s.pending = append(s.pending, payload)
	if payload == nil {
		return true
	}
	s.buffered.Add(int64(len(payload)))
	s.trackBufferedMax()
	s.mux.metrics.StreamOverflowSpills.Add(1)
	return true
}

// trackBufferedMax 更新「单流已收未消费字节数」的峰值观测（只增不减的 gauge）。
func (s *stream) trackBufferedMax() {
	if cur := s.buffered.Load(); cur > s.mux.metrics.MaxBufferedBytes.Load() {
		s.mux.metrics.MaxBufferedBytes.Store(cur)
	}
}

// abortWindowViolation 判定对端**违反流控**（本侧持有量越过 pendingOverflowLimit）：
// 计数 + 告警 + **只 Abort 该流**（fail-closed）。
//
// 为什么不拆连接：违约方之外的流不应受影响（readLoop 改造的目标正是「单个流的问题不要升级成
// 整条连接的故障」）。为什么不给对端发帧：Abort 不经 writeCh（从 readLoop 里再引入一个可能
// 阻塞的投递恰好是本次修复要消除的东西）；对端会因信用不再增长而自行停写或超时。
//
// 已入队的合规字节仍可被应用读出（Read 先清空两条队列，见其 done 分支），随后该流终止：
// 违约帧不交给应用（长度语义已不可信），但也不静默丢弃（有指标与日志）。
func (s *stream) abortWindowViolation() {
	s.mux.metrics.StreamWindowViolations.Add(1)
	s.mux.logger.Warn("mux: peer exceeded flow-control window, aborting stream",
		"stream", s.id, "buffered", s.buffered.Load(), "limit", pendingOverflowLimit)
	_ = s.Abort()
}

// pullResult 表示一次「取帧」的结果。
type pullResult int

const (
	pullNone pullResult = iota // 两条队列皆空
	pullData                   // 取到数据帧
	pullEOF                    // 取到 EOF 标记（对端 FrameCloseWrite）
)

// tryPull 非阻塞取一帧：**先 dataCh，再溢出缓冲**。
//
// 顺序不变式：溢出模式下新帧只进 pending（见 pushFrame），故 dataCh 中的帧在顺序上总是早于
// pending 中的帧 ⇒ 「先取 dataCh」即得 FIFO。
func (s *stream) tryPull() ([]byte, pullResult) {
	select {
	case data, ok := <-s.dataCh:
		if !ok { // dataCh 从不关闭（见 closeChannels），仅防御
			return nil, pullNone
		}
		if data == nil {
			return nil, pullEOF
		}
		return data, pullData
	default:
	}

	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if s.pendingOff >= len(s.pending) {
		return nil, pullNone
	}
	data := s.pending[s.pendingOff]
	s.pending[s.pendingOff] = nil // 释放引用：避免已消费帧被底层数组长期持有
	s.pendingOff++
	if data == nil {
		s.pendingEOF = false // 标记已被取走，后续（异常的）重复标记可再入队一次
	}
	if s.pendingOff == len(s.pending) { // 排空即整体释放（下次溢出重新分配）
		s.pending, s.pendingOff = nil, 0
	} else if s.pendingOff >= pendingCompactOff {
		// 压缩已消费前缀：把未消费部分搬到**新数组**（make+copy ⇒ cap 恰好等于存活条目数，
		// 不会像 append 那样超额分配）⇒ 数组峰值钉在「存活条目 + pendingCompactOff」同阶，
		// 不再随传输总量增长（见 pendingCompactOff 与
		// TestStreamOverflow_BackingArrayBoundedUnderSustainedSpill）。
		//
		// 为何可在此处做：本函数持 pendingMu，与「判空」「追加」互斥 ⇒ 压缩期间不会有生产者
		// 观察到中间态；FIFO 语义不变（只改变同一序列的存储位置，pendingOff 同步归零），
		// pendingEOF（独立 bool）不受影响。已消费条目在下面逐条置 nil，故旧数组不持有负载引用。
		rest := make([][]byte, len(s.pending)-s.pendingOff)
		copy(rest, s.pending[s.pendingOff:])
		s.pending, s.pendingOff = rest, 0
	}
	if data == nil {
		return nil, pullEOF
	}
	return data, pullData
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

	s.touch()
	s.rMu.Lock()
	defer s.rMu.Unlock()

	for s.rOff >= len(s.rBuf) {
		// 优先消费已缓冲的数据：对端可能已发送数据后立即关闭（FrameClose），
		// 若 select 随机选中 done 分支，已缓冲数据会被跳过误报关闭——I27 拨号
		// 结果帧读取在叶子「接受后立即关」场景的可靠性依赖此行为。仅当无缓冲
		// 数据时才等待新数据或关闭信号。
		//
		// 取帧顺序（F2 溢出缓冲引入后必须保持 FIFO）：dataCh → 溢出缓冲 pending。
		// 两条队列之间不会乱序：一旦 pending 尚有未消费项，pushFrame 的后续帧**一律**
		// 追加 pending，因此 dataCh 中遗留的帧在顺序上总是早于 pending 中的帧。
		data, kind := s.tryPull()
		if kind == pullNone {
			select {
			case d, ok := <-s.dataCh:
				if !ok { // dataCh 从不关闭（见 closeChannels），仅防御
					return 0, s.rejectedOrClosedErr()
				}
				data = d
				if d == nil {
					kind = pullEOF
				} else {
					kind = pullData
				}
			case <-s.done:
				// P1-6：done 就绪但队列可能同时有数据（readLoop 先 pushData 再
				// closeChannels，窗口内两分支同时就绪，Go select 随机选取）。必须
				// 优先非阻塞清空**两条**队列，仅当确无数据才报关闭——否则已投递的数据帧
				// 有 ~50% 概率被丢弃（I27 拨号结果帧读取在叶子"接受后立即关"场景的
				// 可靠性依赖此行为）。
				data, kind = s.tryPull()
				if kind == pullNone {
					return 0, s.rejectedOrClosedErr()
				}
			}
		}
		if kind == pullEOF {
			return 0, io.EOF
		}
		s.rBuf = data
		s.rOff = 0
		s.mux.sendWindowUpdateUnsafe(s, int32(len(data)))
	}

	n = copy(p, s.rBuf[s.rOff:])
	s.rOff += n
	// 只有被应用真正取走的字节才离开「已收未消费」账（信用则在取帧时即已发放，见上）。
	s.buffered.Add(-int64(n))
	s.mux.metrics.Streams.BytesRead.Add(int64(n))
	return n, nil
}

// Write 将 p 写入流。返回 n = 实际投递给 writeLoop 的字节数（≤ len(p)）。
//
// 小负载（≤ maxCachedWriteLen）从 writePool 复用发送缓冲（EncodeFrame 会拷贝负载，
// 故可安全复用）；大负载与 closeMarker 等直接投递。返回的 n 语义与历史一致（窗口
// 受限短写：窗口小于 len(p) 时只投递窗口允许的一段）。
func (s *stream) Write(p []byte) (n int, err error) {
	if s.rejected.Load() {
		return 0, fmt.Errorf("mux: stream %d: %w", s.id, ErrStreamRejected)
	}
	if len(p) == 0 {
		return 0, nil
	}

	s.touch()
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
	// 单帧负载上限（issue #213）：窗口是 DefaultWindowSize=65536，而帧头 Length 只有
	// 2 字节（上限 65535）。不在此收敛就会出现「发送 N 字节、对端只收到 65535」的静默丢字节，
	// 使整条字节流错位（上层分块加密报 GCM 认证失败）。超出部分由调用方的 writeFull 循环续写。
	if writeLen > MaxFramePayload {
		writeLen = MaxFramePayload
	}

	var cp []byte
	if writeLen <= maxCachedWriteLen {
		cp = s.getWriteBuf(writeLen)
	} else {
		cp = make([]byte, writeLen)
	}
	copy(cp, p[:writeLen])

	select {
	case s.mux.writeCh <- writeMsg{streamID: s.id, data: cp}:
		s.windowSize.Add(-int32(writeLen))
		s.mux.metrics.Streams.BytesWritten.Add(int64(writeLen))
		// 缓冲已投递（writeLoop 消费后由 sendFrame 归还到 writePool），此处不归还。
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

// Abort 立即放弃该流（非阻塞）：关闭本地 done 以解除 Read/Write 阻塞，并**从流表注销**
// （移除表项 + 递减 activeStreams；与对端 Close 共用同一注销逻辑，但按**对象身份**判定，
// 见 Mux.removeStreamIf）。
//
// 与 Close 的区别：
//   - Close 经 writeCh 向对端发送 FrameClose 优雅关闭；writeCh 打满且 done 未关闭时
//     会永久阻塞（对端停读导致流控窗口耗尽、重传积压的收尾路径）。
//   - Abort 不经 writeCh（因此永不阻塞），立即解除 Read/Write 阻塞，用于收尾/超时强制释放
//     （对齐 meshForwardListen 的非阻塞关闭范本）。
//
// **为什么必须注销**：`activeStreams` 是 `maxStreams` 门禁的判据；若 Abort 只关通道而不递减，
// 流表项与并发计数会永久残留（2026-09-16 审计确认的缺陷，与本仓 pkg/cloud 修过的
// 「置标记但没有清理者」同型）。生产路径当前**不设** maxStreams，因此今天的主要危害是：
// 长寿 mux（hub↔leaf 隧道、`pkg/server/relay_stream.go`、`pkg/sync/httptransport` 的强制关闭）
// 上按被放弃的流数**无界累积**表项与计数；配 `WithMaxStreams(n)` 时才会放大为「永久拒绝新流」。
//
// 幂等性：注销走 `Mux.removeStreamIf`——闸门是「表里登记的是不是**这个对象**」，所以重复 Abort、
// Abort 与 Close/对端 Close 交叉都只递减一次（不会把计数打成负数），且旧句柄不会误伤同 sid 的新流；
// 末尾的 closeChannels 兜底保证「即使已被注销/从未登记，done 也必定关闭」（closeMu + select 防重入）。
//
// 语义注意：Abort 不向对端发送关闭帧——对端感知本侧已放弃流依赖其自身的
// 半关闭传播或超时机制。与 retransmitLoop 的交互：流被 Abort 后若其数据帧仍在重传队列，
// 对端收到未知流的数据帧会直接丢弃，无副作用。
func (s *stream) Abort() error {
	// 按**对象身份**注销（而非只按 sid）：句柄可能已失效（对端复用同 sid 后开新流），
	// 按 id 注销会摘掉并关闭别人的活流。见 Mux.removeStreamIf 的说明。
	s.mux.removeStreamIf(s.id, s, true)
	s.closeChannels() // 兜底：表里已无该流时注销不再关通道，这里保证契约不被表状态影响
	return nil
}

var closeMarker = make([]byte, 0)

type writeMsg struct {
	streamID StreamID
	data     []byte // nil=CloseWrite, empty([]byte{})=Close
	isRaw    bool
	// datagram true 表示 isRaw 帧是 UDP 数据报（FrameDatagram）：发送失败只丢弃
	// 该数据报（UDP 语义），不关闭整个 mux（流数据帧有重传保护，可关；数据报高频
	// 瞬时失败不应连带杀掉同 mux 的 TCP 流）。
	datagram bool
	// pong true 表示 isRaw 帧是心跳回复（FramePong）：与 datagram 同理只丢弃不关连接。
	// 必要性：改经 writeCh 之前，Pong 是在 readLoop 内直接 conn.Send 且**忽略**失败；
	// 若沿用控制帧的「发送失败即关 mux」取舍，会把一次瞬时发送失败放大成拆整条连接
	// （与「减少非必要连接拆除」的目标相反）。
	pong bool
}
