// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
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
}

// NewHubServer 创建节点收口服务。auth 为 nil 时不鉴权。
func NewHubServer(rt *RouteTable, auth *Authenticator, logger *slog.Logger) *HubServer {
	if logger == nil {
		logger = slog.Default()
	}
	return &HubServer{rt: rt, auth: auth, logger: logger}
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

	reg, err := s.readRegisterFrame(ctx, conn)
	if err != nil {
		s.logger.Warn("读取注册帧失败", "error", err)
		return err
	}
	if reg.NodeID == "" {
		return fmt.Errorf("注册帧缺少 node_id")
	}
	if s.auth != nil {
		if err := s.auth.Authenticate(reg.Token); err != nil {
			s.logger.Warn("中继节点鉴权失败", "node", reg.NodeID)
			return err
		}
	}

	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	info := s.registerNode(reg, m)
	s.logger.Info("中继节点已注册", "node", reg.NodeID, "addr", info.Addr)

	// 持续 handle 新到达的流直到连接关闭：
	// 1. 由旧版 relay 等调用方发起的隧道请求（HTTP 请求-响应交换）
	// 2. 期1 新增的 TCP 流中继（首帧 DialRequest）——由 hub 转给出口叶子
	for {
		stream, aerr := m.Accept(ctx)
		if aerr != nil {
			break
		}
		s.handleStream(m, stream)
	}

	// 仅移除属于本连接的节点（防 stale identity：同名节点若已被新连接
	// 重新注册，不应被旧连接断开时误删）。RemoveIfOwned 内部已清除该节点的
	// 服务宣告与 returnCh，无需额外 CleanServices。
	if s.rt.RemoveIfOwned(info.ID, m) {
		s.logger.Info("中继节点已移除", "node", reg.NodeID)
	}
	return nil
}

// readRegisterFrame 读取节点注册信息，兼容两种格式：
//   - JSON（新版）{node_id, token, meta}
//   - 裸字节串（旧版）即 nodeID
//
// 注意：在 xfer 层（conn.Receive）读取，不占用 mux 流的创建。
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
		// 裸字符串兼容
		reg = &RegisterFrame{NodeID: strings.TrimSpace(string(raw))}
	}
	return reg, nil
}

// handleStream 处理单个到达流：按首帧类型分发。
// 若首帧是 DialRequest（[4B len][{"dial":addr}]，与 relay_stream/p2p/leaf 一致），
// 由叶子出口节点（relay.Serve）处理；否则视为隧道 HTTP 请求尾帧。
//
// 注意：当前叶子不会主动向 hub 开流（dial/隧道流都是 hub 主动 Open 向叶子），
// 此路径仅在未来叶子→hub 主动开流时可达。帧格式与 relay_stream.go 保持一致，
// 避免 M2 式的潜伏格式错位。
func (s *HubServer) handleStream(m *mux.Mux, stream mux.Stream) {
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(stream, lenBuf); err != nil {
		return
	}
	metaLen := binary.BigEndian.Uint32(lenBuf)
	if metaLen == 0 || metaLen > 1<<20 {
		s.rt.ReturnStream(m, stream)
		return
	}
	meta := make([]byte, metaLen)
	if _, err := io.ReadFull(stream, meta); err != nil {
		return
	}
	var head DialRequest
	if err := json.Unmarshal(meta, &head); err == nil && head.Dial != "" {
		// Hub 自身不做出口（叶子/portal 承担），此处记录即可。
		s.logger.Debug("收到非出口的 dial 帧", "addr", head.Dial)
		stream.Close()
		return
	}
	// 非 dial 帧（HTTP tunnel 请求尾帧）：回传 JitAccept 队列供 Tunnel.Serve 处理。
	s.rt.ReturnStream(m, stream)
}
