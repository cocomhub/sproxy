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
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
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
	// KindViaNode 表示经中间节点 X 中转路径（X 出站拨号到目标）。
	KindViaNode = "via-node"
)

// Result 是一次 mesh 直连的结果：数据面连接 + 实际使用的路径。
type Result struct {
	Conn net.Conn
	Kind string
	// Latency 是建连耗时（端到端 RTT 近似：发起 → 拨号 ack 首字节可读；
	// 含打洞/中继/多跳各段网络往返）。SmartDial 竞速时填充；单路径 Dial 为 0。
	Latency time.Duration
	// EndToEnd 标记本次连接启用了端到端加密（DialE2E/ServeE2E 建隧道，
	// 数据面密文、中间节点 X 读不到明文）。非端到端路径为 false。
	EndToEnd bool
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
// upgradeHeader 非空时发送 X-WebSocket-Profile 附加校验头（与服务端 WithUpgradeHeader 一致才连通）。
func HubWSDial(ctx context.Context, addr string, insecure bool, upgradeHeader string) (xfer.Conn, error) {
	tp := xfer.Get("ws")
	if tp == nil {
		return nil, fmt.Errorf("ws 传输层未注册")
	}
	if !insecure && upgradeHeader == "" {
		return tp.Dial(ctx, addr)
	}
	opts := ws.DialOptions{}
	if insecure {
		opts.HTTPClient = client.InsecureHTTPClient()
	}
	if upgradeHeader != "" {
		opts.UpgradeHeader = upgradeHeader
	}
	return ws.DialWithOptions(ctx, addr, opts)
}

// HubWSDialCA 拨号 hub 的 WS 端点，以给定 PEM CA 文件为受信根**严格校验**
// （自签/私有 CA 的 wss hub 场景；替代 --insecure 的安全做法）。
func HubWSDialCA(ctx context.Context, addr, caFile string) (xfer.Conn, error) {
	hc, err := client.CAHTTPClient(caFile)
	if err != nil {
		return nil, fmt.Errorf("加载 CA 文件 %s: %w", caFile, err)
	}
	return ws.DialWithOptions(ctx, addr, ws.DialOptions{HTTPClient: hc})
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
//
// e2eOpts 非 nil（端到端加密）：**跳过普通 dial 帧**（e2e 帧由 DialE2EStream 写，
// 避免帧序冲突——流上先有普通 dial 帧会让对端走裸 pump，e2e 帧被当数据），
// 仅返回裸 MuxStreamConn 供调用方包 DialE2EStream。nil = 现状（写普通拨号帧）。
func WebRTCStream(ctx context.Context, conn *webrtc.Conn, addr string, e2eOpts *EndToEndOptions) (*Result, error) {
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	stream, err := m.Open(ctx)
	if err != nil {
		_ = m.Close()
		return nil, fmt.Errorf("打开 webrtc mux 流失败: %w", err)
	}
	if e2eOpts == nil {
		if err := WriteDialFrame(stream, addr); err != nil {
			_ = m.Close()
			return nil, fmt.Errorf("写 webrtc 拨号帧失败: %w", err)
		}
	}
	return &Result{Conn: &MuxStreamConn{Stream: stream, Mux: m}, Kind: KindWebRTC}, nil
}

// DialOptions 是选路选项（Y 二期：把「载体选择」从调用点收敛成显式参数）。
type DialOptions struct {
	// AllowRelayFallback 为 false 时，WebRTC 打洞失败**即返回错误、不回落中继**
	// ——对应 `transport: webrtc` 的显式语义：用户声明了直连，静默降级会掩盖配置/网络问题；
	// 为 true 时回落中继（`transport: auto` 与 CLI 默认行为）。
	AllowRelayFallback bool
	// ICE 是**实例级** ICE 配置（nil = 包级全局，见 webrtc.ICEOptions 的契约）：
	// server 侧同进程多任务/多租户各带自己的 STUN/TURN 时由此注入。
	ICE *webrtc.ICEOptions
	// E2E 是端到端加密配置（显式开关，nil = 不启用，默认关——用户确认，不静默启用）。
	// 非 nil 时 RelayStream 返回的裸数据面连接包 DialE2EStream（ECDH 握手 + AES-256-GCM），
	// Result.EndToEnd 置 true。WebRTC 直连分支一期不接（打洞后已是 mux，mux-over-mux
	// 留二期 via-node）——L 直连 T 的 hub 中继路径已覆盖核心安全目标。
	E2E *EndToEndOptions
}

// Dial 是默认选路：webrtc 打洞优先，失败回落 hub 中继。
//
// signaler 只需满足 `webrtc.Signaler`（生产：经 hub 信令桥的 `*hub.HubSignaler`；
// 局域网直连：`DirectSignaler`；测试：进程内信令）。**留空（nil）时直接走中继**：
// 类型放宽为接口是为了让「无 hub 的真打洞」可测（否则进程内只验证得了回落路径）。
func Dial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, localNode string) (*Result, error) {
	return DialWithOptions(ctx, svc, signaler, target, localNode, DialOptions{AllowRelayFallback: true})
}

// DialWithOptions 是 Dial 的**可选回落/可选实例 ICE**版本。
//
// 语义（勿放宽）：`AllowRelayFallback=false` 且打洞失败 ⇒ 返回打洞错误（**不**调中继）；
// signaler 为 nil ⇒ 无打洞能力 ⇒ 直接中继（此时回落开关无意义）。
func DialWithOptions(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler, target *client.MeshService, _ string, opts DialOptions) (*Result, error) {
	// webrtc 打洞优先（数据面直连，不经过 hub）。
	if SignalerUsable(signaler) && target.Node != "" {
		// ctx 预检：已取消则不触发 webrtc（避免无谓地启动 PeerConnection / STUN gathering）。
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := DialWebRTC(ctx, signaler, target, opts.ICE, opts.E2E)
		if err == nil {
			return &Result{Conn: conn, Kind: KindWebRTC, EndToEnd: opts.E2E != nil}, nil
		}
		if ctx.Err() != nil {
			// ctx 取消（用户中断/命令超时）：不再尝试中继，直接返回。
			return nil, ctx.Err()
		}
		if !opts.AllowRelayFallback {
			// 显式直连语义：失败即失败（带上下文，便于排障），绝不静默降级。
			return nil, fmt.Errorf("webrtc 直连失败（transport 未允许回落中继）: %w", err)
		}
		// 打洞失败回落中继（S57：不静默吞掉诊断，--verbose 下可见）。
		slog.Debug("webrtc 打洞失败，回落 hub 中继", "error", err, "target_node", target.Node)
	}
	// 经 hub 中继到目标节点：统一走 dialRelay（含 E2E 分支——RelayStreamE2E +
	// DialE2EHandshake，hub 写 e2e 首帧）。与 SmartDial 竞速的 relay 候选共用
	// 单一实现（避免多套入口漂移/漏 E2E）。
	return dialRelay(ctx, svc, target, opts)
}

// DialWebRTC 只做 **WebRTC 直连**：打洞（受 WebRTCProbeTimeout 约束）→ 在直连上开 mux 流并
// 写拨号帧（由对端 relay 出口拨号到 target.Addr）。失败即返回错误，**不回落中继**。
//
// 抽成独立函数的原因：`DialWithOptions` 与 `remote.Dialer` 实现（remote_dialer.go）都要用它
// ——打洞细节（探测超时、mux 流建立失败即关连接）只应有一处实现。
// ice 为实例级 ICE 配置（nil = 包级全局）。
func DialWebRTC(ctx context.Context, signaler webrtc.Signaler, target *client.MeshService, ice *webrtc.ICEOptions, e2eOpts *EndToEndOptions) (net.Conn, error) {
	// P1-12：探测受 WebRTCProbeTimeout 约束；直连建立后用完整 ctx 开 mux 流。
	probeCtx, probeCancel := context.WithTimeout(ctx, WebRTCProbeTimeout)
	conn, err := webrtc.DialWithSignalerOptsCtx(probeCtx, target.Node, signaler, ice)
	probeCancel()
	if err != nil {
		return nil, err
	}
	res, serr := WebRTCStream(ctx, conn, target.Addr, e2eOpts)
	if serr != nil {
		// 直连已建立但 mux 流打开/拨号帧写入失败：关闭直连并返回错误（由调用方决定是否回落）。
		_ = conn.Close()
		return nil, fmt.Errorf("webrtc 直连 mux 流建立失败: %w", serr)
	}
	// 端到端加密（e2eOpts 非 nil）：WebRTCStream 已跳过普通 dial 帧，此处包
	// DialE2EStream（在 mux 流上写 e2e 帧 + ECDH 握手 + AES-256-GCM 字节流）——
	// L⇄T 直连路径端到端加密（T 侧 relay.Serve 的 E2EServe 据此解密）。
	// Path 空 = L 直连 T（T 是最终目标，解密而非透传）。
	if e2eOpts != nil {
		e2eConn, derr := DialE2EStream(ctx, res.Conn, target.Addr, "", *e2eOpts)
		if derr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("webrtc 直连 E2E 拨号失败: %w", derr)
		}
		return e2eConn, nil
	}
	return res.Conn, nil
}

