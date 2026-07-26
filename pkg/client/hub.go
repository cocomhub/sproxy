// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
)

// HubNodeInfo 表示 Hub 中继节点信息。
type HubNodeInfo struct {
	ID        string `json:"id"`
	Addr      string `json:"addr,omitempty"`
	Connected string `json:"connected,omitempty"`
}

// HubStats 表示 Hub 中继统计信息。
type HubStats struct {
	NodesConnected int `json:"nodes_connected"`
}

// UpdateStorageConfig 更新服务端存储配置（运行时调整存储上限）。
func (c *FileClient) UpdateStorageConfig(ctx context.Context, maxStorageBytes int64) error {
	body := map[string]any{"max_storage_bytes": maxStorageBytes}
	var result struct {
		Success bool `json:"success"`
	}
	if err := c.doJSON(ctx, http.MethodPut, "/api/storage/config", body, &result); err != nil {
		return fmt.Errorf("更新存储配置失败: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("更新存储配置失败: 服务端返回 success=false")
	}
	return nil
}

// ListHubNodes 列出 Hub 中继节点列表。
func (c *FileClient) ListHubNodes(ctx context.Context) ([]HubNodeInfo, error) {
	var nodes []HubNodeInfo
	if err := c.doJSON(ctx, http.MethodGet, "/api/hub/nodes", nil, &nodes); err != nil {
		return nil, fmt.Errorf("获取 Hub 节点列表失败: %w", err)
	}
	return nodes, nil
}

// RemoveHubNode 移除指定 Hub 中继节点。
func (c *FileClient) RemoveHubNode(ctx context.Context, nodeID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/hub/nodes/"+nodeID, nil, nil)
}

// GetHubStats 获取 Hub 中继统计信息。
func (c *FileClient) GetHubStats(ctx context.Context) (*HubStats, error) {
	var stats HubStats
	if err := c.doJSON(ctx, http.MethodGet, "/api/hub/stats", nil, &stats); err != nil {
		return nil, fmt.Errorf("获取 Hub 统计信息失败: %w", err)
	}
	return &stats, nil
}
