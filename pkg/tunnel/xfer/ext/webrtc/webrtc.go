// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package webrtc provides a WebRTC-based peer-to-peer transport layer built on
// pion/webrtc v4. It offers a net.Conn-like abstraction that uses DataChannel
// as the transport substrate, with in-memory signaling channels for SDP
// Offer/Answer exchange.
//
// Basic usage:
//
//	signal := webrtc.NewSignal()
//
//	// Listener side (goroutine)
//	listener, err := webrtc.Listen(signal)
//	buf := make([]byte, 1024)
//	n, _ := listener.Read(buf)
//
//	// Dialer side
//	conn, err := webrtc.Dial(signal)
//	conn.Write([]byte("hello"))
package webrtc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/pion/logging"
	"github.com/pion/webrtc/v4"
)

func init() {
	xfer.Register(&xfer.Transport{
		Name:   "webrtc",
		Dial:   xferDial,
		Listen: xferListen,
	})
}

const defaultICETimeout = 30 * time.Second

// defaultSTUNServers 是默认 STUN 服务器列表（混合大陆/海外可达性）。
// 单一 Google STUN 在大陆网络常被 UDP 屏蔽/超时，取不到 srflx 候选导致跨 NAT
// 打洞必失败；这里保留 Google（海外最稳）并补上腾讯/小米（大陆通常可达）。
// pion 并发查询全部，任一成功即拿到 srflx；全不通时用 --stun 手工指定。
var defaultSTUNServers = []string{
	"stun:stun.l.google.com:19302",
	"stun:stun.qq.com:3478",
	"stun:stun.miwifi.com:3478",
}

// stunServers 是当前生效的 STUN 服务器列表（pion 并发查询全部，任一成功即可）。
var stunServers = defaultSTUNServers

// SetSTUNServers 覆盖 STUN 服务器列表（调用方传入 --stun 的多个值）。
// 空列表 = 不添加任何 STUN（等价 host-only 候选收集）；传 nil 恢复默认。
// 在创建任何连接前调用（命令入口处）。
func SetSTUNServers(servers []string) {
	if servers == nil {
		stunServers = defaultSTUNServers
		return
	}
	// 过滤空串，避免空白 flag 值产生无效 ICE server
	filtered := servers[:0]
	for _, s := range servers {
		if strings.TrimSpace(s) != "" {
			filtered = append(filtered, s)
		}
	}
	stunServers = filtered
}

// signalingTimeout 是 DialWithSignaler/ListenWithSignaler 内 Wait* 的整体超时。
// 默认 30s：hub 信令（mesh/p2p）下对端离线或不对时快速失败回落中继；
// 手工 SDP（--manual）调用方用 SetSignalingTimeout 调大到 10min（人工拷文件）。
var signalingTimeout = defaultICETimeout

// SetSignalingTimeout 覆盖信令等待整体超时（--manual 人工拷文件可调大）。
func SetSignalingTimeout(d time.Duration) { signalingTimeout = d }

var useHostOnly bool

// SetHostOnly 控制是否使用仅本机 host 候选（不加 STUN）。
// 主要用于测试与无 STUN 可达性的内网场景；生产跨公网打洞请保持默认（含 STUN）。
func SetHostOnly(hostOnly bool) { useHostOnly = hostOnly }

// verbose 控制 pion 底层（ice/dtls/sctp/webrtc 等 scope）的日志级别。
// 打洞失败需要排障时开启：会输出 candidate 收发、STUN binding、DTLS 握手等明细。
// 默认 false：pion 日志级别 Error，仅错误才输出，常驻无噪音。
var verbose bool

// SetVerbose 开启 pion 底层打洞日志（candidate/STUN/DTLS 明细），供 --verbose 排障使用。
// 在创建任何连接前调用（命令入口处）；生效于后续创建的 PeerConnection。
func SetVerbose(v bool) { verbose = v }

