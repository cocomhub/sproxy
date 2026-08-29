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
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/internal/icecfg"
	"github.com/pion/ice/v4"
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
// 在创建任何连接前调用（命令入口处）。非法 URL（scheme/host:port 不合法）打 Warn 并跳过。
func SetSTUNServers(servers []string) {
	if servers == nil {
		stunServers = defaultSTUNServers
		return
	}
	// 过滤空串与非法 URL，避免空白/无效 flag 值产生无效 ICE server。
	// 使用独立切片，避免与调用方 slice 共享底层数组（防后续修改污染 stunServers）。
	filtered := make([]string, 0, len(servers))
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !validSTUNURL(s) {
			slog.Warn("webrtc: 忽略非法的 STUN/TURN URL", "url", s)
			continue
		}
		filtered = append(filtered, s)
	}
	stunServers = filtered
}

// turnServers 是当前生效的 TURN 服务器列表（与 stunServers 独立）。
// 空切片 = 不使用 TURN（默认）；SetTURNServers(nil) 清空。
var turnServers []string

// turnUsername / turnPassword 是 TURN 服务器的长期凭据（静态密码模式）。
var turnUsername, turnPassword string

// SetTURNServers 覆盖 TURN 服务器列表（调用方传入 --turn 的多个值）。
// nil = 清空（不使用 TURN）；否则过滤掉空串与非法 URL（打 Warn 并跳过），
// 存储时用独立切片，避免与调用方 slice 共享底层数组（防后续修改污染）。
// 注：只设服务器不设凭据时 defaultConfig 不下发 TURN 条目（pion 需要
// Username/Credential，否则 newPC 校验失败）。
func SetTURNServers(urls []string) {
	if urls == nil {
		turnServers = nil
		return
	}
	filtered := make([]string, 0, len(urls))
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !validSTUNURL(u) {
			slog.Warn("webrtc: 忽略非法的 TURN URL", "url", u)
			continue
		}
		filtered = append(filtered, u)
	}
	turnServers = filtered
}

// SetTURNCredential 设置 TURN 服务器凭据（静态用户名/密码，pion password 模式）。
func SetTURNCredential(username, password string) {
	turnUsername = username
	turnPassword = password
}

// validSTUNURL 校验 STUN/TURN URL 的 scheme 与 host:port 基本格式。
// 支持 stun:/stuns:/turn:/turns: 四种 scheme；TURN URL 可带 ?transport=udp 查询参数。
// 端口要求为数字（STUN/TURN 均用数字端口；服务名端口不属于合法 ICE server URL）。
func validSTUNURL(s string) bool {
	for _, prefix := range []string{"stun:", "stuns:", "turn:", "turns:"} {
		if !strings.HasPrefix(s, prefix) {
			continue
		}
		rest := s[len(prefix):]
		if i := strings.IndexByte(rest, '?'); i >= 0 {
			rest = rest[:i]
		}
		host, port, err := net.SplitHostPort(rest)
		if err != nil || host == "" || port == "" {
			return false
		}
		if _, err := strconv.Atoi(port); err != nil {
			return false
		}
		return true
	}
	return false
}

// signalingTimeout 是 DialWithSignaler/ListenWithSignaler 内 Wait* 的整体超时。
// 默认 30s：hub 信令（mesh/p2p）下对端离线或不对时快速失败回落中继；
// 手工 SDP（--manual）调用方用 SetSignalingTimeout 调大到 10min（人工拷文件）。
var signalingTimeout = defaultICETimeout

// SetSignalingTimeout 覆盖信令等待整体超时（--manual 人工拷文件可调大）。
func SetSignalingTimeout(d time.Duration) { signalingTimeout = d }

// ResetSignalingTimeout 恢复信令等待整体超时为默认值（30s）。
// --manual 场景 SetSignalingTimeout 调大后，命令/测试结束应恢复默认，
// 避免全局泄漏污染库内嵌场景与后续测试（S69）。
func ResetSignalingTimeout() { signalingTimeout = defaultICETimeout }

var useHostOnly bool

// SetHostOnly 控制是否使用仅本机 host 候选（不加 STUN）。
// 主要用于测试与无 STUN 可达性的内网场景；生产跨公网打洞请保持默认（含 STUN）。
// 注意：host-only 模式下远程 ICE 候选过滤全部放行（本机 loopback 候选合法），
// 该开关同时作为安全过滤（SetRemoteIPFilter）的测试专用旁路。
func SetHostOnly(hostOnly bool) { useHostOnly = hostOnly }

