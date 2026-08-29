// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// 直连 WebRTC 信令：mDNS 无 hub 场景下，拨号方直连对端广播的 TCP 信令端点完成
// SDP offer/answer 交换（替代 hub.HubSignaler 的 HTTP 存转桥）。
//
// 协议：每帧 [4B BE len][JSON]，与网关/拨号帧同款长度前缀。
//   - offer 帧：`{"node":"<拨号方 node-id>","sdp":"<offer sdp>"}`（node 供监听侧
//     恢复对端身份，mesh 自动对等发现的 accept 侧据此注册链路）
//   - answer 帧：`{"sdp":"<answer sdp>"}`
//
// 两端分别实现 webrtc.Signaler：
//   - 监听侧 directSignalerServer：WaitOffer 弹出一条已接受的 TCP 连接并读 offer；
//     SendAnswer 把 answer 写回该连接并关闭（单连接一次往返，SDP 已含全部 ICE
//     候选，无需 trickle）。
//   - 拨号侧 directSignalerClient：SendOffer 连上端点写 offer；WaitAnswer 读 answer。

const (
	// directSignalTimeout 是直连信令单次读写/握手的超时。
	directSignalTimeout = 30 * time.Second
	// directSignalMaxFrame 是直连信令帧长度上限（SDP 含全部 ICE 候选，量级 K 级）。
	directSignalMaxFrame = 512 << 10
)

// directSignalMsg 是直连信令帧的载荷。
type directSignalMsg struct {
	// Node 是拨号方 node-id（offer 帧携带；answer 帧为空）。
	Node string `json:"node,omitempty"`
	// SDP 是 offer/answer 的 SDP JSON。
	SDP string `json:"sdp"`
}

// errDirectSignalConn 表示一条对端信令连接异常（提前关闭/畸形长度前缀/非法 JSON/
// 缺 SDP）。属 per-connection 瞬时失败：runWebRTCAcceptLoop 收到该哨兵应关闭该连接
// 并继续监听，而非把整节点判死（端口扫描/curl 误连等任何可 TCP 触达的进程都能
// 触发，若按致命处理即成为远程无认证 DoS）。
var errDirectSignalConn = errors.New("direct signal: 对端信令连接异常")

// DirectSignalServer 是直连信令的 TCP 监听器：accept 到的连接压入通道，供
// directSignalerServer.WaitOffer 弹出。一个实例服务于 mesh node 的整个生命周期，
// 每条连接对应一次 webrtc 信令往返。
type DirectSignalServer struct {
	ln     net.Listener
	connCh chan net.Conn

	closed    chan struct{}
	closeOnce sync.Once
}

// NewDirectSignalServer 监听 addr（空回落 ":0" 全接口随机端口），返回服务器。
// 调用方随后启动 Serve(ctx) 进入 accept 循环。
func NewDirectSignalServer(addr string) (*DirectSignalServer, error) {
	if addr == "" {
		addr = ":0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("直连信令监听失败: %w", err)
	}
	return &DirectSignalServer{
		ln:     ln,
		connCh: make(chan net.Conn, 4),
		closed: make(chan struct{}),
	}, nil
}

// Addr 返回实际监听地址（NewDirectSignalServer(":0") 后取端口供 mDNS 广播）。
func (s *DirectSignalServer) Addr() net.Addr { return s.ln.Addr() }

// Serve 进入 accept 循环，把每条入站连接交给信令消费者（WaitOffer）；ctx 取消或
// Close 时关闭监听器退出。阻塞直到退出。
func (s *DirectSignalServer) Serve(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-s.closed:
		}
	}()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				continue
			}
		}
		select {
		case s.connCh <- c:
		case <-s.closed:
			_ = c.Close()
			return
		}
	}
}

// Close 关闭监听器并排空未消费的已 accept 连接（幂等）。
// Serve 的 push 与 Close 的排空以 s.closed 关闭为界：关闭后 Serve 的 select 会走
// <-s.closed 分支不再 push；排空循环把 connCh 中残留的连接关闭，释放 FD。
func (s *DirectSignalServer) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		_ = s.ln.Close()
		for {
			select {
			case c := <-s.connCh:
				_ = c.Close()
			default:
				return
			}
		}
	})
	return nil
}

// NewSignaler 构造监听侧 Signaler（每次 webrtc.ListenWithSignalerCtx 复用同一实例，
// WaitOffer 依次弹出一条已 accept 的连接）。
func (s *DirectSignalServer) NewSignaler() webrtc.Signaler {
	return &directSignalerServer{srv: s}
}

// directSignalerServer 实现 webrtc.Signaler 的监听侧（WaitOffer + SendAnswer）。
type directSignalerServer struct {
	srv  *DirectSignalServer
	mu   sync.Mutex
	conn net.Conn // 待 SendAnswer 回写的连接
}

func (s *directSignalerServer) SendOffer(_ string, _ string) error {
	return errors.New("direct signal: 监听侧不应发送 offer")
}

