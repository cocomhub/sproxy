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
	"log/slog"
	"net"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// GatewayDefaultAddr 是 mesh node 本地网关的默认监听地址（loopback 安全默认，
// 避免 LAN/防火墙暴露；同机多 mesh node 时可用 127.0.0.1:0 随机端口）。
const GatewayDefaultAddr = "127.0.0.1:18085"

// gatewayMaxRequestBytes 是网关请求/应答帧的长度上限。
const gatewayMaxRequestBytes = 4096

// gatewayRequestTimeout 是网关读取客户端请求帧的超时（连接/泵送阶段不设 deadline）。
const gatewayRequestTimeout = 15 * time.Second

// ErrNoPeerLink 表示本地 mesh node 尚无到目标节点的已建立直连链路。
// mesh connect --gateway 收到该错误时回落常规拨号（webrtc 打洞 / hub 中继）。
var ErrNoPeerLink = errors.New("mesh: 本地节点尚无到目标节点的已建立直连链路")

// KindPeerLink 表示复用已建立直连链路的数据面路径（mesh connect --gateway 路由）。
const KindPeerLink = "peer-link"

// 网关应答错误码（服务端与客户端共享，客户端据此区分 ErrNoPeerLink 与其他失败）。
const (
	gatewayErrBadRequest = "bad_request"
	gatewayErrNoPeerLink = "no_peer_link"
	gatewayErrOpenStream = "open_stream"
	gatewayErrDialFrame  = "dial_frame"
)

// gatewayRequest 是本地网关路由请求（[4B BE len][JSON] 帧，与拨号帧同款长度前缀）。
type gatewayRequest struct {
	// Peer 是目标 mesh node 的稳定 node-id（connect）。
	Peer string `json:"peer"`
	// Addr 是目标服务地址（connect；须等于对端宣告的服务地址，供出口拨号策略精确放行）。
	Addr string `json:"addr"`
	// Status true 时查询节点拓扑（node_id + services + 已建直连链路），不走 connect。
	Status bool `json:"status"`
}

// gatewayAck 是网关对 connect 请求的应答帧。
// 成功（ok=true）后连接直接进入数据面泵送；失败回 error 码 + message。
type gatewayAck struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
	Message string `json:"message,omitempty"`
}

// GatewayPeer 描述一条已建立的对等直连链路（mesh status --gateway 拓扑可观测）。
type GatewayPeer struct {
	// Peer 是对端 mesh node 的稳定 node-id。
	Peer string `json:"peer"`
	// Link 是链路类型（当前恒为 "webrtc-direct"——自动对等发现的打洞直连）。
	Link string `json:"link"`
	// Since 是链路建立时间（RFC3339）。
	Since time.Time `json:"since"`
}

// GatewayStatus 是网关拓扑查询的应答（mesh status --gateway 使用）。
type GatewayStatus struct {
	// NodeID 是本 mesh node 的稳定 node-id。
	NodeID string `json:"node_id"`
	// Services 是本节点宣告到 hub 的服务。
	Services []hub.Service `json:"services"`
	// Peers 是本节点已建立的对等直连链路（全节点互联拓扑）。
	Peers []GatewayPeer `json:"peers"`
}

// Gateway 是 mesh node 本地网关：把对等直连链路（linkPool）暴露为 loopback 端口，
// mesh connect --gateway 复用已建立链路路由到目标节点服务；同时提供拓扑状态查询。
//
// 安全边界：仅监听 loopback（NormalizeListenAddr 归一裸端口），数据面仍由对端
// 出口拨号策略（NewServiceDialPolicy）把守——网关本身不做地址授权，只转发
// 调用方请求的 (peer, addr) 到已建链路上的拨号帧。
type Gateway struct {
	links    *linkPool
	nodeID   string
	services []hub.Service
	logger   *slog.Logger
}

// newGateway 构造网关。cfg 提供 node-id 与服务宣告（状态查询）；links 是已建链路池。
func newGateway(links *linkPool, cfg NodeConfig, logger *slog.Logger) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	return &Gateway{links: links, nodeID: cfg.NodeID, services: cfg.Services, logger: logger}
}

// Serve 启动网关 accept 循环（独立 goroutine，ctx 取消关闭 listener）。返回实际监听
// 地址（配置 addr 被占时回落 127.0.0.1:0 随机端口并 Warn，不终止 mesh node）。
func (g *Gateway) Serve(ctx context.Context, addr string) (string, error) {
	if addr == "" {
		addr = GatewayDefaultAddr
	}
	addr = iostream.NormalizeListenAddr(addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		g.logger.Warn("mesh 本地网关默认端口被占，回落随机端口", "addr", addr, "error", err)
		ln, err = net.Listen("tcp", iostream.NormalizeListenAddr("127.0.0.1:0"))
		if err != nil {
			return "", fmt.Errorf("mesh 本地网关监听失败: %w", err)
		}
	}
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				if ctx.Err() != nil {
					return
				}
				g.logger.Warn("mesh 本地网关 accept 失败", "error", aerr)
				continue
			}
			go func(cc net.Conn) { g.handleConn(ctx, cc) }(c)
		}
	}()
	return ln.Addr().String(), nil
}