// rejectPrivateRemoteCandidates 控制是否拒绝私有网段（RFC1918 + ULA）的远程 ICE 候选。
// 默认 false：LAN mesh 需要放行私网候选做同网段直连；
// 安全敏感部署（公网节点）可开启收紧，避免对端注入内网地址引发 UDP 探测。
var rejectPrivateRemoteCandidates bool

// SetRejectPrivateRemoteCandidates 收紧/放开私网远程候选过滤。
// 默认保持私网放行以支持 LAN mesh；安全敏感部署可显式开启。
// 在创建任何连接前调用（命令入口处）。
func SetRejectPrivateRemoteCandidates(reject bool) { rejectPrivateRemoteCandidates = reject }

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

// configureLoggerFactory 构造 pion 底层日志 factory。
// pion 没有全局 SetLogLevel：级别通过 SettingEngine.LoggerFactory 按 scope 注入，
// 因此这里构造好 factory，newPC 时通过 s.LoggerFactory 传入。
// verbose 开启时把 ice/dtls/sctp/webrtc 四个关键 scope 提到 Trace（打洞排障明细），
// 否则显式设为 Error（无噪音）。
// 每次构造独立 factory 并显式覆盖 4 个 scope 的级别，不依赖 PION_LOG_* 环境变量的
// 单例，避免外部 env 影响行为（也修复了日志级别被环境变量意外抬高的问题）。
func configureLoggerFactory(verboseOn bool) logging.LoggerFactory {
	f := logging.NewDefaultLoggerFactory()
	level := logging.LogLevelError
	if verboseOn {
		// TRACE 级覆盖打洞排障关键链路：ICE 候选/连通性 + DTLS 握手 + SCTP 连接
		level = logging.LogLevelTrace
	}
	for _, scope := range []string{"ice", "dtls", "sctp", "webrtc"} {
		f.ScopeLevels[scope] = level
	}
	return f
}

// logICEEvent 常驻记录 ICE 连接状态变化（打洞失败时的诊断主线）。
// 初始状态在闭包内捕获，去重状态由闭包持有，无需外部 *prev 指针。
// 注意：pion 可能在连接快速关闭时并发触发多次状态回调，去重变量必须加锁保护。
func logICEEvent(pc *webrtc.PeerConnection) {
	prev := pc.ICEConnectionState()
	var mu sync.Mutex
	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		mu.Lock()
		unchanged := prev == s
		if !unchanged {
			prev = s
		}
		mu.Unlock()
		if unchanged {
			return
		}
		level := iceStateLevel(s)
		slog.Log(context.Background(), level,
			"webrtc: ICE 状态变化", "state", s.String())
	})
}

// logPCStateEvent 常驻记录聚合连接状态（ICE+DTLS+SCTP 的最终结果）。
// 初始状态在闭包内捕获，去重状态由闭包持有，无需外部 *prev 指针。
// 注意：pion 可能在连接快速关闭时并发触发多次状态回调，去重变量必须加锁保护。
func logPCStateEvent(pc *webrtc.PeerConnection) {
	prev := pc.ConnectionState()
	var mu sync.Mutex
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		mu.Lock()
		unchanged := prev == s
		if !unchanged {
			prev = s
		}
		mu.Unlock()
		if unchanged {
			return
		}
		level := pcStateLevel(s)
		slog.Log(context.Background(), level,
			"webrtc: 连接状态变化", "state", s.String())
	})
}

// srflxDiag 跟踪最近一次连接的候选收集情况，用于打洞失败时的诊断提示。
type srflxDiag struct {
	mu       sync.Mutex
	gotSrflx bool
	total    int
}

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
// diag 是本次连接的候选诊断实例（每次 newPC 新建，杜绝跨连接累积污染）。
func logCandidateEvents(pc *webrtc.PeerConnection, counter *int, diag *srflxDiag) {
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			slog.Debug("webrtc: ICE 候选收集完成", "candidates", *counter)
			return
		}
		*counter++
		diag.record(c)
		slog.Debug("webrtc: 收集到 ICE 候选", "index", *counter,
			"type", c.Typ.String(), "addr", c.Address, "port", c.Port)
	})
}

