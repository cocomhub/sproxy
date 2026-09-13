// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// mesh_status.go 提供 `GET /api/mesh/status`：跨节点（mesh）面的**只读运维视图**（W1）。
//
// 为什么需要它：跨节点能力默认关闭、且失败多以「任务报错/对端连不上」的形式暴露 ⇒ 出问题时
// 第一个要回答的问题就是「本机的面起了没、pin 了几个、节点注册上没」。此前只能翻日志。
//
// 设计边界（职责切分）：
//   - **配置态**（是否启用 / 监听配置 / pin 数 / 角色参数）由本包从 `cfg` 推导——装配层不必重复；
//   - **运行态**（实际监听地址、节点角色是否真的跑起来）由装配层经 `SetMeshRuntimeInfo` 注入
//     ——因为只有 `cmd/sproxy` 知道 listener 实际绑到了哪个端口、`RunNode` 是否成功启动。
//     两者分离的硬理由：**配置了但没起成功**（端口被占/凭据缺失）时必须显示真实状态，否则误导排障。
//
// 安全：只暴露拓扑与计数，**不含任何秘密**（指纹本身是公开标识；不返回 SK/隧道密钥/信令密钥）。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// MeshFaceStatus 是单个跨节点面（只读/写）的状态。
type MeshFaceStatus struct {
	Enabled bool `json:"enabled"`
	// Addr 是**实际**监听地址（运行态优先；未注入运行态时回落配置值）。
	Addr string `json:"addr,omitempty"`
	// Pinned 是 pin 的节点指纹数：读面 = 全部 mesh_readers；写面 = 仅 scope 授予写（write/rw）。
	Pinned int `json:"pinned"`
}

// MeshNodeStatus 是 B 侧 mesh node 角色状态。
type MeshNodeStatus struct {
	// Running 表示角色是否**真的**在运行（配置启用但启动失败时为 false）。
	Running bool `json:"running"`
	// NodeID / HubURL / WebRTC / Services 来自配置（未运行时也显示，便于对照排查）。
	NodeID   string   `json:"node_id,omitempty"`
	HubURL   string   `json:"hub_url,omitempty"`
	WebRTC   bool     `json:"webrtc"`
	Services []string `json:"services,omitempty"`
}

// MeshStatus 是 `GET /api/mesh/status` 的响应体。
type MeshStatus struct {
	RemoteRead  *MeshFaceStatus `json:"remote_read,omitempty"`
	RemoteWrite *MeshFaceStatus `json:"remote_write,omitempty"`
	Node        *MeshNodeStatus `json:"node,omitempty"`
	// HubURL 是 A 侧 mesh 客户端使用的 hub（空 = 本机 hub）。
	HubURL string `json:"hub_url,omitempty"`
	// SignalingEnabled 表示是否配置了 WebRTC 信令（`mesh.node_id`）。
	SignalingEnabled bool `json:"signaling_enabled"`
}

// MeshRuntimeInfo 是装配层注入的**运行态**片段（其余由配置推导）。
type MeshRuntimeInfo struct {
	// RemoteReadAddr / RemoteWriteAddr 是 listener **实际**监听地址（配置写 `:0` 时只有
	// listener 知道真实端口）；空 = 未启动，视图回落配置值。
	RemoteReadAddr  string
	RemoteWriteAddr string
	// NodeRoleRunning 表示 `mesh.node` 角色是否成功启动。
	NodeRoleRunning bool
}

// SetMeshRuntimeInfo 注入运行态信息提供者（装配层在 listener / 角色启动后调用）。
// 未注入时视图只反映配置态（Addr 回落配置值、Running=false）。
func (h *Handlers) SetMeshRuntimeInfo(fn func() MeshRuntimeInfo) {
	h.meshRuntimeInfo = fn
}

// meshStatus 汇总配置态 + 运行态。
func (h *Handlers) meshStatus() MeshStatus {
	cfg := h.cfgPtr.Load()
	st := MeshStatus{}
	if cfg == nil {
		return st
	}
	var rt MeshRuntimeInfo
	if h.meshRuntimeInfo != nil {
		rt = h.meshRuntimeInfo()
	}

	if cfg.RemoteRead.Enabled {
		addr := rt.RemoteReadAddr
		if addr == "" {
			addr = cfg.RemoteRead.Listen
		}
		st.RemoteRead = &MeshFaceStatus{
			Enabled: true,
			Addr:    addr,
			Pinned:  len(meshReaderFingerprints(cfg)),
		}
	}
	if cfg.RemoteWrite.Enabled {
		addr := rt.RemoteWriteAddr
		if addr == "" {
			addr = cfg.RemoteWrite.Listen
		}
		st.RemoteWrite = &MeshFaceStatus{
			Enabled: true,
			Addr:    addr,
			Pinned:  len(meshWriterFingerprints(cfg)),
		}
	}
	if cfg.Mesh.Node.Enabled {
		st.Node = &MeshNodeStatus{
			Running:  rt.NodeRoleRunning,
			NodeID:   cfg.MeshNodeID(),
			HubURL:   cfg.MeshNodeHubURL(),
			WebRTC:   cfg.Mesh.Node.WebRTC,
			Services: meshNodeDeclaredServices(cfg),
		}
	}
	st.HubURL = cfg.Mesh.HubURL
	st.SignalingEnabled = cfg.Mesh.NodeID != ""
	return st
}

// meshNodeDeclaredServices 返回 node 角色将要宣告的服务名（读面/写面 + extra_services 的名字部分）。
//
// 只给**名字**（不含地址）：地址已在对应面里显示，这里回答的是「宣告了哪些服务」。
func meshNodeDeclaredServices(cfg *Config) []string {
	out := make([]string, 0, 3)
	if cfg.RemoteRead.Enabled {
		out = append(out, "volread")
	}
	if cfg.RemoteWrite.Enabled {
		out = append(out, "volwrite")
	}
	for _, decl := range cfg.Mesh.Node.ExtraServices {
		// "name:host:port" ⇒ 取首段；畸形声明（无冒号）跳过（RunNode 侧解析时会告警）。
		if i := indexByte(decl, ':'); i > 0 {
			out = append(out, decl[:i])
		}
	}
	return out
}

// indexByte 是 strings.IndexByte 的本地包装（避免为本文件引入额外 import 面）。
func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// meshStatusHandler 处理 GET /api/mesh/status。
func (h *Handlers) meshStatusHandler(w http.ResponseWriter, _ *http.Request) {
	sendJSONResponse(w, h.meshStatus(), http.StatusOK)
}

// 编译期：MeshFaceStatus 的 Pinned 口径与 volume 包的 scope 判定同源（防将来漂移）。
var _ = volume.MeshScopeRW