// logLevel 是 ICE 状态常量 → slog 级别的映射辅助。
// 正常状态流转打 Info，异常（failed/disconnected）打 Warn。
func iceStateLevel(s webrtc.ICEConnectionState) slog.Level {
	switch s {
	case webrtc.ICEConnectionStateFailed,
		webrtc.ICEConnectionStateDisconnected,
		webrtc.ICEConnectionStateClosed:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// pcStateLevel 是聚合连接状态（含 DTLS/SCTP）→ slog 级别的映射。
func pcStateLevel(s webrtc.PeerConnectionState) slog.Level {
	switch s {
	case webrtc.PeerConnectionStateFailed,
		webrtc.PeerConnectionStateDisconnected,
		webrtc.PeerConnectionStateClosed:
		return slog.LevelWarn
	default:
		return slog.LevelInfo
	}
}

// pionLoggerFactory 是传给 pion 各传输模块的 logger factory。
// verbose 开启时把关键 scope 提到 Trace，否则保持默认 Error（无噪音）。
var pionLoggerFactory = func() logging.LoggerFactory {
	f := logging.NewDefaultLoggerFactory()
	return f
}()

// setupVerboseLogging 在 verbose 开启时提升 pion 底层 scope 的日志级别。
// pion 没有全局 SetLogLevel：级别通过 SettingEngine.LoggerFactory 按 scope 注入，
// 因此这里先构造好 factory，newPC 时通过 s.LoggerFactory 传入。
func configureLoggerFactory(verboseOn bool) logging.LoggerFactory {
	if verboseOn {
		f := logging.NewDefaultLoggerFactory()
		// TRACE 级覆盖打洞排障关键链路：ICE 候选/连通性 + DTLS 握手 + SCTP 连接
		for _, scope := range []string{"ice", "dtls", "sctp", "webrtc"} {
			f.ScopeLevels[scope] = logging.LogLevelTrace
		}
		return f
	}
	return pionLoggerFactory
}

// logICEEvent 常驻记录 ICE 连接状态变化（打洞失败时的诊断主线）。
func logICEEvent(pc *webrtc.PeerConnection, prev *webrtc.ICEConnectionState) {
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		if *prev != s {
			*prev = s
			level := iceStateLevel(s)
			slog.Log(context.Background(), level,
				"webrtc: ICE 状态变化", "state", s.String())
		}
	})
}

// logPCStateEvent 常驻记录聚合连接状态（ICE+DTLS+SCTP 的最终结果）。
func logPCStateEvent(pc *webrtc.PeerConnection, prev *webrtc.PeerConnectionState) {
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if *prev != s {
			*prev = s
			level := pcStateLevel(s)
			slog.Log(context.Background(), level,
				"webrtc: 连接状态变化", "state", s.String())
		}
	})
}

// srflxDiag 跟踪最近一次连接的候选收集情况，用于打洞失败时的诊断提示。
type srflxDiag struct {
	mu       sync.Mutex
	gotSrflx bool
	total    int
}

var lastCandidateDiag srflxDiag

// recordCandidate 记录候选类型（host/srflx/relay），供诊断查询。
func (d *srflxDiag) record(c *webrtc.ICECandidate) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.total++
	switch c.Typ {
	case webrtc.ICECandidateTypeSrflx, webrtc.ICECandidateTypeRelay:
		d.gotSrflx = true
	}
}

// diagnose 返回打洞失败时的候选诊断信息（是否取到公网候选）。
func (d *srflxDiag) diagnose(stunEnabled bool) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case !stunEnabled:
		return "（未配置 STUN，仅 host 候选；跨 NAT 打洞需要 srflx，请用 --stun 指定可达的 STUN 服务器）"
	case d.total == 0:
		return "（未收集到任何 ICE 候选）"
	case !d.gotSrflx:
		return "（仅 host 候选，未获取到公网 srflx：请检查 STUN 服务器可达性，或用 --stun 指定本地可达的服务器）"
	default:
		return "（已获取公网候选，失败更可能是对端不可达或防火墙/对称 NAT）"
	}
}

// logCandidateEvents 常驻记录候选收集（Debug 级；帮判断 host/srflx 是否齐全）。
func logCandidateEvents(pc *webrtc.PeerConnection, counter *int) {
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			slog.Debug("webrtc: ICE 候选收集完成", "candidates", *counter)
			return
		}
		*counter++
		lastCandidateDiag.record(c)
		slog.Debug("webrtc: 收集到 ICE 候选", "index", *counter,
			"type", c.Typ.String(), "addr", c.Address, "port", c.Port)
	})
}

// Signal provides in-memory channels for SDP Offer/Answer exchange.
type Signal struct {
	Offer  chan string
	Answer chan string
}

func NewSignal() *Signal {
	return &Signal{
		Offer:  make(chan string, 1),
		Answer: make(chan string, 1),
	}
}

