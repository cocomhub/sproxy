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
	// Volume 是条目所在卷（多卷汇总视图；空 = 旧服务端）。
	Volume string `json:"volume,omitempty"`
}

// TrashListResponse 是 GET /api/trash 的响应容器（服务端返回 {entries: [...]}）。
type TrashListResponse struct {
	Entries []TrashItem `json:"entries"`
}

// ListTrash 列出回收站条目（roadmap 11.8-A2）：GET /api/trash[?volume=<卷>]。
// volume 空 = **汇总** owner 可见的全部本地卷（服务端缺省语义）。
func (c *FileClient) ListTrash(ctx context.Context, volume string) ([]TrashItem, error) {
	apiPath := "/api/trash"
	if volume != "" {
		apiPath += "?volume=" + url.QueryEscape(volume)
	}
	var list TrashListResponse
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &list); err != nil {
		return nil, fmt.Errorf("列举回收站: %w", err)
	}
	return list.Entries, nil
}

// RestoreTrash 恢复回收站条目（POST /api/trash/restore，trash_rel 令牌）。
// 契约：trash_rel 经 ?file= query 传递（pkg/server/trash.go restoreTrashHandler 权威；
// 放在 body 会得到 400 "file 不能为空"）。
func (c *FileClient) RestoreTrash(ctx context.Context, trashRel, volume string) error {
	if trashRel == "" {
		return fmt.Errorf("回收站: trash_rel 不能为空")
	}
	// volume 空 = 跨卷定位（服务端按条目所在卷查找；条目只存在于软删时所在卷）。
	path := "/api/trash/restore?file=" + url.QueryEscape(trashRel)
	if volume != "" {
		path += "&volume=" + url.QueryEscape(volume)
	}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := c.doJSON(ctx, http.MethodPost, path, nil, &resp); err != nil {
		return fmt.Errorf("恢复回收站条目: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("恢复回收站条目: 服务端返回 success=false: %s", resp.Message)
	}
	return nil
}

// EmptyTrash 清空回收站（POST /api/trash/empty[?volume=<卷>|?all=1]）。
//
// **安全默认**：volume 空且 all=false ⇒ 服务端只清**默认卷**（既有语义，破坏性默认不扩大）；
// 要清全部卷必须显式 all=true（CLI 用 --all，或 --volume 指定单卷）。
func (c *FileClient) EmptyTrash(ctx context.Context, volume string, all bool) error {
	apiPath := "/api/trash/empty"
	switch {
	case all:
		apiPath += "?all=1"
	case volume != "":
		apiPath += "?volume=" + url.QueryEscape(volume)
	}
	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := c.doJSON(ctx, http.MethodPost, apiPath, nil, &resp); err != nil {
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