// Signal provides in-memory channels for SDP Offer/Answer exchange.
// 注意：chan buffer 为 1，仅支持单连接 rendezvous（单拨号-单监听）；
// 生产 mesh/p2p 请使用 hub.HubSignaler 或 manualSignaler（不经本类型）。
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
func (a signalerAdapter) WaitOffer(ctx context.Context) (string, string, error) {
	select {
	case sdp := <-a.signal.Offer:
		return "", sdp, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}
func (a signalerAdapter) SendAnswer(_ string, sdp string) error {
	a.signal.Answer <- sdp
	return nil
}
func (a signalerAdapter) WaitAnswer(ctx context.Context) (string, string, error) {
	select {
	case sdp := <-a.signal.Answer:
		return "", sdp, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

type webrtcAddr struct{}

func (webrtcAddr) Network() string { return "webrtc" }
func (webrtcAddr) String() string  { return "webrtc" }

// Conn implements net.Conn over a WebRTC DataChannel.
type Conn struct {
	raw       io.ReadWriteCloser
	pc        *webrtc.PeerConnection
	closeCh   chan struct{}
	closeOnce sync.Once
	readMu    sync.Mutex // 串行化 Read（pion detached DataChannel 不支持并发 Read）

	// remotePeerID 是对端信令身份：拨号方为 Dial 的目标 peer；监听方为 offer 的
	// 发送方（offerFrom）。用于 mesh 自动对等发现的 accept 侧恢复拨号者真实 node-id。
	remotePeerID string
}

// RemotePeerID 返回对端信令身份（拨号方=目标 peer；监听方=offer 发送方）。
// 供 mesh accept 侧判断拨号者是否 discovery peer（disc- 前缀）并注册链路。
func (c *Conn) RemotePeerID() string { return c.remotePeerID }

// Read 读取一条消息。与底层读并行监听 closeCh：Close 后 Read 可被确定性唤醒并
// 立即返回错误（pion detached DataChannel 在 pc.Close 后不保证唤醒读循环，
// 实测可能阻塞数秒甚至更久）。Read-after-Close 返回 xfer.ErrConnClosed。
func (c *Conn) Read(b []byte) (int, error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()

	select {
	case <-c.closeCh:
		return 0, xfer.ErrConnClosed
	default:
	}

	type readResult struct {
		n   int
		err error
		buf []byte
	}
	// 在独立 goroutine 中读入临时缓冲，避免 closeCh 命中时 caller 复用 b 与
	// 在途 raw.Read 继续写入 b 产生数据竞争。
	resCh := make(chan readResult, 1)
	go func() {
		tmp := make([]byte, len(b))
		n, err := c.raw.Read(tmp)
		resCh <- readResult{n: n, err: err, buf: tmp}
	}()
	select {
	case r := <-resCh:
		copy(b, r.buf[:r.n])
		return r.n, r.err
	case <-c.closeCh:
		// 有在途数据时 Close 优先：数据丢弃，返回关闭语义错误
		return 0, io.ErrClosedPipe
	}
}

func (c *Conn) Write(b []byte) (int, error) {
	select {
	case <-c.closeCh:
		return 0, io.ErrClosedPipe
	default:
	}
	return c.raw.Write(b)
}

func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.closeCh)
		if c.raw != nil {
			err = c.raw.Close()
		}
		if cerr := c.pc.Close(); err == nil {
			err = cerr
		}
	})
	return err
}
func (c *Conn) LocalAddr() net.Addr                { return webrtcAddr{} }
func (c *Conn) RemoteAddr() net.Addr               { return webrtcAddr{} }
func (c *Conn) SetDeadline(_ time.Time) error      { return nil }
func (c *Conn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *Conn) SetWriteDeadline(_ time.Time) error { return nil }

