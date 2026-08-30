// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"crypto/hmac"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置 TCP 传输层（裸 TCP 中继）
)

// registerFrameTTL 是注册帧等待的超时时间。
const registerFrameTTL = 10 * time.Second

// maxRegisterFrameBytes 是注册帧的最大字节数（防恶意大帧耗尽内存）。
// 注意：conn.Receive 已完整缓冲整条消息后才做此处检查，真实内存上界由传输层
// maxMessageBytes 决定（如 WS 为 1MiB，见 ext/ws/ws.go）；此常量是注册帧专项的
// 收紧阈值（纵深防御），并非内存上界本身（S3）。
const maxRegisterFrameBytes = 64 << 10 // 64 KiB

// 注册 ACK 帧常量（xfer 层一条消息）。导出供 sclient 注册后等待。
// 节点声明 per-node-secret 能力（见 CapabilityPerNodeSecret）时，REG_OK 携带
// 独立 secret，线上格式为 "REG_OK:<base64url secret>"；未声明时为纯 "REG_OK"。
const (
	RegisterAckOK  = "REG_OK"
	RegisterAckErr = "REG_ERR:"
)

// registerAckSecretSep 是 REG_OK 中 secret 字段的分隔符。
const registerAckSecretSep = ":"

// CapabilityPerNodeSecret 是节点声明"希望获得 per-node 独立 secret"的能力标志。
// hub 注册成功后生成 32B 随机 secret 存入 NodeInfo.Secret 并随 REG_OK 下发，
// 供后续批次（B3 服务端校验 / B2 客户端携带）的信令身份校验使用（I1）。
const CapabilityPerNodeSecret = "per-node-secret"