func (s *directSignalerServer) WaitOffer(ctx context.Context) (string, string, error) {
	var c net.Conn
	select {
	case c = <-s.srv.connCh:
	case <-ctx.Done():
		return "", "", ctx.Err()
	case <-s.srv.closed:
		return "", "", net.ErrClosed
	}
	// 关闭上一次 WaitOffer 成功后未 SendAnswer 的遗留连接（后续 SDP 解析失败等场景
	// 可能跳过 SendAnswer），防 FD 泄漏累积。
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.mu.Unlock()

	// ctx 感知读：peer 连上但不发数据时，ctx 取消（节点关停）能立即解除读阻塞并
	// 关闭连接，避免最多等 directSignalTimeout(30s) 才退出。
	if err := c.SetDeadline(time.Now().Add(directSignalTimeout)); err != nil {
		_ = c.Close()
		return "", "", errDirectSignalConn
	}
	type readResult struct {
		msg directSignalMsg
		err error
	}
	readCh := make(chan readResult, 1)
	go func() {
		var msg directSignalMsg
		err := readDirectSignalFrame(c, &msg)
		readCh <- readResult{msg: msg, err: err}
	}()
	var rr readResult
	select {
	case rr = <-readCh:
	case <-ctx.Done():
		_ = c.Close()
		return "", "", ctx.Err()
	}
	if rr.err != nil {
		_ = c.Close()
		return "", "", errDirectSignalConn
	}
	_ = c.SetDeadline(time.Time{})
	if rr.msg.SDP == "" {
		_ = c.Close()
		return "", "", errDirectSignalConn
	}
	s.mu.Lock()
	s.conn = c
	s.mu.Unlock()
	return rr.msg.Node, rr.msg.SDP, nil
}

func (s *directSignalerServer) SendAnswer(_ string, sdp string) error {
	s.mu.Lock()
	c := s.conn
	s.conn = nil
	s.mu.Unlock()
	if c == nil {
		return errors.New("direct signal: 无待应答的 offer 连接")
	}
	defer func() { _ = c.Close() }()
	return writeDirectSignalFrame(c, directSignalMsg{SDP: sdp})
}

func (s *directSignalerServer) WaitAnswer(_ context.Context) (string, string, error) {
	return "", "", errors.New("direct signal: 监听侧不应等待 answer")
}

// DirectSignaler 是直连信令的拨号侧视图：实现 webrtc.Signaler，并显式 Close
// 关闭底层 TCP 连接（信令握手完成后调用方负责释放）。
type DirectSignaler interface {
	webrtc.Signaler
	Close() error
}

// DialDirectSignaler 连接对端直连信令端点，返回拨号侧 Signaler。
// nodeID 是本节点 node-id（随 offer 发送，供对端恢复身份）。
func DialDirectSignaler(ctx context.Context, addr, nodeID string) (DirectSignaler, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("直连信令连接失败: %w", err)
	}
	return &directSignalerClient{conn: conn, nodeID: nodeID}, nil
}

// directSignalerClient 实现 webrtc.Signaler 的拨号侧（SendOffer + WaitAnswer）。
type directSignalerClient struct {
	conn   net.Conn
	nodeID string
}

func (c *directSignalerClient) SendOffer(_ string, sdp string) error {
	if err := c.conn.SetWriteDeadline(time.Now().Add(directSignalTimeout)); err != nil {
		return err
	}
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	return writeDirectSignalFrame(c.conn, directSignalMsg{Node: c.nodeID, SDP: sdp})
}

func (c *directSignalerClient) WaitAnswer(ctx context.Context) (string, string, error) {
	if err := c.conn.SetReadDeadline(time.Now().Add(directSignalTimeout)); err != nil {
		return "", "", err
	}
	// ctx 感知读（与监听侧 WaitOffer 对称）：对端信令端口可达但不回 answer 时，
	// ctx 取消（探测超时/用户中断/节点关停）能立即解除阻塞，而非卡满 30s。
	type readResult struct {
		msg directSignalMsg
		err error
	}
	readCh := make(chan readResult, 1)
	go func() {
		var msg directSignalMsg
		err := readDirectSignalFrame(c.conn, &msg)
		readCh <- readResult{msg: msg, err: err}
	}()
	var rr readResult
	select {
	case rr = <-readCh:
	case <-ctx.Done():
		_ = c.conn.Close()
		return "", "", ctx.Err()
	}
	if ctx.Err() != nil {
		// 读与 ctx 同时就绪时优先报 ctx：探测超时/用户中断语义优先于连接级错误
		// （read goroutine 因 conn.Close 解阻返回错误，不应掩盖 ctx 到期）。
		return "", "", ctx.Err()
	}
	_ = c.conn.SetReadDeadline(time.Time{})
	if rr.err != nil {
		return "", "", fmt.Errorf("direct signal: 读取 answer 失败: %w", rr.err)
	}
	if rr.msg.SDP == "" {
		return "", "", errors.New("direct signal: answer 缺 SDP")
	}
	return "", rr.msg.SDP, nil
}

func (c *directSignalerClient) SendAnswer(_ string, _ string) error {
	return errors.New("direct signal: 拨号侧不应发送 answer")
}

func (c *directSignalerClient) WaitOffer(_ context.Context) (string, string, error) {
	return "", "", errors.New("direct signal: 拨号侧不应等待 offer")
}

func (c *directSignalerClient) Close() error { return c.conn.Close() }

// readDirectSignalFrame 读一帧 [4B BE len][JSON]。
func readDirectSignalFrame(r io.Reader, v *directSignalMsg) error {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 || n > directSignalMaxFrame {
		return fmt.Errorf("direct signal: 帧长度非法: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

func writeDirectSignalFrame(w io.Writer, msg directSignalMsg) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	if err := iostream.WriteFull(w, lenBuf[:]); err != nil {
		return err
	}
	return iostream.WriteFull(w, b)
}