func defaultConfig() webrtc.Configuration {
	// host-only 是测试专用逃生舱（仅本机候选），此时仍返回空配置（既有行为与测试）。
	// 其余情况：stunServers 与 turnServers 都为空时才返回空配置，否则逐项构建。
	if useHostOnly || (len(stunServers) == 0 && len(turnServers) == 0) {
		return webrtc.Configuration{}
	}
	var servers []webrtc.ICEServer
	if len(stunServers) > 0 {
		servers = append(servers, webrtc.ICEServer{URLs: stunServers})
	}
	// TURN 条目仅在服务器与凭据三者齐备时下发（pion 4.2.18 对无凭据 turn URL
	// 报 ErrNoTurnCredentials，缺凭据时下发会导致 newPC 失败——因此静默不追加）。
	if len(turnServers) > 0 && turnUsername != "" && turnPassword != "" {
		servers = append(servers, webrtc.ICEServer{
			URLs:           turnServers,
			Username:       turnUsername,
			Credential:     turnPassword,
			CredentialType: webrtc.ICECredentialTypePassword,
		})
	}
	return webrtc.Configuration{ICEServers: servers}
}

func newPC() (*webrtc.PeerConnection, *srflxDiag, error) {
	s := webrtc.SettingEngine{}
	s.DetachDataChannels()
	// verbose 时提升 pion 底层 scope（ice/dtls/sctp/webrtc）到 TRACE，便于打洞排障
	s.LoggerFactory = configureLoggerFactory(verbose)
	// 测试专用 loopback 收敛：webrtctest.New(t) 开启后，把 UDP 候选收集收敛到
	// loopback 接口（每个 PeerConnection 独立 socket），避免 Windows 反复弹防火墙
	// 授权框；生产默认 false 不注入。需同时 SetIncludeLoopbackCandidate(true)：
	// pion ICE agent 默认 includeLoopback=false，即使绑了 loopback 也会跳过该 host
	// 候选，导致 ICE 连通性检查必然失败（gather.go 对 loopback 地址有显式过滤）。
	//
	// 注意：这里用「每 PC 独立 loopback socket」（ice.NewUDPMuxDefault + 127.0.0.1），
	// 而非共享单 socket 复用——两个独立 PeerConnection 共享同一 UDP socket 时 pion 的
	// ICE 无法区分包归属（ufrag 串扰），连通性检查必然失败。
	loopbackOnly := icecfg.LoopbackOnly()
	if loopbackOnly {
		// pion 默认 MulticastDNSModeQueryOnly 会另建独立 UDP socket（监听组播），
		// 不走 SetICEUDPMux，绑定非 loopback → 仍触发 Windows 防火墙弹窗。必须显式
		// 禁用 mDNS（loopback 收敛仅本机互联，host 候选用 IP，mDNS 无意义）。
		s.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
		if ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
			// 创建失败回退默认（可能弹窗但不阻断连接）。
			slog.Warn("webrtc: 创建 loopback UDP socket 失败，回退默认候选收集", "err", err)
		} else {
			s.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: ln}))
			s.SetIncludeLoopbackCandidate(true)
		}
	}
	// 安全过滤远程 ICE 候选：默认拒 loopback/link-local/multicast/unspecified/broadcast，
	// 私网（RFC1918+ULA）默认放行（保 LAN mesh）；useHostOnly 或 loopback 收敛开启时
	// 全放行（本机 loopback 候选合法——loopback 收敛时远程候选即本机 loopback，若过滤
	// 会被拒导致 ICE 连通性检查必然失败）。
	if !useHostOnly && !loopbackOnly {
		s.SetRemoteIPFilter(remoteCandidateFilter(rejectPrivateRemoteCandidates))
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(s))
	pc, err := api.NewPeerConnection(defaultConfig())
	if err != nil {
		return nil, nil, err
	}
	// 常驻打洞流程日志：状态流转（Info）+ 失败（Warn）+ 候选收集（Debug）。
	// diag 按连接实例创建，候选收集从零开始，杜绝跨连接累积污染诊断。
	diag := &srflxDiag{}
	logICEEvent(pc)
	logPCStateEvent(pc)
	var candidateCount int
	logCandidateEvents(pc, &candidateCount, diag)
	return pc, diag, nil
}