// OpenUDPMux 经 mesh 到出口节点建立 UDP 端口映射（sclient udp map）：建立 webrtc
// mux 连接，开一条控制流写 [4B len][{"udp": remote}] 帧（出口 relay 据此把该 mux
// 作为 UDP 数据报通道），返回 mux + 控制流（供 SendDatagram / SetDatagramHandler）。
//
// 仅支持 webrtc 直连（hub 信令 / mDNS 直连信令）；hub 中继回落暂不支持 UDP。
// signaler 由调用方建立（AutoRegister 的 HubSignaler 或 DialDirectSignaler）。
func OpenUDPMux(ctx context.Context, signaler webrtc.Signaler, targetNode, udpAddr string) (*mux.Mux, mux.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, WebRTCProbeTimeout)
	conn, err := webrtc.DialWithSignalerCtx(probeCtx, targetNode, signaler)
	probeCancel()
	if err != nil {
		return nil, nil, err
	}
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	stream, err := m.Open(ctx)
	if err != nil {
		_ = m.Close()
		return nil, nil, fmt.Errorf("打开 UDP 控制流失败: %w", err)
	}
	// 控制帧写入受 ctx 约束（对端停读时 stream.Write 可能阻塞，Ctrl+C 需能打断）。
	writeDone := make(chan error, 1)
	go func() { writeDone <- WriteUDPControl(stream, udpAddr) }()
	select {
	case err := <-writeDone:
		if err != nil {
			_ = m.Close()
			return nil, nil, fmt.Errorf("写 UDP 映射帧失败: %w", err)
		}
	case <-ctx.Done():
		_ = m.Close()
		return nil, nil, ctx.Err()
	}
	return m, stream, nil
}

