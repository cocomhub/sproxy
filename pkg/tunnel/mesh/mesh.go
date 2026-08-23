// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mesh 提供 mesh 内网穿透的选路/直连/自动注册能力，从 cmd/sclient 抽出。
//
// 依赖 webrtc/ws 等外部传输模块，故作为独立 go.mod 模块存在（replace 指向
// 主仓库与 webrtc/ws 子模块），避免把 pion 等依赖带进主 go.mod。未来 mesh node
// 常驻模式（注册 + 服务宣告 + webrtc listen + relay serve + 信令 poll）可直接
// 复用本包；cmd/sclient 仅是 CLI 前端。
package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
)

const (
	// RegisterAckTimeout 是等待 hub 注册 ACK 的超时。
	RegisterAckTimeout = 10 * time.Second
	// WebRTCProbeTimeout 是 webrtc 直连探测的超时上限（P1-12）：目标仅跑 relay
	// start（不消费信令收件箱）时不再白等 30s 信令超时才回落中继。
	WebRTCProbeTimeout = 10 * time.Second
)

// 路径类型。
const (
	// KindWebRTC 表示直连路径（数据面为 mux 流，已写好拨号帧）。
	KindWebRTC = "webrtc"
	// KindRelay 表示回落 hub 中继路径（hub 的 RelayStreamHandler 已写好拨号帧）。
	KindRelay = "relay"
)

// Result 是一次 mesh 直连的结果：数据面连接 + 实际使用的路径。
type Result struct {
	Conn net.Conn
	Kind string
}

// WriteDialFrame 在任意 io.Writer 上写 [4B len][{"dial":addr}] 帧（与 relay 协议
// 一致），指示出口节点拨目标。net.Conn.Write / mux.Stream.Write 均满足 io.Writer。
func WriteDialFrame(w io.Writer, addr string) error {
	head, err := json.Marshal(hub.DialRequest{Dial: addr})
	if err != nil {
		return err
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if err := iostream.WriteFull(w, lenBuf); err != nil {
		return err
	}
	return iostream.WriteFull(w, head)
}

// HubWSDial 拨号 hub 的 WS 端点；insecure 时跳过证书校验（自签 wss hub 场景）。
// 非 insecure 路径保持 xfer.Get("ws").Dial 原样；insecure 路径走 ws.DialWithOptions
// 注入跳过证书校验的 HTTPClient。
func HubWSDial(ctx context.Context, addr string, insecure bool) (xfer.Conn, error) {
	tp := xfer.Get("ws")
	if tp == nil {
		return nil, fmt.Errorf("ws 传输层未注册")
	}
	if !insecure {
		return tp.Dial(ctx, addr)
	}
	return ws.DialWithOptions(ctx, addr, ws.DialOptions{HTTPClient: client.InsecureHTTPClient()})
}

// MuxStreamConn 把 mux.Stream 适配为 net.Conn（mesh webrtc 直连数据面）。
// Close 关闭整个 mux（连带关闭流与底层 WebRTC 连接）；CloseWrite 向对端传播
// 半关闭（流 EOF），供 pump 的 C1 半关闭收尾路径使用。
type MuxStreamConn struct {
	Stream mux.Stream
	Mux    *mux.Mux
}

func (c *MuxStreamConn) Read(p []byte) (int, error)  { return c.Stream.Read(p) }
func (c *MuxStreamConn) Write(p []byte) (int, error) { return c.Stream.Write(p) }
func (c *MuxStreamConn) Close() error                { return c.Mux.Close() }
func (c *MuxStreamConn) CloseWrite() error           { return c.Stream.CloseWrite() }

// MuxStreamAddr 是 MuxStreamConn 的地址类型。
type MuxStreamAddr struct{}

func (MuxStreamAddr) Network() string { return "mux" }
func (MuxStreamAddr) String() string  { return "mux" }

func (c *MuxStreamConn) LocalAddr() net.Addr                { return MuxStreamAddr{} }
func (c *MuxStreamConn) RemoteAddr() net.Addr               { return MuxStreamAddr{} }
func (c *MuxStreamConn) SetDeadline(_ time.Time) error      { return nil }
func (c *MuxStreamConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *MuxStreamConn) SetWriteDeadline(_ time.Time) error { return nil }

// WebRTCStream 在已建立的 WebRTC 直连上打开 mux 流并写好拨号帧（P0-1 修复）。
//
// 协议对齐 p2p connect：数据面必须经 mux 分帧。mesh connect 曾把
// [4B len][{"dial":addr}] 拨号帧以裸字节写在 DataChannel 上，而对端 p2p listen
// 用 mux.New(webrtc.ConnAsXfer) 按帧消费——帧协议载体错位，直连数据面 100% 失败
// （对端 readLoop 报 frame length mismatch 后拆会话）。这里先 mux.New 包装，再
// 在流上写拨号帧，对端 relay.Serve 经流读到后出站拨号。
func WebRTCStream(ctx context.Context, conn *webrtc.Conn, addr string) (*Result, error) {
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	stream, err := m.Open(ctx)
	if err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("打开 webrtc mux 流失败: %w", err)
	}
	if err := WriteDialFrame(stream, addr); err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("写 webrtc 拨号帧失败: %w", err)
	}
	return &Result{Conn: &MuxStreamConn{Stream: stream, Mux: m}, Kind: KindWebRTC}, nil
}