// remoteCandidateFilter 构造传给 pion SettingEngine.SetRemoteIPFilter 的过滤函数。
// 默认拒：loopback / link-local（unicast+multicast）/ multicast / unspecified / broadcast；
// 私网（RFC1918 + ULA）默认放行（保 LAN mesh），rejectPrivate 为 true 时一并拒绝。
// 过滤发生在 ICE agent addRemoteCandidate（内联候选与 trickle 候选的唯一入口），
// 在任何 connectivity check 发起之前，是零依赖、覆盖内联 + trickle 的安全边界。
func remoteCandidateFilter(rejectPrivate bool) func(net.IP) bool {
	return func(ip net.IP) bool {
		if ip == nil {
			return false
		}
		switch {
		case ip.IsLoopback():
			return false
		case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
			return false
		case ip.IsMulticast():
			return false
		case ip.IsUnspecified():
			return false
		case ip.Equal(net.IPv4bcast): // 255.255.255.255
			return false
		case rejectPrivate && ip.IsPrivate():
			return false
		}
		return true
	}
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
// 整体等待受 signalingTimeout 约束，避免对端离线时永久挂起。
// 该便捷包装使用 context.Background()；需要外部 ctx 取消请用 DialWithSignalerCtx。
func DialWithSignaler(peer string, sig Signaler) (*Conn, error) {
	return DialWithSignalerCtx(context.Background(), peer, sig)
}

// DialWithSignalerCtx 通过指定的 Signaler 建立连接（跨机器可用）。
// peer 是远端节点标识：Offer 发给 peer，Answer 等待来自 peer。
// 整体等待受 ctx 与 signalingTimeout 共同约束：ctx 取消立即返回 ctx.Err()，
// 对端离线超时返回带候选诊断的错误。
func DialWithSignalerCtx(ctx context.Context, peer string, sig Signaler) (*Conn, error) {
	pc, diag, err := newPC()
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
	waitCtx, cancel := context.WithTimeout(ctx, signalingTimeout)
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
	case <-ctx.Done():
		pc.Close()
		return nil, ctx.Err()
	case <-time.After(defaultICETimeout):
		pc.Close()
		return nil, fmt.Errorf("dial: dc open timed out %s", diag.diagnose(!useHostOnly && len(stunServers) > 0))
	}

	raw, err := dc.Detach()
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("dial: detach: %w", err)
	}
	return &Conn{raw: raw, pc: pc, closeCh: make(chan struct{}), remotePeerID: peer}, nil
}

// ListenWithSignaler 通过指定的 Signaler 等待连接（跨机器可用）。
// 等待发给本节点的 Offer，Answer 回给 offer 的发送方。
// 整体等待受 signalingTimeout 约束，避免无拨号方时永久挂起。
// 该便捷包装使用 context.Background()；需要外部 ctx 取消请用 ListenWithSignalerCtx。
// ErrNoIncomingConnection 表示信令/数据通道等待超时（signalingTimeout 或
// defaultICETimeout 内无对端发起连接）。
// 监听方可将此视为"空闲"而非"失败：不应触发重注册/退避重连，只需继续监听（P1-11）。
var ErrNoIncomingConnection = errors.New("webrtc: 无对端在超时窗口内发起连接")

func ListenWithSignaler(peer string, sig Signaler) (*Conn, error) {
	return ListenWithSignalerCtx(context.Background(), peer, sig)
}

// ListenWithSignalerCtx 通过指定的 Signaler 等待连接（跨机器可用）。
// 等待发给本节点的 Offer，Answer 回给 offer 的发送方。
// 整体等待受 ctx 与 signalingTimeout 共同约束：ctx 取消立即返回 ctx.Err()。
func ListenWithSignalerCtx(ctx context.Context, peer string, sig Signaler) (*Conn, error) {
	pc, diag, err := newPC()
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

	waitCtx, cancel := context.WithTimeout(ctx, signalingTimeout)
	defer cancel()
	offerFrom, oJSON, err := sig.WaitOffer(waitCtx)
	if err != nil {
		pc.Close()
		if errors.Is(err, context.DeadlineExceeded) {
			// P1-11：signalingTimeout 内无 offer → 空闲而非失败（哨兵供监听方区分，
			// 不触发重注册/退避重连）。
			return nil, fmt.Errorf("listen: wait offer: %w", ErrNoIncomingConnection)
		}
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
	case <-ctx.Done():
		pc.Close()
		return nil, ctx.Err()
	case <-time.After(defaultICETimeout):
		pc.Close()
		// P1-11：与 wait offer 同语义——无对端连接属空闲而非失败（哨兵供监听方区分）。
		return nil, fmt.Errorf("listen: dc not received within %v %s: %w", defaultICETimeout, diag.diagnose(!useHostOnly && len(stunServers) > 0), ErrNoIncomingConnection)
	}

	// Wait for the DataChannel to open and then detach it.
	openCh := make(chan struct{})
	dc.OnOpen(func() { close(openCh) })
	select {
	case <-openCh:
	case <-ctx.Done():
		pc.Close()
		return nil, ctx.Err()
	case <-time.After(defaultICETimeout):
		pc.Close()
		return nil, fmt.Errorf("listen: dc open timed out %s", diag.diagnose(!useHostOnly && len(stunServers) > 0))
	}

	raw, err := dc.Detach()
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("listen: detach: %w", err)
	}
	return &Conn{raw: raw, pc: pc, closeCh: make(chan struct{}), remotePeerID: offerFrom}, nil
}