// Signaler 抽象 SDP Offer/Answer（以及可选 ICE candidate）的交换通道。
//
// 方向语义（重要）：
//   - SendOffer/SendAnswer 的 to 是目标对端节点 ID；
//   - WaitOffer/WaitAnswer 等待「发给本节点」的消息，返回 from（发送方节点 ID）。
//
// 进程内实现用 Signal channel；跨机器实现可走 hub 的 HTTP 存转信令桥。
type Signaler interface {
	// SendOffer 向对端 to 发送 Offer SDP。
	SendOffer(to string, sdp string) error
	// WaitOffer 阻塞等待对端发来的 Offer SDP，返回发送方节点 ID。
	WaitOffer(ctx context.Context) (from, sdp string, err error)
	// SendAnswer 向对端 to 发送 Answer SDP。
	SendAnswer(to string, sdp string) error
	// WaitAnswer 阻塞等待对端发来的 Answer SDP，返回发送方节点 ID。
	WaitAnswer(ctx context.Context) (from, sdp string, err error)
}

// signalerAdapter 把内存 Signal channel 包装成 Signaler，
// 使既有 Dial/Listen 兼容新接口（单进程共享，from 无意义）。
type signalerAdapter struct {
	signal *Signal
}

func (a signalerAdapter) SendOffer(_ string, sdp string) error {
	a.signal.Offer <- sdp
	return nil
}
func (a signalerAdapter) WaitOffer(_ context.Context) (string, string, error) {
	return "", <-a.signal.Offer, nil
}
func (a signalerAdapter) SendAnswer(_ string, sdp string) error {
	a.signal.Answer <- sdp
	return nil
}
func (a signalerAdapter) WaitAnswer(_ context.Context) (string, string, error) {
	return "", <-a.signal.Answer, nil
}

type webrtcAddr struct{}

func (webrtcAddr) Network() string { return "webrtc" }
func (webrtcAddr) String() string  { return "webrtc" }

// Conn implements net.Conn over a WebRTC DataChannel.
type Conn struct {
	raw       io.ReadWriteCloser
	pc        *webrtc.PeerConnection
	closeOnce sync.Once
}

func (c *Conn) Read(b []byte) (int, error)  { return c.raw.Read(b) }
func (c *Conn) Write(b []byte) (int, error) { return c.raw.Write(b) }
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() { err = c.pc.Close() })
	return err
}
func (c *Conn) LocalAddr() net.Addr                { return webrtcAddr{} }
func (c *Conn) RemoteAddr() net.Addr               { return webrtcAddr{} }
func (c *Conn) SetDeadline(_ time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(_ time.Time) error { return nil }

func defaultConfig() webrtc.Configuration {
	if useHostOnly || len(stunServers) == 0 {
		return webrtc.Configuration{}
	}
	return webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: stunServers}},
	}
}

func newPC() (*webrtc.PeerConnection, error) {
	s := webrtc.SettingEngine{}
	s.DetachDataChannels()
	// verbose 时提升 pion 底层 scope（ice/dtls/sctp/webrtc）到 TRACE，便于打洞排障
	s.LoggerFactory = configureLoggerFactory(verbose)
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	pc, err := api.NewPeerConnection(defaultConfig())
	if err != nil {
		return nil, err
	}
	// 常驻打洞流程日志：状态流转（Info）+ 失败（Warn）+ 候选收集（Debug）
	iceState := pc.ICEConnectionState()
	logICEEvent(pc, &iceState)
	pcState := pc.ConnectionState()
	logPCStateEvent(pc, &pcState)
	var candidateCount int
	logCandidateEvents(pc, &candidateCount)
	return pc, nil
}

func marshalLD(pc *webrtc.PeerConnection) (string, error) {
	<-webrtc.GatheringCompletePromise(pc)
	b, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	return string(b), nil
}

// Dial initiates a connection. Listen must be started first.
func Dial(signal *Signal) (*Conn, error) {
	return DialWithSignaler("", signalerAdapter{signal: signal})
}

// Listen waits for an incoming connection. Must be started before Dial.
func Listen(signal *Signal) (*Conn, error) {
	return ListenWithSignaler("", signalerAdapter{signal: signal})
}