// RegisterFrame 是节点连接后的注册帧（JSON）。
// 向后兼容：若首个流上收到的是非 JSON 裸字符串（旧版仅发 nodeID），
// 则等价于仅携带 NodeID 且无 token 的注册帧。
//
// Capabilities 是节点声明的能力标志列表（可扩展：未来新增能力直接追加
// 字符串常量，hub 端用 hasCapability 判断，旧 hub/旧客户端忽略未知项）。
type RegisterFrame struct {
	NodeID string `json:"node_id"`
	// Token 已废弃：不再用于准入（保留字段避免破坏旧客户端 JSON）。
	Token string `json:"token,omitempty"`
	// AccessKey 是 SproxySig 准入 AccessKey（与 access_keys 配置一致）。
	AccessKey string `json:"access_key,omitempty"`
	// AccessKeyProof 是 ComputeRegisterProof 输出（HMAC-SHA256 证明持有 SK）。
	AccessKeyProof string `json:"access_key_proof,omitempty"`
	// TS / Nonce 是注册证明的防重放字段（M-6）：TS 为 unix 毫秒、Nonce 为一次性随机串，
	// 均参与 ComputeRegisterProof 签名；hub 校验 TS 新鲜度 + nonce 去重。
	TS           int64    `json:"ts,omitempty"`
	Nonce        string   `json:"nonce,omitempty"`
	Meta         Meta     `json:"meta"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// Meta 是节点注册时宣告的附加信息，供 mesh 选路使用。
type Meta struct {
	Addr     string    `json:"addr,omitempty"`     // 节点自身可达地址（直连地址，可为空）
	Services []Service `json:"services,omitempty"` // 本地服务宣告（本地监听或出口可达目标）
	Tags     []string  `json:"tags,omitempty"`     // 可选标签（如 "exit"、"trusted"）
	// RealNodeID 是 mesh 自动对等发现临时注册（disc-<base>-<nano>）代表的本节点真实
	// node-id。hub 强制校验（见 registerNode）：base 必须等于 RealNodeID 且持有该真实
	// 节点 per-node secret 的 HMAC 证明（防冒充他人污染链路池）。
	RealNodeID string `json:"real_node_id,omitempty"`
	// RealNodeProof 是 HMAC-SHA256(真实节点 per-node secret, RealNodeID) 的 hex。
	// 仅 mesh node 的 discovery 临时注册携带；hub 端用存储的真实节点 secret 复核。
	RealNodeProof string `json:"real_node_proof,omitempty"`
}

// Service 描述一个可被 mesh 寻址的服务。
type Service struct {
	Name  string `json:"name"`            // 服务名（mesh connect 的目标）
	Addr  string `json:"addr"`            // 服务地址（127.0.0.1:22、或出口节点可达的 192.x.x.x:22）
	Layer string `json:"layer,omitempty"` // 用途标注（debug/info）
}

// ServicesOf 返回指定节点宣告的服务列表。
func (rt *RouteTable) ServicesOf(id NodeID) []Service {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	if rt.services == nil {
		return nil
	}
	svcs := rt.services[id]
	out := make([]Service, len(svcs))
	copy(out, svcs)
	return out
}

// HasService 返回节点是否宣告了指定名称的服务。
func (rt *RouteTable) HasService(id NodeID, name string) bool {
	_, ok := rt.lookupService(id, name)
	return ok
}

// SetServices 记录节点宣告的服务。
func (rt *RouteTable) SetServices(id NodeID, svcs []Service) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.services == nil {
		rt.services = make(map[NodeID][]Service)
	}
	if len(svcs) == 0 {
		delete(rt.services, id)
		return
	}
	rt.services[id] = svcs
}

// ClearServices 清除节点宣告的服务（节点断开时调用）。
func (rt *RouteTable) ClearServices(id NodeID) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.services != nil {
		delete(rt.services, id)
	}
}

// ServiceHosts 返回宣告了指定服务名的节点 ID 列表。
func (rt *RouteTable) ServiceHosts(name string) []NodeID {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	var out []NodeID
	for id, svcs := range rt.services {
		for _, s := range svcs {
			if s.Name == name {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// LookupService 返回宣告了指定服务名的节点与其服务地址。
func (rt *RouteTable) LookupService(name string) (NodeID, Service, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for id, svcs := range rt.services {
		if _, ok := rt.nodes[id]; !ok {
			continue
		}
		for _, s := range svcs {
			if s.Name == name {
				return id, s, true
			}
		}
	}
	return "", Service{}, false
}

func (rt *RouteTable) lookupService(id NodeID, name string) (Service, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	for _, s := range rt.services[id] {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// DialRequest 是发往叶子出口节点的导出结构（JSON），
// portal/relay 收到后向 addr 发起出站连接，随后进入字节中转。
type DialRequest struct {
	Dial string `json:"dial,omitempty"` // 目标叶子出站连接的 TCP 地址
}

// UDPRequest 是 UDP 端口映射控制帧（sclient udp map 首帧）：目标叶子的 UDP 目标
// 地址（host:port）。收到后叶子把该流的 mux 作为 UDP 数据报通道（FrameDatagram）。
type UDPRequest struct {
	UDP string `json:"udp,omitempty"` // 目标叶子出站 UDP 地址
}

// DialResultFrame 是叶子出口拨号结果回帧（I27）。
// hub 的 /api/relay/stream 在写 200 前读取 [4B len][DialResultFrame JSON]，
// 据此决定返回 200（ok）或 502/504（error/超时）。
// 仅当叶子以 ServeOptions.DialResultFrames 模式（relay start 经 hub 中继）运行时
// 回写；webrtc 直连（p2p listen）不回写，避免结果帧污染数据流。
type DialResultFrame struct {
	DialResult string `json:"dial_result"`
	Message    string `json:"message,omitempty"`
}

// DialResultFrame 取值。
const (
	DialResultOK    = "ok"
	DialResultError = "error"
)

// NewRegisterFrame 构建注册帧。当无 meta/ak/proof/ts/nonce/caps 时退化为裸 nodeID，
// 保证与旧版 hub（仅接收裸 nodeID）兼容。
// ts/nonce 为注册证明的防重放字段（M-6，与 ComputeRegisterProof 参数一致）。
// caps 为可选变参：声明能力（如 CapabilityPerNodeSecret）后 hub 回 REG_OK 携带
// per-node secret（I1）；现有调用不传 caps 时行为不变。
func NewRegisterFrame(nodeID, ak, proof string, ts int64, nonce string, meta Meta, caps ...string) []byte {
	if meta.Addr == "" && len(meta.Services) == 0 && len(meta.Tags) == 0 && ak == "" && proof == "" && ts == 0 && nonce == "" && len(caps) == 0 {
		return []byte(nodeID)
	}
	frame := RegisterFrame{NodeID: nodeID, AccessKey: ak, AccessKeyProof: proof, TS: ts, Nonce: nonce, Meta: meta, Capabilities: caps}
	b, _ := json.Marshal(frame)
	return b
}

// hasCapability 判断能力列表中是否包含指定能力。
func hasCapability(caps []string, want string) bool {
	return slices.Contains(caps, want)
}

// generateNodeSecret 用 crypto/rand 生成 32 字节随机数并 base64url 编码。
// 该 secret 仅下发到持有该 node_id 注册连接的节点，不落日志。
func generateNodeSecret() (string, error) {
	var b [32]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成 per-node secret 失败: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// buildRegisterAck 构造注册成功 ACK 帧。secret 为空（节点未声明 per-node-secret
// 能力或生成失败）时返回纯 "REG_OK"，与不感知能力标志的旧客户端兼容。
func buildRegisterAck(secret string) []byte {
	if secret == "" {
		return []byte(RegisterAckOK)
	}
	return []byte(RegisterAckOK + registerAckSecretSep + secret)
}

// maxServiceNameLen / maxServiceAddrLen 是服务宣告字段的长度上限（I3）。
const (
	maxServiceNameLen = 64
	maxServiceAddrLen = 255
)

// containsControlChar 判断字符串是否含控制字符（0x00-0x1F 或 0x7F）。
func containsControlChar(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			return true
		}
	}
	return false
}

// validateServices 校验服务宣告：过滤掉非法条目（空 name/addr、含控制字符、
// 超长），并去除同节点内重复的 name（保留首个）。
// 多节点同名服务保持多候选（不在此做全局去重，不破坏 failover）。
func validateServices(svcs []Service) []Service {
	seen := make(map[string]struct{}, len(svcs))
	out := make([]Service, 0, len(svcs))
	for _, s := range svcs {
		if s.Name == "" || s.Addr == "" {
			continue
		}
		if len(s.Name) > maxServiceNameLen || len(s.Addr) > maxServiceAddrLen {
			continue
		}
		if containsControlChar(s.Name) || containsControlChar(s.Addr) {
			continue
		}
		if _, dup := seen[s.Name]; dup {
			continue
		}
		seen[s.Name] = struct{}{}
		out = append(out, s)
	}
	return out
}

// registerNode 将节点注册到路由表。
// 注意：AddWithInfoAndServices 原子写入节点与服务宣告（重连时清空旧宣告，
// 避免同名节点重连且新注册不带服务时旧服务残留，M4/S4）。
// 节点声明 per-node-secret 能力时生成独立 secret 存入 NodeInfo.Secret（I1/S1）。
//
// mesh 隔离（M-9）：mesh 由注册 AK 解析（tunnel.AccessKeyMesh），写入 NodeInfo.Mesh 并
// 注册到该 mesh 的独立 RouteTable（默认 mesh "" 等价单 mesh 行为）。
//
// mesh 自动对等发现临时身份（disc-<base>-<unixnano>）做防冒充校验（S-fix）：
// base 必须等于 Meta.RealNodeID 且持有该真实节点 per-node secret 的 HMAC 证明，
// 否则拒绝注册（fail-closed）。否则任何已准入节点可注册 disc-<victim>-<nano>
// 冒充 victim，污染对端 accept 侧链路池实现 MITM。
func (s *HubServer) registerNode(reg *RegisterFrame, m *mux.Mux) (NodeInfo, error) {
	info := NodeInfo{
		ID:        NodeID(reg.NodeID),
		Mux:       m,
		Connected: time.Now(),
	}
	if reg.Meta.Addr != "" {
		info.Addr = reg.Meta.Addr
	}
	if strings.HasPrefix(reg.NodeID, discPrefix) {
		base, ok := ParseDiscNodeID(reg.NodeID)
		if !ok || base == "" {
			return NodeInfo{}, fmt.Errorf("disc 临时节点 ID 非法: %q", reg.NodeID)
		}
		if reg.Meta.RealNodeID != base {
			return NodeInfo{}, fmt.Errorf("disc 临时节点 real_node_id 不匹配（疑似冒充 %q）", base)
		}
		realInfo, ok := s.rt.LookupInfo(NodeID(base))
		if !ok || realInfo.Secret == "" {
			return NodeInfo{}, fmt.Errorf("disc 临时节点目标 %q 未注册或未声明 per-node secret", base)
		}
		if !validRealNodeProof(realInfo.Secret, base, reg.Meta.RealNodeProof) {
			return NodeInfo{}, fmt.Errorf("disc 临时节点 real_node_id 证明失败（疑似冒充 %q）", base)
		}
		info.RealNodeID = base
	}
	if hasCapability(reg.Capabilities, CapabilityPerNodeSecret) {
		if secret, err := generateNodeSecret(); err == nil {
			info.Secret = secret
		} else {
			// 生成失败（crypto/rand 极端异常）按未声明能力处理，节点仍可注册。
			s.logger.Warn("生成 per-node secret 失败，节点按未声明能力处理", "node", reg.NodeID, "error", err)
		}
	}
	mesh := tunnel.AccessKeyMesh(reg.AccessKey)
	s.rt.Add(mesh, info, validateServices(reg.Meta.Services))
	// 节点发现表（DHT）喂入：路由表仍权威，DHT 仅作候选节点来源（供 /api/hub/nodes
	// 合并发现）。注册失败不阻断连接（DHT 是辅助发现，不承载转发）。
	// 只喂**稳定真实节点**：跳过瞬态临时身份（disc-/mesh-/p2p- 拨号临时 ID，拨号后
	// 即注销、DHT 无移除路径），否则幽灵节点永久污染发现列表并挤占 k-bucket。
	if s.dht != nil && !isTransientNodeID(reg.NodeID) {
		if perr := s.dht.Register(context.Background(), PeerInfo{
			ID:    string(info.ID),
			Addrs: []string{info.Addr},
			Meta:  map[string]string{"mesh": mesh, "addr": info.Addr},
		}); perr != nil {
			s.logger.Debug("DHT 节点注册失败（忽略）", "node", info.ID, "error", perr)
		}
	}
	return info, nil
}

// isTransientNodeID 判断 node-id 是否为瞬态临时身份（mesh 自动对等拨号 disc-、
// mesh connect mesh-、p2p p2p- 前缀的 <prefix>-<base>-<随机hex>）。这些身份拨号完成
// 后即注销，不应喂入 DHT（发现表只保留稳定真实节点）。
func isTransientNodeID(nodeID string) bool {
	return strings.HasPrefix(nodeID, discPrefix) ||
		strings.HasPrefix(nodeID, "mesh-") ||
		strings.HasPrefix(nodeID, "p2p-")
}

// DiscPrefix 是 mesh 自动对等发现临时节点 ID 的基前缀（不含 '-'，AutoRegister 的
// Prefix 参数用）。完整节点 ID 形如 disc-<base>-<unixnano>，单一来源避免各处硬编码
// 失配（hub 注册校验与 mesh 拨号共用）。
const DiscPrefix = "disc"

// discPrefix 是 mesh discovery 临时节点 ID 的完整前缀（含 '-'）。
const discPrefix = DiscPrefix + "-"

// ParseDiscNodeID 从 mesh discovery 临时节点 ID（disc-<base>-<suffix>）解析真实
// node-id base（base 可含 '-'；suffix 为 16 位随机 hex 尾段，保证并发拨号不碰撞）。
// hub 注册校验与 mesh accept 侧解析共用同一实现，保证"hub 已验证的 base"与
// "accept 侧使用的 base"一致。取最后一个 '-' 之前的全部为 base（base 可含 '-'）；
// 尾段须为合法 hex（对齐 newTempSuffix 格式），避免歧义。
func ParseDiscNodeID(nodeID string) (string, bool) {
	if !strings.HasPrefix(nodeID, discPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(nodeID, discPrefix)
	idx := strings.LastIndex(rest, "-")
	if idx <= 0 {
		return "", false
	}
	tail := rest[idx+1:]
	if !isHexSuffix(tail) {
		return "", false
	}
	base := rest[:idx]
	if base == "" {
		return "", false
	}
	return base, true
}

// isHexSuffix 报告 tail 是否为随机 hex 后缀（newTempSuffix 生成 16 位 hex）。
func isHexSuffix(tail string) bool {
	if len(tail) < 8 || len(tail) > 64 {
		return false
	}
	for _, r := range tail {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// validRealNodeProof 校验 mesh discovery 临时注册的 real_node_id 证明：
// HMAC-SHA256(secret, realNodeID) 的 hex 与 proof 恒时比较。真实节点的 per-node
// secret 只下发给出该节点（hub 注册时生成、仅 REG_OK 携带），冒充者无法计算证明。
func validRealNodeProof(secret, realNodeID, proof string) bool {
	if secret == "" || proof == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(realNodeID))
	got := hex.EncodeToString(mac.Sum(nil))
	return subtle.ConstantTimeCompare([]byte(got), []byte(proof)) == 1
}

// HubServer 是 Hub 端的节点收口服务：接收 xfer.Conn 连接，
// 完成注册/鉴权并写入 RouteTable；连接断开后自动移除。
//
// 注册协议：节点连接建立后，在 xfer 层（mux 之上）直接发送一条
// 注册消息（JSON 或裸 nodeID），Hub 侧通过 conn.Receive 读取，不占用
// mux 的流创建——这样后续 Tunnel.Serve 的 Open/Accept 与 TCP 流中继
// 可复用同一条 mux 而互不抢 acceptCh。
type HubServer struct {
	rt     *MeshRouteTable
	auth   *Authenticator
	logger *slog.Logger
	// maxConns 是并发连接上限信号量；nil 表示无上限（兼容现有测试构造与极端场景）。
	maxConns chan struct{}
	// dht 是节点发现表（nil = 不启用 DHT 候选，既有行为）。路由表仍 hub 权威；
	// DHT 只作为候选节点来源（注册时喂入，发现时供 /api/hub/nodes 合并）。
	// 由 cmd/sproxy 装配 Kademlia 时经 SetDHT 注入（hub.dht: kad）。
	dht DHT
}

// SetDHT 注入节点发现表（DHT）。nil 清除（恢复不启用 DHT 候选）。
// 须在服务器开始处理连接前调用。
func (s *HubServer) SetDHT(dht DHT) {
	s.dht = dht
}

// NewHubServer 创建节点收口服务。auth 为 nil 时视为 fail-closed（拒绝所有注册），
// 防止调用方"遗漏传 auth"走向开放注册（M-7）。
// rt 为每 mesh 独立路由表的聚合（M-9），按注册 AK 解析的 mesh 分表隔离。
// maxConns 为可选变参：传 >0 的值表示 Hub 同时处理的连接数上限（I30），
// 不传或 <=0 表示无上限。
func NewHubServer(rt *MeshRouteTable, auth *Authenticator, logger *slog.Logger, maxConns ...int) *HubServer {
	if logger == nil {
		logger = slog.Default()
	}
	if auth == nil {
		auth = NewAuthenticator(nil)
	}
	s := &HubServer{rt: rt, auth: auth, logger: logger}
	if len(maxConns) > 0 && maxConns[0] > 0 {
		s.maxConns = make(chan struct{}, maxConns[0])
	}
	return s
}

// TryHandleConn 非阻塞获取一个连接名额；成功时启动 goroutine 调用 HandleConn 处理连接，
// 并在处理结束后释放名额。信号量已满时返回 false，由调用方负责关闭 conn。
// maxConns 未配置（nil）时始终接受，退化为无条件并发处理。
func (s *HubServer) TryHandleConn(ctx context.Context, conn xfer.Conn) bool {
	if s.maxConns != nil {
		select {
		case s.maxConns <- struct{}{}:
		default:
			return false
		}
	}
	go func() {
		if s.maxConns != nil {
			defer func() { <-s.maxConns }()
		}
		_ = s.HandleConn(ctx, conn)
	}()
	return true
}

// ListenTCP 绑定裸 TCP 中继监听地址（同步，绑定错误立即返回，便于装配点 fail-fast）。
// 复用内置 xfer/tcp 传输（长度前缀分帧），连接接入后走与 WS 完全相同的
// HandleConn 注册/鉴权/中继路径——传输层从 ws 扩到 tcp，协议零改动。
func (s *HubServer) ListenTCP(ctx context.Context, addr string) (xfer.Listener, error) {
	tp := xfer.Get("tcp")
	if tp == nil {
		return nil, fmt.Errorf("tcp 传输层未注册")
	}
	ln, err := tp.Listen(ctx, addr)
	if err != nil {
		return nil, fmt.Errorf("hub tcp listen %s: %w", addr, err)
	}
	return ln, nil
}

// AcceptTCP 在已绑定的裸 TCP listener 上接受节点连接，复用 HandleConn 的
// 注册/鉴权/中继（与 WS accept 循环等价，仅传输层不同）。
// 阻塞直到 ctx 取消或 listener 关闭；ctx 取消时返回 nil。
// 连接数上限由 HubServer 信号量（maxConns）控制，超限立即关闭新连接。
func (s *HubServer) AcceptTCP(ctx context.Context, ln xfer.Listener) error {
	for {
		conn, aerr := ln.Accept(ctx)
		if aerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return aerr
		}
		if !s.TryHandleConn(ctx, conn) {
			s.logger.Warn("Hub TCP 连接数达到上限，拒绝新连接")
			_ = conn.Close()
			continue
		}
	}
}

// HandleConn 接收一个已建立的节点连接，注册并维护其生命周期。
// 阻塞直到连接断开后返回。
//
// 协议顺序（重要）：先读取 xfer 层注册帧（节点连接后发送的一条消息，
// 不经过 mux 流），完成注册/鉴权后再创建 mux 走流协议。
// 若先建 mux，其 readLoop 会与注册帧的 Receive 竞争同一连接。
func (s *HubServer) HandleConn(ctx context.Context, conn xfer.Conn) error {
	// 确保所有退出路径都关闭连接（注册帧读取失败、鉴权失败等
	// 在 mux 创建前 return，不 close 会导致 WS 连接泄漏）。
	defer conn.Close()

	// flushCtx 供 sendRegErr flush 使用（写失败/对端离线时快速放弃，不拖慢连接关闭）。
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()

	// sendRegErr 回发错误 ACK（尽力而为，客户端据此判断注册失败）
	sendRegErr := func(reason string) error {
		// 关键帧必须经 flush 确保真正写出再关闭，否则 defer conn.Close() 的
		// CloseNow() 会掐掉排队中的 REG_ERR，对端只收到 EOF 而误判网络波动重连。
		if serr := conn.Send(ctx, []byte(RegisterAckErr+reason)); serr == nil {
			if fl, ok := conn.(xfer.Flusher); ok {
				if ferr := fl.Flush(flushCtx); ferr != nil {
					s.logger.Debug("flush REG_ERR 失败", "error", ferr)
				}
			}
		}
		return fmt.Errorf("注册失败: %s", reason)
	}

	reg, received, err := s.readRegisterFrame(ctx, conn)
	if err != nil {
		if !received {
			// P1-7：注册帧未读到（超时/网络错误）→ 不回 REG_ERR，静默断开让客户端按
			// 网络问题重连。回 REG_ERR 会让客户端 isTerminalRelayError 判终态永久
			// 退出——纯网络抖动即可杀死 relay 守护进程（违反 relay.go 自述契约）。
			s.logger.Warn("读取注册帧失败（网络/超时），静默断开", "error", err)
			return err
		}
		// 已读到帧但非法：回 REG_ERR（客户端据此终止或重试）。
		s.logger.Warn("注册帧非法", "error", err)
		return sendRegErr("bad register frame")
	}
	if reg.NodeID == "" {
		return sendRegErr("missing node_id")
	}
	if s.auth != nil {
		if authErr := s.auth.Authenticate(reg.AccessKey, reg.AccessKeyProof, reg.NodeID, reg.TS, reg.Nonce); authErr != nil {
			s.logger.Warn("中继节点鉴权失败", "node", reg.NodeID, "error", authErr)
			_ = sendRegErr("invalid access key") // 回发 REG_ERR 供客户端终止重连（忽略错误，保留原始鉴权错误）
			return authErr                       // 保留原始错误（ErrInvalidAccessKey/Proof）供调用方/测试识别
		}
	}
	// reg.AccessKey == "" 时 Authenticate 会因未命中 accessKeys 返回 ErrInvalidAccessKey（fail-closed），
	// 无需额外判断。

	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	info, err := s.registerNode(reg, m)
	if err != nil {
		// fail-closed：disc 临时身份防冒充校验失败（或注册表异常）→ REG_ERR 拒绝，
		// 不注册节点。客户端按 REG_ERR 判终态/重试。
		s.logger.Warn("节点注册被拒绝", "node", reg.NodeID, "error", err)
		return sendRegErr(err.Error())
	}
	s.logger.Info("中继节点已注册", "node", reg.NodeID, "addr", info.Addr)

	// 注册成功后立即注册清理 defer（RemoveIfOwned 幂等，重复调用返回 false）。
	// 结构性覆盖所有 return 路径（I4）：ACK 发送失败提前 return 时也不会残留
	// "已注册但 mux 已关"的幽灵节点。仅移除属于本连接的节点（防 stale identity：
	// 同名节点若已被新连接重新注册，所有权不匹配不误删）。RemoveIfOwned 内部
	// 已清除该节点的服务宣告，无需额外 CleanServices。
	defer func() {
		if s.rt.RemoveIfOwned(info.ID, m) {
			s.logger.Info("中继节点已移除", "node", reg.NodeID)
			// 同步从发现表（DHT）移除，防幽灵节点残留（稳定节点断开后不应再出现在
			// /api/hub/nodes 发现列表）。DHT 移除失败不影响连接清理。
			if s.dht != nil && !isTransientNodeID(reg.NodeID) {
				if rerr := s.dht.Remove(context.Background(), string(info.ID)); rerr != nil {
					s.logger.Debug("DHT 节点移除失败（忽略）", "node", info.ID, "error", rerr)
				}
			}
		}
	}()

	// 回发注册 ACK：让客户端尽早感知注册成功（而非等到建流失败才发现）。
	// 鉴权失败路径在 mux 创建前 return，不经过这里。
	ack := buildRegisterAck(info.Secret)
	if ackErr := conn.Send(ctx, ack); ackErr != nil {
		s.logger.Warn("回发注册 ACK 失败", "node", reg.NodeID, "error", ackErr)
		return ackErr
	}
	// 与 REG_ERR 对称：关键帧 flush 后再进入 accept 循环，避免 WS 传输下
	// sendLoop 异步写出时连接关闭掐掉排队中的 REG_OK（S24）。
	if fl, ok := conn.(xfer.Flusher); ok {
		if ferr := fl.Flush(flushCtx); ferr != nil {
			s.logger.Debug("flush REG_OK 失败", "node", reg.NodeID, "error", ferr)
		}
	}

	// 阻塞直到连接断开：accept 循环让 mux 保持存活，并消费叶子可能
	// 主动 open 的流（当前协议下叶子只 accept 不 open，正常不会到达）。
	// 循环退出意味着连接断开，随后的 defer 移除节点。
	for {
		stream, aerr := m.Accept(ctx)
		if aerr != nil {
			break
		}
		// 叶子主动开流当前无协议场景，直接关闭（防流堆积在 acceptCh）。
		_ = stream.Close()
	}

	return nil
}

// readRegisterFrame 读取节点注册信息。
// 协议：连接建立后，节点在 xfer 层发送一条 JSON 注册帧 {node_id, token, meta}
// （由 hub.NewRegisterFrame 构建；为容错，裸字符串 nodeID 也接受）。
//
// 返回 (reg, received, err)：
//   - received=false：注册帧未读到（超时/网络错误）——调用方不应回发 REG_ERR，
//     静默断开即可（P1-7：瞬时失败回 REG_ERR 会让客户端误判终态永久退出）；
//   - received=true 且 err != nil：已读到帧但非法（过大/缺 node_id）——调用方回 REG_ERR。
//
// 注意：在 xfer 层（conn.Receive）读取，不占用 mux 流的创建。
// 与旧版（base 399ff89）的「mux 控制流发 nodeID」协议不兼容——分支未发布，
// 属协议升级而非破坏。
func (s *HubServer) readRegisterFrame(ctx context.Context, conn xfer.Conn) (*RegisterFrame, bool, error) {
	regCtx, cancel := context.WithTimeout(ctx, registerFrameTTL)
	defer cancel()

	msg, err := conn.Receive(regCtx)
	if err != nil {
		return nil, false, err
	}
	if len(msg) > maxRegisterFrameBytes {
		return nil, true, fmt.Errorf("注册帧过大: %d bytes (max %d)", len(msg), maxRegisterFrameBytes)
	}
	raw := msg

	reg := &RegisterFrame{}
	if err := json.Unmarshal(raw, reg); err != nil {
		// 非 JSON → 裸字符串容错（旧版仅发 nodeID）
		reg = &RegisterFrame{NodeID: strings.TrimSpace(string(raw))}
	} else if reg.NodeID == "" {
		// 合法 JSON 但缺 node_id：拒绝，不把 JSON 垃圾整串当裸串回退（S2）
		return nil, true, fmt.Errorf("注册帧缺少 node_id")
	}
	return reg, true, nil
}
