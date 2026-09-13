// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// mesh_acl.go 提供 `GET /api/mesh/acl`：回答「**谁**被授权读写**我的**命名空间」的只读视图（W3）。
//
// 为什么需要它：跨节点写在 B 侧是**默认拒绝**的（scope 决定对端能否建链、能否写），而配置在
// 卷 ACL 里、且常由别人（平台/运维）维护 ⇒ 出问题时 owner 需要能自证「我这条授权到底配了什么」。
//
// **可见性口径（用户 2026-09-13 决策：仅 owner 自身）**：只返回
// `mesh_readers.owner == 调用者 owner` 的条目。别人的授权不仅不出现在列表里，也**不通过计数
// 泄露**（总数恰为本人条目数）。这比 `/api/volumes`（按 ACL 判定「可用卷」）更窄：那里「可见」=
// 可用，这里「可见」= **本人命名空间的授权**。口径只认「已认证 actor」（`ActorFrom`），
// **不接受任何查询参数覆盖**——否则等于把 owner 过滤变成可绕过。
//
// 安全：返回的指纹是**公开标识**（对端服务端身份，非秘密；生产日志/`/api/mesh/status` 同样输出），
// 但不含任何密钥；无认证部署下 actor 为空 → 归入 `anonymous` owner（与全仓库 `normalizeOwner`
// 口径一致：该部署只有一个隐式 owner，本来就能读到全部文件）。

import (
	"net/http"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// MeshACLEntry 是单条跨节点授权（对应一卷的 `mesh_readers` 条目中属于本 owner 者）。
type MeshACLEntry struct {
	// Volume 是卷名（条目所在卷；授权是「卷 + owner」二维的）。
	Volume string `json:"volume"`
	// Node 是被授权节点名（人类可读标识，便于对照配置）。
	Node string `json:"node"`
	// Fingerprint 是被授权节点的 Ed25519 指纹（公开标识，规范形 `sha256:<64 hex>`）。
	Fingerprint string `json:"fingerprint"`
	// Scope 是授权范围：read | write | rw（缺省 read；取值集合单源在 pkg/volume）。
	Scope string `json:"scope"`
}

// MeshACLResponse 是 `GET /api/mesh/acl` 的响应体。
//
// 始终带上 `owner`：让调用方（CLI/UI）能显示「这是**谁**的授权」，也便于排查「我以为我是 alice」
// 这类认证口径问题。
type MeshACLResponse struct {
	Owner   string         `json:"owner"`
	Entries []MeshACLEntry `json:"entries"`
}

// meshACLEntriesForOwner 返回 cfg 中 owner 命中目标值的全部 mesh_readers 条目。
//
// 顺序 = 卷声明序 × 条目声明序（稳定，便于测试与 UI diff）；无授权返回**空切片**（非 nil，
// JSON 序列化为 `[]`）。scope 经 `volume.NormalizeMeshScope` 归一（缺省 read；非法值在配置
// Validate 期已被拒绝，这里再兜底为 read 而不是静默丢弃条目——**不隐藏**比「少一条」更安全）。
func meshACLEntriesForOwner(cfg *Config, owner string) []MeshACLEntry {
	out := make([]MeshACLEntry, 0, 4)
	if cfg == nil {
		return out
	}
	for i := range cfg.Volumes {
		v := &cfg.Volumes[i]
		if v.ACL == nil {
			continue
		}
		for j := range v.ACL.MeshReaders {
			mr := &v.ACL.MeshReaders[j]
			if mr.Owner != owner {
				continue
			}
			scope := volume.MeshScopeRead
			if norm, ok := volume.NormalizeMeshScope(mr.Scope); ok {
				scope = norm
			}
			out = append(out, MeshACLEntry{
				Volume:      v.Name,
				Node:        mr.Node,
				Fingerprint: mr.Fingerprint,
				Scope:       scope,
			})
		}
	}
	return out
}

// meshACLHandler 处理 GET /api/mesh/acl（口径见文件头）。
func (h *Handlers) meshACLHandler(w http.ResponseWriter, r *http.Request) {
	owner := normalizeOwner(ownerFromRequest(r))
	sendJSONResponse(w, MeshACLResponse{
		Owner:   owner,
		Entries: meshACLEntriesForOwner(h.cfgPtr.Load(), owner),
	}, http.StatusOK)
}
