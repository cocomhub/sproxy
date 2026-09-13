// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
)

// MeshStatus 镜像服务端 `GET /api/mesh/status` 的响应（跨节点面的只读运维视图）。
//
// 字段与服务端 `pkg/server.MeshStatus` 一一对应（**不含任何秘密**：指纹为公开标识）。
// 用于 CLI `mesh status --server` 展示「本机跨节点面/角色起了没、pin 了几个」。
type MeshStatus struct {
	RemoteRead       *MeshFaceStatus `json:"remote_read,omitempty"`
	RemoteWrite      *MeshFaceStatus `json:"remote_write,omitempty"`
	Node             *MeshNodeStatus `json:"node,omitempty"`
	HubURL           string          `json:"hub_url,omitempty"`
	SignalingEnabled bool            `json:"signaling_enabled"`
}

// MeshFaceStatus 是单个跨节点面（只读/写）的状态。
type MeshFaceStatus struct {
	Enabled bool `json:"enabled"`
	// Addr 是**实际**监听地址（配置写 `:0` 时只有 listener 知道真实端口）。
	Addr string `json:"addr,omitempty"`
	// Pinned 是 pin 的节点指纹数（读面 = 全部 mesh_readers；写面 = 仅 scope 授予写）。
	Pinned int `json:"pinned"`
}

// MeshNodeStatus 是 B 侧 mesh node 角色状态。
type MeshNodeStatus struct {
	// Running 表示角色是否**真的**在运行（配置启用但启动失败时为 false）。
	Running bool   `json:"running"`
	NodeID  string `json:"node_id,omitempty"`
	HubURL  string `json:"hub_url,omitempty"`
	WebRTC  bool   `json:"webrtc"`
	// Services 是宣告的服务名（地址在各面里显示）。
	Services []string `json:"services,omitempty"`
}

// MeshStatus 查询服务端跨节点面/角色状态（`GET /api/mesh/status`）。
//
// 全关时服务端返回 `{}` ⇒ 本方法返回**空结构且不报错**（与「未启用」这一正常状态一致）；
// 非 200 才报错。
func (c *FileClient) MeshStatus(ctx context.Context) (*MeshStatus, error) {
	var st MeshStatus
	if err := c.doJSON(ctx, http.MethodGet, "/api/mesh/status", nil, &st); err != nil {
		return nil, fmt.Errorf("获取跨节点状态失败: %w", err)
	}
	return &st, nil
}

// MeshACLEntry 是单条跨节点授权（对应服务端 `mesh_readers` 中属于本 owner 的条目）。
type MeshACLEntry struct {
	Volume      string `json:"volume"`
	Node        string `json:"node"`
	Fingerprint string `json:"fingerprint"`
	Scope       string `json:"scope"`
}

// MeshACL 镜像服务端 `GET /api/mesh/acl` 的响应：**仅本人 owner** 的跨节点授权列表。
type MeshACL struct {
	Owner   string         `json:"owner"`
	Entries []MeshACLEntry `json:"entries"`
}

// MeshACL 查询服务端返回的、**调用者自己 owner** 的跨节点授权（`GET /api/mesh/acl`）。
//
// 可见性由服务端按已认证 actor 判定（口径：仅 owner 自身），客户端**无法**要求别人的授权 ——
// 故意不提供 owner 参数。无授权时返回空列表且不报错（正常态）。
func (c *FileClient) MeshACL(ctx context.Context) (*MeshACL, error) {
	var acl MeshACL
	if err := c.doJSON(ctx, http.MethodGet, "/api/mesh/acl", nil, &acl); err != nil {
		return nil, fmt.Errorf("获取跨节点授权失败: %w", err)
	}
	return &acl, nil
}