// Dial 是默认选路：webrtc 打洞优先，失败回落 hub 中继。signaler 为经 hub 信令桥
// 的 *hub.HubSignaler（实现 webrtc.Signaler）；nil 时直接走中继。
func Dial(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, _ string) (*Result, error) {
	// webrtc 打洞优先（数据面直连，不经过 hub）。
	if signaler != nil && target.Node != "" {
		// ctx 预检：已取消则不触发 webrtc（避免无谓地启动 PeerConnection / STUN gathering）。
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// P1-12：探测受 WebRTCProbeTimeout 约束；直连建立后用完整 ctx 开 mux 流。
		probeCtx, probeCancel := context.WithTimeout(ctx, WebRTCProbeTimeout)
		conn, err := webrtc.DialWithSignalerCtx(probeCtx, target.Node, signaler)
		probeCancel()
		if err == nil {
			res, serr := WebRTCStream(ctx, conn, target.Addr)
			if serr != nil {
				// 直连已建立但 mux 流打开/拨号帧写入失败：关闭直连，回落中继。
				_ = conn.Close()
				slog.Debug("webrtc 直连 mux 流建立失败，回落 hub 中继", "error", serr, "target_node", target.Node)
			} else {
				return res, nil
			}
		}
		if ctx.Err() != nil {
			// ctx 取消（用户中断/命令超时）：不再尝试中继，直接返回。
			return nil, ctx.Err()
		}
		// 打洞失败回落中继（S57：不静默吞掉诊断，--verbose 下可见）。
		slog.Debug("webrtc 打洞失败，回落 hub 中继", "error", err, "target_node", target.Node)
	}
	conn, err := svc.RelayStream(ctx, target.Node, target.Addr)
	if err != nil {
		return nil, err
	}
	return &Result{Conn: conn, Kind: KindRelay}, nil
}

// AutoRegisterParams 是一次自动注册（mesh/p2p 信令前置）的参数。
type AutoRegisterParams struct {
	// HubURL 是 hub 地址（空时回落 ServerURL）。
	HubURL string
	// ServerURL 是 HubURL 为空的回退基址（mesh 用 svc.ServerURL()；p2p 传 ""）。
	ServerURL string
	// RelayToken 是注册用 token（hub relay_token）。
	RelayToken string
	// SignalToken 是信令 Bearer（hub auth_token）。
	SignalToken string
	// NodeID 是节点 ID 基础（为空回落主机名）。
	NodeID string
	// Prefix 是临时 node 前缀："mesh" | "p2p"。
	Prefix string
	// ExactNode true=注册成 NodeID 原样（p2p listen 的被寻址方需稳定 ID）；
	// false=临时 nodeID（<prefix>-<base>-<unixnano>）。
	ExactNode bool
	// Insecure 注册 WS 拨号 + HubSignaler HTTP 跳过证书校验（自签 wss hub）。
	Insecure bool
}

// TempRegistration 是一次信令前置的临时注册（生命周期与本次命令绑定）。
type TempRegistration struct {
	Signaler *hub.HubSignaler // 携带临时 node_id + per-node secret
	Closer   func() error     // 关闭注册连接 → hub 移除临时节点
	TempNode string           // 临时节点 ID（调试/日志用）
}

// AutoRegister 是 mesh/p2p 共用的信令自动注册：声明 per-node-secret 能力，从
// REG_OK:<secret> 解析 per-node secret，构建携带 secret 的 HubSignaler，供 webrtc
// 信令身份校验（B3 服务端对未声明/不匹配 secret 的信令 fail-closed 返回 403）。
//
// 注册连接用 mux.New 保活（自动跑 readLoop/pingLoop 处理心跳）；Closer 关闭 mux
// → 底层 WS → hub RemoveIfOwned 移除节点。
func AutoRegister(ctx context.Context, p AutoRegisterParams) (*TempRegistration, error) {
	httpBase, wsURL, err := hub.NormalizeEndpoints(p.HubURL, p.ServerURL)
	if err != nil {
		return nil, err
	}
	base := p.NodeID
	if base == "" {
		base = iostream.LocalHostname("mesh-node")
	}
	nodeID := base
	if !p.ExactNode {
		nodeID = fmt.Sprintf("%s-%s-%d", p.Prefix, base, time.Now().UnixNano())
	}

	conn, err := HubWSDial(ctx, wsURL, p.Insecure)
	if err != nil {
		return nil, fmt.Errorf("连接 Hub 注册端点失败: %w", err)
	}
	// 注册帧：声明 per-node-secret 能力，hub 回 REG_OK:<secret>（B1）。
	if err := conn.Send(ctx, hub.NewRegisterFrame(nodeID, p.RelayToken, hub.Meta{}, hub.CapabilityPerNodeSecret)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("发送注册帧失败: %w", err)
	}
	ackCtx, ackCancel := context.WithTimeout(ctx, RegisterAckTimeout)
	ack, ackErr := conn.Receive(ackCtx)
	ackCancel()
	if ackErr != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("等待注册 ACK 失败: %w", ackErr)
	}
	secret, ackErr := hub.ParseRegisterAck(string(ack))
	if ackErr != nil {
		_ = conn.Close()
		return nil, ackErr
	}
	if secret == "" {
		_ = conn.Close()
		return nil, fmt.Errorf("hub 未下发 per-node secret（未声明能力或能力不被支持）")
	}
	// mux 保活：自动跑 readLoop/writeLoop/pingLoop 处理心跳，注册连接存活到命令退出。
	m := mux.New(conn, mux.RoleListener)
	signaler := hub.NewHubSignaler(httpBase, p.SignalToken, nodeID, secret)
	signaler.SetContext(ctx)
	if p.Insecure {
		signaler.SetHTTPClient(client.InsecureHTTPClient())
	}
	return &TempRegistration{
		Signaler: signaler,
		Closer:   func() error { return m.Close() },
		TempNode: nodeID,
	}, nil
}