// WriteUDPControl 写 [4B len][{"udp": addr}] 控制帧（指示出口把该 mux 作为 UDP 通道）。
func WriteUDPControl(w io.Writer, udpAddr string) error {
	head, err := json.Marshal(hub.UDPRequest{UDP: udpAddr})
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

// DialDirect 经给定信令器直连目标（mDNS 无 hub 场景）：无 hub 中继回落，仅走
// webrtc 打洞直连。signaler 为直连信令器（DialDirectSignaler 的返回，已连到对端
// 广播的信令端点）或任何实现 webrtc.Signaler 的通道；target 由 mDNS 服务解析获得。
// 拨号成功后调用方负责 Close signaler（信令握手已结束，数据面独立）。
func DialDirect(ctx context.Context, signaler webrtc.Signaler, target *client.MeshService) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, WebRTCProbeTimeout)
	conn, err := webrtc.DialWithSignalerCtx(probeCtx, target.Node, signaler)
	probeCancel()
	if err != nil {
		return nil, err
	}
	return WebRTCStream(ctx, conn, target.Addr, nil)
}

// AutoRegisterParams 是一次自动注册（mesh/p2p 信令前置）的参数。
type AutoRegisterParams struct {
	// HubURL 是 hub 地址（空时回落 ServerURL）。
	HubURL string
	// ServerURL 是 HubURL 为空的回退基址（mesh 用 svc.ServerURL()；p2p 传 ""）。
	ServerURL string
	// AccessKey 是 SproxySig 请求签名认证的 AccessKey（信令/节点列表/hub 注册准入）。
	AccessKey string
	// AccessKeySecret 是 SproxySig AccessKeySecret（本地密钥，仅计算签名与注册证明，永不上线）。
	AccessKeySecret string
	// AccessKeyID 是 SproxySig SK 条目 ID（skey-id，v2 协议必传——信令/节点列表签名用）。
	AccessKeyID string
	// NodeID 是节点 ID 基础（为空回落主机名）。
	NodeID string
	// Prefix 是临时 node 前缀："mesh" | "p2p"。
	Prefix string
	// ExactNode true=注册成 NodeID 原样（p2p listen 的被寻址方需稳定 ID）；
	// false=临时 nodeID（<prefix>-<base>-<unixnano>）。
	ExactNode bool
	// Insecure 注册 WS 拨号 + HubSignaler HTTP 跳过证书校验（自签 wss hub）。
	Insecure bool
	// CAFile 是 hub 的 TLS 受信 CA 文件路径（PEM）。非空时用该 CA 构建专属证书池**严格
	// 校验**（InsecureSkipVerify=false）注册 WS + 信令 HTTP——自签/私有 CA 的安全做法
	// （对齐 hub/federation 的 peer TLS 前例）；与 Insecure 互斥由调用方保证。
	CAFile string
	// UpgradeHeader 是 WS 升级附加校验头值（形态对齐 §5.3；与 hub.transports.ws.upgrade_header
	// 一致才连通）。空 = 不发送（默认 /ws 标准零回归）。
	UpgradeHeader string
	// Services 是宣告到 hub 的服务（mesh node 常驻用；mesh/p2p 拨号方不传）。
	// 进注册帧 Meta.Services，供 mesh connect 服务发现与选路。
	Services []hub.Service
	// Tags 是节点标签（如 ["exit"] 表示出口节点；mesh node --dial-allow 时打）。
	Tags []string
	// RealNodeID 是 mesh discovery 临时注册（Prefix:"disc"）代表的本节点真实 node-id。
	// hub 注册时强制校验（base==RealNodeID 且 RealNodeProof 有效），防冒充他人污染
	// 对端链路池。mesh connect/p2p 拨号方不传。
	RealNodeID string
	// RealNodeProof 是 HMAC-SHA256(本节点 per-node secret, RealNodeID) 的 hex。
	// 本节点 per-node secret 来自自身常驻注册（runNodeOnce 的 reg.Secret）。
	RealNodeProof string
}