// handleConn 处理一条网关连接：先读一帧请求（connect 或 status），然后按类型响应。
func (g *Gateway) handleConn(ctx context.Context, c net.Conn) {
	defer c.Close()
	// 请求帧读取阶段设 deadline：恶意/半开连接不长期占用 goroutine。
	if err := c.SetDeadline(time.Now().Add(gatewayRequestTimeout)); err != nil {
		return
	}
	var req gatewayRequest
	if err := readGatewayFrame(c, &req); err != nil {
		g.logger.Warn("mesh 本地网关读取请求失败", "error", err)
		return
	}
	// 连接/泵送阶段取消 deadline（数据面由 Pump 的宽限期与底层链路生命周期约束）。
	_ = c.SetDeadline(time.Time{})

	if req.Status {
		g.writeStatus(c)
		return
	}
	if req.Peer == "" || req.Addr == "" {
		_ = writeGatewayFrame(c, gatewayAck{OK: false, Error: gatewayErrBadRequest, Message: "peer 与 addr 必填"})
		return
	}
	m, ok := g.links.get(req.Peer)
	if !ok {
		_ = writeGatewayFrame(c, gatewayAck{OK: false, Error: gatewayErrNoPeerLink, Message: ErrNoPeerLink.Error()})
		return
	}
	stream, err := m.Open(ctx)
	if err != nil {
		_ = writeGatewayFrame(c, gatewayAck{OK: false, Error: gatewayErrOpenStream, Message: err.Error()})
		return
	}
	defer func() { _ = stream.Abort() }() // P0-3：收尾用 Abort（非阻塞），避免 writeCh 满时 Close 永久阻塞
	if err := WriteDialFrame(stream, req.Addr); err != nil {
		_ = writeGatewayFrame(c, gatewayAck{OK: false, Error: gatewayErrDialFrame, Message: err.Error()})
		return
	}
	// 先写 ok 应答再进入泵送：客户端读到 ok 后把连接视为原始数据流（无帧边界）。
	if err := writeGatewayFrame(c, gatewayAck{OK: true}); err != nil {
		return
	}
	// 双向泵送（C1 半关闭：本地客户端 CloseWrite → 对端流 EOF → 服务读尾；反之亦然）。
	iostream.Pump(c, stream, iostream.PumpGrace)
}

// writeStatus 回写拓扑状态帧（connect 未失败路径：node_id + services + 已建链路）。
func (g *Gateway) writeStatus(c net.Conn) {
	st := GatewayStatus{NodeID: g.nodeID, Services: g.services, Peers: g.toGatewayPeers(g.links.snapshot())}
	_ = writeGatewayFrame(c, &st)
}

func (g *Gateway) toGatewayPeers(in []peerLinkInfo) []GatewayPeer {
	out := make([]GatewayPeer, 0, len(in))
	for _, p := range in {
		out = append(out, GatewayPeer(p))
	}
	return out
}

// GatewayConnect 经本地 mesh node 网关复用已建立的直连链路路由到目标服务。
// addr 是网关地址（127.0.0.1:port）；peer/targetAddr 是目标节点与宣告的服务地址。
// 返回的连接为原始数据流（网关已写拨号帧，对端 relay.Serve 已接上目标服务）。
// 本地节点无到 peer 的已建链路时返回 ErrNoPeerLink（调用方回落常规拨号）。
func GatewayConnect(ctx context.Context, addr, peer, targetAddr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mesh 网关连接失败: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	if err := writeGatewayFrame(conn, gatewayRequest{Peer: peer, Addr: targetAddr}); err != nil {
		return nil, fmt.Errorf("mesh 网关写入请求失败: %w", err)
	}
	var ack gatewayAck
	if err := readGatewayFrame(conn, &ack); err != nil {
		return nil, fmt.Errorf("mesh 网关读取应答失败: %w", err)
	}
	if !ack.OK {
		if ack.Error == gatewayErrNoPeerLink {
			return nil, ErrNoPeerLink
		}
		msg := ack.Message
		if msg == "" {
			msg = ack.Error
		}
		return nil, fmt.Errorf("mesh 网关路由失败: %s", msg)
	}
	ok = true
	return conn, nil
}

// QueryGatewayStatus 查询本地 mesh node 网关的拓扑状态（node-id + 服务宣告 + 已建链路）。
func QueryGatewayStatus(ctx context.Context, addr string) (*GatewayStatus, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("mesh 网关连接失败: %w", err)
	}
	defer conn.Close()
	if err := writeGatewayFrame(conn, gatewayRequest{Status: true}); err != nil {
		return nil, fmt.Errorf("mesh 网关写入状态请求失败: %w", err)
	}
	var st GatewayStatus
	if err := readGatewayFrame(conn, &st); err != nil {
		return nil, fmt.Errorf("mesh 网关读取状态失败: %w", err)
	}
	return &st, nil
}

// readGatewayFrame 读取一帧 [4B BE len][JSON]（长度有界，防止超大帧占用内存）。
func readGatewayFrame(r io.Reader, v any) error {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(r, lenBuf); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(lenBuf)
	if n == 0 || n > gatewayMaxRequestBytes {
		return fmt.Errorf("网关帧长度非法: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// writeGatewayFrame 写一帧 [4B BE len][JSON]（写满循环处理短写）。
func writeGatewayFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(b)))
	if err := iostream.WriteFull(w, lenBuf); err != nil {
		return err
	}
	return iostream.WriteFull(w, b)
}
