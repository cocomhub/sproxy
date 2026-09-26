// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cluster_scaling.go 是集群扩缩容管理（roadmap 11.11 方案 A-⑤）：
//   - ClusterConfig：集群节点配置段（cluster.xxx；空 = 单节点零回归）；
//   - NodeRegistry：节点注册表（StateStore nodes/<id> key + CAS 加入 + 状态机）；
//   - 装配校验：replica 必须外部卷 + 非 local state_store；master 租约冲突 fail-fast。
//
// 零回归：cluster.enabled 缺省 false → 全部校验/注册跳过（单节点照旧）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/state"
)

// ClusterRole 是节点角色。
const (
	ClusterRoleMaster  = "master"
	ClusterRoleReplica = "replica"
)

// 节点状态机合法跳转（joining → active → draining → leaving；active ↔ draining）。
var nodeStatusTransitions = map[string]map[string]bool{
	"joining":  {"active": true},
	"active":   {"draining": true},
	"draining": {"active": true, "leaving": true},
	"leaving":  {},
}

// ClusterConfig 是集群节点配置（cluster 段）。空 = 单节点（零回归）。
type ClusterConfig struct {
	Enabled        bool          `yaml:"enabled" mapstructure:"enabled"`
	NodeID         string        `yaml:"node_id" mapstructure:"node_id"`                             // 必填（Enabled 时）；跨节点唯一
	Role           string        `yaml:"role" mapstructure:"role"`                                   // master | replica；默认 master
	ResyncInterval time.Duration `yaml:"index_resync_interval" mapstructure:"index_resync_interval"` // 索引 resync 兜底周期（默认 5m；0 = 关闭）
}

// Validate 校验集群段（未启用 → 恒通过零回归）。
func (c ClusterConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.NodeID == "" {
		return errors.New("cluster: enabled 时 node_id 必填")
	}
	if c.Role != ClusterRoleMaster && c.Role != ClusterRoleReplica {
		return fmt.Errorf("cluster: role 必须为 %q 或 %q（got %q）", ClusterRoleMaster, ClusterRoleReplica, c.Role)
	}
	if c.ResyncInterval < 0 {
		return errors.New("cluster: index_resync_interval 不能为负")
	}
	return nil
}

// nodeEntry 是节点注册表条目。
type nodeEntry struct {
	Role     string `json:"role"`
	Status   string `json:"status"` // joining|active|draining|leaving
	JoinedAt int64  `json:"joined_at"`
	LastSeen int64  `json:"last_seen"`
}

// NodeRegistry 是节点注册表（StateStore nodes/<id> 承载）。
type NodeRegistry struct {
	st     state.StateStore
	logger *slog.Logger
	prefix string
}

// NewNodeRegistry 构造注册表。
func NewNodeRegistry(st state.StateStore, logger *slog.Logger) *NodeRegistry {
	return &NodeRegistry{st: st, logger: logger, prefix: "nodes/"}
}

// Join 注册节点（CAS joining→active：防同 id 双进程并发加入）。
func (r *NodeRegistry) Join(ctx context.Context, nodeID, role string) error {
	key := r.prefix + nodeID
	now := time.Now().UnixNano()
	joining, _ := json.Marshal(nodeEntry{Role: role, Status: "joining", JoinedAt: now, LastSeen: now})
	active, _ := json.Marshal(nodeEntry{Role: role, Status: "active", JoinedAt: now, LastSeen: now})
	// CAS：期望不存在（create-only）→ 成功；已存在 → ErrCASMismatch（身份冲突）。
	if err := r.st.CAS(ctx, key, nil, joining); err != nil {
		return fmt.Errorf("cluster: 节点 %s 注册失败（同 id 冲突？）: %w", nodeID, err)
	}
	// joining → active（CAS 防并发改）
	if err := r.st.CAS(ctx, key, joining, active); err != nil {
		// 竞态：另一进程已接管 → 清理本进程写入
		_ = r.st.Delete(ctx, key)
		return fmt.Errorf("cluster: 节点 %s 加入仲裁失败: %w", nodeID, err)
	}
	return nil
}

// SetStatus 更新节点状态（状态机校验非法跳转）。
func (r *NodeRegistry) SetStatus(ctx context.Context, nodeID, status string) error {
	key := r.prefix + nodeID
	data, err := r.st.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("cluster: 节点 %s 不存在: %w", nodeID, err)
	}
	var e nodeEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return fmt.Errorf("cluster: 节点 %s 条目损坏: %w", nodeID, err)
	}
	if !nodeStatusTransitions[e.Status][status] {
		return fmt.Errorf("cluster: 节点 %s 状态跳转非法 %q → %q", nodeID, e.Status, status)
	}
	e.Status = status
	e.LastSeen = time.Now().UnixNano()
	newData, _ := json.Marshal(e)
	return r.st.Put(ctx, key, newData)
}

// List 返回全部节点（/api/cluster/nodes 用）。
func (r *NodeRegistry) List(ctx context.Context) map[string]nodeEntry {
	out := map[string]nodeEntry{}
	keys, err := r.st.List(ctx, r.prefix)
	if err != nil {
		return out
	}
	for _, k := range keys {
		data, err := r.st.Get(ctx, k)
		if err != nil {
			continue
		}
		var e nodeEntry
		if err := json.Unmarshal(data, &e); err != nil {
			continue
		}
		out[k[len(r.prefix):]] = e
	}
	return out
}

// Get 返回单节点（/api/cluster/self 用）。
func (r *NodeRegistry) Get(ctx context.Context, nodeID string) (nodeEntry, error) {
	data, err := r.st.Get(ctx, r.prefix+nodeID)
	if err != nil {
		return nodeEntry{}, err
	}
	var e nodeEntry
	if err := json.Unmarshal(data, &e); err != nil {
		return nodeEntry{}, err
	}
	return e, nil
}

// handleClusterNodes 是 GET /api/cluster/nodes（受认证；未装配 → 400）。
func (h *Handlers) handleClusterNodes(w http.ResponseWriter, r *http.Request) {
	if h.nodeRegistry == nil {
		writeAIError(w, http.StatusBadRequest, "集群未启用")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"nodes": h.nodeRegistry.List(r.Context()),
	})
}

// handleClusterSelf 是 GET /api/cluster/self（本节点信息）。
func (h *Handlers) handleClusterSelf(w http.ResponseWriter, r *http.Request) {
	if h.cfgPtr == nil {
		writeAIError(w, http.StatusBadRequest, "集群未启用")
		return
	}
	cfg := h.cfgPtr.Load()
	if cfg == nil || !cfg.Cluster.Enabled {
		writeAIError(w, http.StatusBadRequest, "集群未启用")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"node_id": cfg.Cluster.NodeID,
		"role":    cfg.Cluster.Role,
	})
}