// TempRegistration 是一次信令前置的临时注册（生命周期与本次命令绑定）。
type TempRegistration struct {
	Signaler *hub.HubSignaler // 携带临时 node_id + per-node secret
	Closer   func() error     // 关闭注册连接 → hub 移除临时节点
	TempNode string           // 临时节点 ID（调试/日志用）
	// Secret 是本临时节点的 per-node secret（mesh node 常驻注册用它派生 discovery
	// 拨号的 real_node_proof；供同一进程内派生 HMAC 证明）。
	Secret string
	// VirtualIP 是本节点虚拟 IP（hub 权威分配；仅稳定常驻注册 ExactNode=true 时
	// 由 REG_OK 下发，临时拨号身份/旧 hub 为无效 Addr）。mesh node 用它装配出口
	// 虚拟 IP NAT 拨号策略（NewVirtualIPDialPolicy 的 selfVIP）。
	VirtualIP netip.Addr
	// Mux 是注册连接上的 mux（RoleListener）：mesh node 在其上 relay.Serve 接受
	// 经 hub 的中继流；p2p/mesh connect 拨号方不消费。
	Mux *mux.Mux
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
		// 临时 node-id：<prefix>-<base>-<随机 hex>。用随机后缀而非仅 unixnano——
		// 并发拨号（discovery 并行）下 UnixNano 可能碰撞，导致对端 Answer 交叉路由
		// （node-a 拨 b/c 的临时身份若相同，b/c 的 Answer 会互相串到对方 inbox）。
		nodeID = fmt.Sprintf("%s-%s-%s", p.Prefix, base, newTempSuffix())
	}

	// 注册准入：hub 已废除共享 token，改用 SproxySig AccessKey + HMAC proof
	// （hub.ComputeRegisterProof 绑定 nodeID + ts/nonce，防串用/重放）。
	// fail-closed：AccessKeySecret 为空时直接报错（防止无凭据注册被 hub fail-closed
	// 拒绝后客户端困惑——明明连上了却被拒）。
	if p.AccessKeySecret == "" {
		return nil, fmt.Errorf("register: access_key_secret 为空，无法计算注册 proof")
	}
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, err := hub.ComputeRegisterProof(p.AccessKeySecret, nodeID, ts, nonce)
	if err != nil {
		return nil, fmt.Errorf("register: 计算注册证明失败: %w", err)
	}

	// 注册 WS 拨号：CAFile 非空 → CA 严格校验；否则按 insecure 语义（insecure=false 走系统根池）。
	var conn xfer.Conn
	var signalerHTTP *http.Client
	if p.CAFile != "" {
		conn, err = HubWSDialCA(ctx, wsURL, p.CAFile)
		if err != nil {
			return nil, fmt.Errorf("连接 Hub 注册端点失败（CA）: %w", err)
		}
		signalerHTTP, err = client.CAHTTPClient(p.CAFile)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("加载 CA 文件 %s: %w", p.CAFile, err)
		}
	} else {
		conn, err = HubWSDial(ctx, wsURL, p.Insecure, p.UpgradeHeader)
		if err != nil {
			return nil, fmt.Errorf("连接 Hub 注册端点失败: %w", err)
		}
	}
	// 注册帧：声明 per-node-secret 能力（hub 回 REG_OK:<secret>，B1）与 virtual-ip
	// 能力（hub 回 REG_OK:<secret>:<vip>，本节点虚拟 IP；不感知该能力的旧 hub 忽略
	// 未知能力位，回旧格式），并携带服务宣告（mesh node 常驻）与标签（如 exit）。
	// mesh discovery 临时注册（disc-）另带 real_node_id + real_node_proof（hub 强制
	// 校验防冒充）。mesh/p2p 拨号方不传则 Meta{}。
	if err := conn.Send(ctx, hub.NewRegisterFrame(nodeID, p.AccessKey, proof, ts, nonce, hub.Meta{Services: p.Services, Tags: p.Tags, RealNodeID: p.RealNodeID, RealNodeProof: p.RealNodeProof}, hub.CapabilityPerNodeSecret, hub.CapabilityVirtualIP)); err != nil {
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
	ackFull, ackErr := hub.ParseRegisterAckFull(string(ack))
	if ackErr != nil {
		_ = conn.Close()
		return nil, ackErr
	}
	if ackFull.Secret == "" {
		_ = conn.Close()
		return nil, fmt.Errorf("hub 未下发 per-node secret（未声明能力或能力不被支持）")
	}
	// mux 保活：自动跑 readLoop/writeLoop/pingLoop 处理心跳，注册连接存活到命令退出。
	m := mux.New(conn, mux.RoleListener)
	signaler := hub.NewHubSignaler(httpBase, p.AccessKey, nodeID, ackFull.Secret)
	signaler.SetAccessKeySecret(p.AccessKeySecret)
	signaler.SetAccessKeyID(p.AccessKeyID)
	signaler.SetContext(ctx)
	if p.CAFile != "" {
		// CA 严格校验：注册 WS 走 HubWSDialCA（上方已拨号），信令 HTTP 注入带 RootCAs 的 client。
		if signalerHTTP != nil {
			signaler.SetHTTPClient(signalerHTTP)
		}
	} else if p.Insecure {
		signaler.SetHTTPClient(client.InsecureHTTPClient())
	}
	return &TempRegistration{
		Signaler:  signaler,
		Closer:    func() error { return m.Close() },
		TempNode:  nodeID,
		Secret:    ackFull.Secret,
		VirtualIP: ackFull.VirtualIP,
		Mux:       m,
	}, nil
}

// newTempSuffix 生成临时 node-id 的随机后缀（8B hex）。随机而非仅时间戳——
// 并发拨号下 UnixNano 可能碰撞，导致对端 Answer 交叉路由。
func newTempSuffix() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