// DialWithSignaler 通过指定的 Signaler 建立连接（跨机器可用）。
// peer 是远端节点标识：Offer 发给 peer，Answer 等待来自 peer。
// 整体等待受 defaultICETimeout 约束，避免对端离线时永久挂起。
func DialWithSignaler(peer string, sig Signaler) (*Conn, error) {
	pc, err := newPC()
	if err != nil {
		return nil, fmt.Errorf("dial: new pc: %w", err)
	}

	dc, err := pc.CreateDataChannel("data", nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: create dc: %w", err)
	}

	openCh := make(chan struct{})
	dc.OnOpen(func() { close(openCh) })

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: create offer: %w", err)
	}
	if serr := pc.SetLocalDescription(offer); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: set local desc: %w", serr)
	}

	oJSON, err := marshalLD(pc)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: %w", err)
	}
	if serr := sig.SendOffer(peer, oJSON); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: send offer: %w", serr)
	}

	// 等待对端 Answer（整体超时：默认 30s，--manual 场景可 SetSignalingTimeout 调大）
	waitCtx, cancel := context.WithTimeout(context.Background(), signalingTimeout)
	defer cancel()
	from, aJSON, err := sig.WaitAnswer(waitCtx)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: wait answer: %w", err)
	}
	if from != "" && from != peer {
		pc.Close()
		return nil, fmt.Errorf("dial: answer 来自非预期节点 %q（期望 %q）", from, peer)
	}
	var answer webrtc.SessionDescription
	if serr := json.Unmarshal([]byte(aJSON), &answer); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: unmarshal answer: %w", serr)
	}
	if serr := pc.SetRemoteDescription(answer); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: set remote desc: %w", serr)
	}

	select {
	case <-openCh:
	case <-time.After(defaultICETimeout):
		pc.Close()
		return nil, fmt.Errorf("dial: dc open timed out %s", lastCandidateDiag.diagnose(!useHostOnly && len(stunServers) > 0))
	}

	raw, err := dc.Detach()
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: detach: %w", err)
	}
	return &Conn{raw: raw, pc: pc}, nil
}

// ListenWithSignaler 通过指定的 Signaler 等待连接（跨机器可用）。
// 等待发给本节点的 Offer，Answer 回给 offer 的发送方。
// 整体等待受 defaultICETimeout 约束，避免无拨号方时永久挂起。
func ListenWithSignaler(peer string, sig Signaler) (*Conn, error) {
	pc, err := newPC()
	if err != nil {
		return nil, fmt.Errorf("listen: new pc: %w", err)
	}

	// Non-blocking: just stash the DataChannel when it arrives.
	dcCh := make(chan *webrtc.DataChannel, 1)
	pc.OnDataChannel(func(d *webrtc.DataChannel) {
		select {
		case dcCh <- d:
		default:
		}
	})

	waitCtx, cancel := context.WithTimeout(context.Background(), signalingTimeout)
	defer cancel()
	offerFrom, oJSON, err := sig.WaitOffer(waitCtx)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: wait offer: %w", err)
	}
	var offer webrtc.SessionDescription
	if serr := json.Unmarshal([]byte(oJSON), &offer); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: unmarshal offer: %w", serr)
	}
	if serr := pc.SetRemoteDescription(offer); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: set remote desc: %w", serr)
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: create answer: %w", err)
	}
	if serr := pc.SetLocalDescription(answer); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: set local desc: %w", serr)
	}

	aJSON, err := marshalLD(pc)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: %w", err)
	}
	// Answer 回给 offer 的发送方（offerFrom）；未知则回给 peer（本地 listen 通常即对方）。
	answerTo := offerFrom
	if answerTo == "" {
		answerTo = peer
	}
	if serr := sig.SendAnswer(answerTo, aJSON); serr != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: send answer: %w", serr)
	}

	// Wait for the DataChannel to arrive.
	var dc *webrtc.DataChannel
	select {
	case dc = <-dcCh:
	case <-time.After(defaultICETimeout):
		pc.Close()
		return nil, fmt.Errorf("listen: dc not received within %v %s", defaultICETimeout, lastCandidateDiag.diagnose(!useHostOnly && len(stunServers) > 0))
	}

	// Wait for the DataChannel to open and then detach it.
	openCh := make(chan struct{})
	dc.OnOpen(func() { close(openCh) })
	select {
	case <-openCh:
	case <-time.After(defaultICETimeout):
		pc.Close()
		return nil, fmt.Errorf("listen: dc open timed out %s", lastCandidateDiag.diagnose(!useHostOnly && len(stunServers) > 0))
	}

	raw, err := dc.Detach()
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: detach: %w", err)
	}
	return &Conn{raw: raw, pc: pc}, nil
}

