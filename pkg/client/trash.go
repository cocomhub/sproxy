// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// TrashItem 是服务端 /api/trash 列表项（对齐 pkg/server/trash.go trashEntry JSON）。
type TrashItem struct {
	TrashRel string `json:"trash_rel"`
	Name     string `json:"name"`
}

// TrashListResponse 是 GET /api/trash 的响应容器（服务端返回 {entries: [...]}）。
type TrashListResponse struct {
	Entries []TrashItem `json:"entries"`
}

// ListTrash 列出回收站条目（roadmap 11.8-A2）：GET /api/trash。
func (c *FileClient) ListTrash(ctx context.Context) ([]TrashItem, error) {
	var list TrashListResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/trash", nil, &list); err != nil {
		return nil, fmt.Errorf("列举回收站: %w", err)
	}
	return list.Entries, nil
}

// RestoreTrash 恢复回收站条目（POST /api/trash/restore，trash_rel 令牌）。
func (c *FileClient) RestoreTrash(ctx context.Context, trashRel string) error {
	if trashRel == "" {
		return fmt.Errorf("回收站: trash_rel 不能为空")
	}
	body := map[string]string{"trash_rel": trashRel}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/trash/restore", body, &resp); err != nil {
		return fmt.Errorf("恢复回收站条目: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("恢复回收站条目: 服务端返回 success=false: %s", resp.Message)
	}
	return nil
}

// EmptyTrash 清空回收站（POST /api/trash/empty）。
func (c *FileClient) EmptyTrash(ctx context.Context) error {
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/trash/empty", nil, &resp); err != nil {
		return fmt.Errorf("清空回收站: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("清空回收站: 服务端返回 success=false: %s", resp.Message)
	}
	return nil
}

// QuotaStats 是 /api/stats quota 段（roadmap 11.8-A3：usage/max_bytes/watermark）。
type QuotaStats struct {
	Usage     int64 `json:"usage"`
	MaxBytes  int64 `json:"max_bytes"`
	Watermark int   `json:"watermark"`
}

// GetQuota 查询本 owner 配额水位（GET /api/stats，取 quota 段）。
func (c *FileClient) GetQuota(ctx context.Context) (*QuotaStats, error) {
	var stats StatsResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/stats", nil, &stats); err != nil {
		return nil, fmt.Errorf("获取配额: %w", err)
	}
	return stats.Quota, nil
}

// url 引用（unused 防 vet）。
var _ = url.PathEscape
var _ = json.Marshal