// ---------------------------------------------------------------------------
// xfer.Conn / xfer.Transport adapter
// ---------------------------------------------------------------------------

// connReadWriter 抽象底层连接，便于注入测试桩（阻塞写等）。*Conn 满足该接口。
type connReadWriter interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Close() error
}

// webrtcXferConn wraps *Conn to implement xfer.Conn.
//
// 消息分帧：DataChannel 是字节流（无消息边界），xfer.Conn 要求消息保序成块。
// 这里仿照 tcp 传输，用 [4B big-endian length][payload] 帧界定消息，
// 使 mux 的最大帧（8B 头 + 65535 负载）能完整传输，不被 Read 截断。
type webrtcXferConn struct {
	raw connReadWriter
	// mu 串行化 Send 的 raw.Write（DataChannel 写不保证并发安全）。
	// 注意：对端存活但不消费时（SCTP 流控窗口归零）raw.Write 可能长时间阻塞，
	// 因此 mu 不能被 Close 持有——否则 Close 等不到锁、raw.Close() 永不执行、
	// 阻塞中的 Write 永不解除，Send 与 Close 互相等待构成永久死锁（P0-2）。
	mu sync.Mutex
	// closeMu 保护 closed 标志（与 mu 独立）：Close 只短暂抢 closeMu 置位 closed，
	// 随后不经 mu 直接 raw.Close() 解除阻塞中的 Send。
	closeMu sync.Mutex
	closed  bool
}

func (c *webrtcXferConn) Send(ctx context.Context, msg []byte) error {
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return xfer.ErrConnClosed
	}
	c.closeMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// 单条消息上限防御：uint32(len(msg)) 在 len(msg) 超 4GiB 时溢出，且对端
	// maxFrameBytes 缓冲放不下。mux 帧本身封顶 64KiB，此处为传输层防御缺口补漏。
	if len(msg) > maxFrameBytes {
		return fmt.Errorf("webrtc: message too large: %d > %d bytes", len(msg), maxFrameBytes)
	}
	frame := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(msg)))
	copy(frame[4:], msg)
	c.mu.Lock()
	// P0-2：此处 raw.Write 可能无限期阻塞（对端存活但不消费）；Close 不经 mu
	// 直接 raw.Close() 解除阻塞，Write 返回错误后 Send 释放 mu 正常退出。
	_, err := c.raw.Write(frame)
	c.mu.Unlock()
	return err
}

func (c *webrtcXferConn) Receive(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
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
	c.closeMu.Lock()
	if c.closed {
		c.closeMu.Unlock()
		return nil
	}
	c.closed = true
	c.closeMu.Unlock()
	// 不经 c.mu（P0-2）：并发 Send 可能正阻塞在 raw.Write（对端不消费），
	// 若等 c.mu 会死锁。raw.Close() 使阻塞中的 Write 立即返回错误。
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
	signal    *Signal
	addr      string
	acceptCh  chan *webrtcXferConn
	done      chan struct{}
	closeOnce sync.Once // Close 幂等（对齐 tcp.TcpListener）
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
	l.closeOnce.Do(func() { close(l.done) })
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
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// l.done 关闭时取消 loopCtx，使阻塞中的 ListenWithSignalerCtx 立即返回（无泄漏）。
	go func() {
		select {
		case <-l.done:
			cancel()
		case <-loopCtx.Done():
		}
	}()

	for {
		conn, err := ListenWithSignalerCtx(loopCtx, "", signalerAdapter{signal: l.signal})
		if err != nil {
			select {
			case <-l.done:
				return
			case <-loopCtx.Done():
				return
			default:
				// 瞬时失败继续监听
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