// ---------------------------------------------------------------------------
// xfer.Conn / xfer.Transport adapter
// ---------------------------------------------------------------------------

// webrtcXferConn wraps *Conn to implement xfer.Conn.
//
// 消息分帧：DataChannel 是字节流（无消息边界），xfer.Conn 要求消息保序成块。
// 这里仿照 tcp 传输，用 [4B big-endian length][payload] 帧界定消息，
// 使 mux 的最大帧（8B 头 + 65535 负载）能完整传输，不被 Read 截断。
type webrtcXferConn struct {
	raw *Conn
	mu  sync.Mutex // 串行化 Send（保护 raw.Write 不被并发交错）
}

func (c *webrtcXferConn) Send(_ context.Context, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	frame := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(msg)))
	copy(frame[4:], msg)
	_, err := c.raw.Write(frame)
	return err
}

func (c *webrtcXferConn) Receive(_ context.Context) ([]byte, error) {
	// pion DetachDataChannel 的 Read 是「消息级」：一次返回一整条应用消息，
	// 若 p 小于消息则返回 io.ErrShortBuffer。因此必须先读整帧到足够大的缓冲，
	// 再解析前 4 字节长度（不能先读 4B 再读体——那会把整个帧当一条消息）。
	buf := make([]byte, maxFrameBytes)
	n, err := c.raw.Read(buf)
	if err != nil {
		return nil, err
	}
	if n < 4 {
		return nil, fmt.Errorf("webrtc: frame too short: %d bytes", n)
	}
	msgLen := binary.BigEndian.Uint32(buf[:4])
	if int(msgLen) != n-4 {
		return nil, fmt.Errorf("webrtc: frame length mismatch: header=%d got=%d", msgLen, n-4)
	}
	msg := make([]byte, msgLen)
	copy(msg, buf[4:n])
	return msg, nil
}

func (c *webrtcXferConn) Close() error {
	return c.raw.Close()
}

// maxFrameBytes 是单条消息上限：mux 帧 65543 + 隧道元数据余量，取 1 MiB。
const maxFrameBytes = 1 << 20

// ConnAsXfer 把一个已建立的 WebRTC *Conn 包装成 xfer.Conn，
// 供上层 mux 复用（网络信令建立后，把 DataChannel 当作 xfer 消息通道）。
func ConnAsXfer(c *Conn) xfer.Conn {
	return &webrtcXferConn{raw: c}
}

// globalSignals stores signal channels indexed by address, for xfer.Dial/Listen.
var (
	signals   = make(map[string]*Signal)
	signalsMu sync.Mutex
)

func getOrCreateSignal(addr string) *Signal {
	signalsMu.Lock()
	defer signalsMu.Unlock()
	if s, ok := signals[addr]; ok {
		return s
	}
	s := NewSignal()
	signals[addr] = s
	return s
}

// xferDial implements xfer.Transport.Dial.
func xferDial(ctx context.Context, addr string) (xfer.Conn, error) {
	signal := getOrCreateSignal(addr)
	conn, err := Dial(signal)
	if err != nil {
		return nil, err
	}
	return &webrtcXferConn{raw: conn}, nil
}

// webrtcListener implements xfer.Listener.
type webrtcListener struct {
	signal   *Signal
	addr     string
	acceptCh chan *webrtcXferConn
	done     chan struct{}
}

func (l *webrtcListener) Accept(ctx context.Context) (xfer.Conn, error) {
	select {
	case c := <-l.acceptCh:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, fmt.Errorf("webrtc: listener closed")
	}
}

func (l *webrtcListener) Close() error {
	close(l.done)
	return nil
}

func (l *webrtcListener) Addr() string { return l.addr }

// xferListen implements xfer.Transport.Listen.
func xferListen(ctx context.Context, addr string) (xfer.Listener, error) {
	signal := getOrCreateSignal(addr)
	l := &webrtcListener{
		signal:   signal,
		addr:     addr,
		acceptCh: make(chan *webrtcXferConn, 16),
		done:     make(chan struct{}),
	}
	go l.acceptLoop(ctx)
	return l, nil
}

func (l *webrtcListener) acceptLoop(ctx context.Context) {
	for {
		conn, err := Listen(l.signal)
		if err != nil {
			select {
			case <-l.done:
				return
			default:
				continue
			}
		}
		select {
		case l.acceptCh <- &webrtcXferConn{raw: conn}:
		case <-l.done:
			conn.Close()
			return
		}
	}
}
