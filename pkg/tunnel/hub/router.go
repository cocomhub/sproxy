// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// registerFrameTTL 是注册帧等待的超时时间。
const registerFrameTTL = 10 * time.Second

// maxRegisterFrameBytes 是注册帧的最大字节数（防恶意大帧耗尽内存）。
const maxRegisterFrameBytes = 64 << 10 // 64 KiB

// 注册 ACK 帧常量（xfer 层一条消息）。导出供 sclient 注册后等待。
const (
	RegisterAckOK  = "REG_OK"
	RegisterAckErr = "REG_ERR:"
)

// RegisterFrame 是节点连接后的注册帧（JSON）。
// 向后兼容：若首个流上收到的是非 JSON 裸字符串（旧版仅发 nodeID），
// 则等价于仅携带 NodeID 且无 token 的注册帧。
type RegisterFrame struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token,omitempty"`
	Meta   Meta   `json:"meta"`
}

// Meta 是节点注册时宣告的附加信息，供 mesh 选路使用。
type Meta struct {
	Addr     string    `json:"addr,omitempty"`     // 节点自身可达地址（直连地址，可为空）
	Services []Service `json:"services,omitempty"` // 本地服务宣告（本地监听或出口可达目标）
	Tags     []string  `json:"tags,omitempty"`     // 可选标签（如 "exit"、"trusted"）
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

// RegisterRequest 是节点发起注册的导出请求结构，供 sclient 复用。
type RegisterRequest struct {
	NodeID string `json:"node_id"`
	Token  string `json:"token,omitempty"`
	Meta   Meta   `json:"meta"`
}

// NewRegisterFrame 构建注册帧。当无 meta/token 时退化为裸 nodeID，
// 保证与旧版 hub（仅接收裸 nodeID）兼容。
func NewRegisterFrame(nodeID, token string, meta Meta) []byte {
	if meta.Addr == "" && len(meta.Services) == 0 && len(meta.Tags) == 0 && token == "" {
		return []byte(nodeID)
	}
	req := RegisterRequest{NodeID: nodeID, Token: token, Meta: meta}
	b, _ := json.Marshal(req)
	return b
}

// registerNode 将节点注册到路由表。
// 注意：重连时先清空旧服务宣告再写入新宣告，避免同名节点重连
// 且新注册不带服务时旧服务残留（M4）。
func (s *HubServer) registerNode(reg *RegisterFrame, m *mux.Mux) NodeInfo {
	info := NodeInfo{
		ID:        NodeID(reg.NodeID),
		Mux:       m,
		Connected: time.Now(),
		Token:     reg.Token,
	}
	if reg.Meta.Addr != "" {
		info.Addr = reg.Meta.Addr
	}
	s.rt.AddWithInfo(info)
	s.rt.SetServices(info.ID, reg.Meta.Services) // 空 slice 也写入，等价清除旧宣告
	return info
}

// HubServer 是 Hub 端的节点收口服务：接收 xfer.Conn 连接，
// 完成注册/鉴权并写入 RouteTable；连接断开后自动移除。
//
// 注册协议：节点连接建立后，在 xfer 层（mux 之上）直接发送一条
// 注册消息（JSON 或裸 nodeID），Hub 侧通过 conn.Receive 读取，不占用
// mux 的流创建——这样后续 Tunnel.Serve 的 Open/Accept 与 TCP 流中继
// 可复用同一条 mux 而互不抢 acceptCh。
type HubServer struct {
	rt     *RouteTable
	auth   *Authenticator
	logger *slog.Logger
	// maxConns 是并发连接上限信号量；nil 表示无上限（兼容现有测试构造与极端场景）。
	maxConns chan struct{}
}

// NewHubServer 创建节点收口服务。auth 为 nil 时不鉴权。
// maxConns 为可选变参：传 >0 的值表示 Hub 同时处理的连接数上限（I30），
// 不传或 <=0 表示无上限。
func NewHubServer(rt *RouteTable, auth *Authenticator, logger *slog.Logger, maxConns ...int) *HubServer {
	if logger == nil {
		logger = slog.Default()
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

	reg, err := s.readRegisterFrame(ctx, conn)
	if err != nil {
		s.logger.Warn("读取注册帧失败", "error", err)
		return sendRegErr("bad register frame")
	}
	if reg.NodeID == "" {
		return sendRegErr("missing node_id")
	}
	if s.auth != nil {
		if err := s.auth.Authenticate(reg.Token); err != nil {
			s.logger.Warn("中继节点鉴权失败", "node", reg.NodeID)
			_ = sendRegErr("invalid token") // 回发 REG_ERR 供客户端终止重连（忽略错误，保留原始鉴权错误）
			return err                      // 保留原始错误（ErrInvalidToken）供调用方/测试识别
		}
	}

	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	info := s.registerNode(reg, m)
	s.logger.Info("中继节点已注册", "node", reg.NodeID, "addr", info.Addr)

	// 回发注册 ACK：让客户端尽早感知注册成功（而非等到建流失败才发现）。
	// 鉴权失败路径在 mux 创建前 return，不经过这里。
	if ackErr := conn.Send(ctx, []byte(RegisterAckOK)); ackErr != nil {
		s.logger.Warn("回发注册 ACK 失败", "node", reg.NodeID, "error", ackErr)
		return ackErr
	}

	// 阻塞直到连接断开：accept 循环让 mux 保持存活，并消费叶子可能
	// 主动 open 的流（当前协议下叶子只 accept 不 open，正常不会到达）。
	// 循环退出意味着连接断开，随后移除节点。
	for {
		stream, aerr := m.Accept(ctx)
		if aerr != nil {
			break
		}
		// 叶子主动开流当前无协议场景，直接关闭（防流堆积在 acceptCh）。
		_ = stream.Close()
	}

	// 仅移除属于本连接的节点（防 stale identity：同名节点若已被新连接
	// 重新注册，不应被旧连接断开时误删）。RemoveIfOwned 内部已清除该节点的
	// 服务宣告，无需额外 CleanServices。
	if s.rt.RemoveIfOwned(info.ID, m) {
		s.logger.Info("中继节点已移除", "node", reg.NodeID)
	}
	return nil
}

// readRegisterFrame 读取节点注册信息。
// 协议：连接建立后，节点在 xfer 层发送一条 JSON 注册帧 {node_id, token, meta}
// （由 hub.NewRegisterFrame 构建；为容错，裸字符串 nodeID 也接受）。
//
// 注意：在 xfer 层（conn.Receive）读取，不占用 mux 流的创建。
// 与旧版（base 399ff89）的「mux 控制流发 nodeID」协议不兼容——分支未发布，
// 属协议升级而非破坏。
func (s *HubServer) readRegisterFrame(ctx context.Context, conn xfer.Conn) (*RegisterFrame, error) {
	regCtx, cancel := context.WithTimeout(ctx, registerFrameTTL)
	defer cancel()

	msg, err := conn.Receive(regCtx)
	if err != nil {
		return nil, err
	}
	if len(msg) > maxRegisterFrameBytes {
		return nil, fmt.Errorf("注册帧过大: %d bytes (max %d)", len(msg), maxRegisterFrameBytes)
	}
	raw := msg

	reg := &RegisterFrame{}
	if err := json.Unmarshal(raw, reg); err != nil || reg.NodeID == "" {
		// 裸字符串容错
		reg = &RegisterFrame{NodeID: strings.TrimSpace(string(raw))}
	}
	return reg, nil
}
