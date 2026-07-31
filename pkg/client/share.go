// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// ShareOption 配置分享链接行为。
type ShareOption func(*shareOptions)

type shareOptions struct {
	ttl          time.Duration
	maxDownloads int
	oneTime      bool
}

func WithShareTTL(d time.Duration) ShareOption {
	return func(o *shareOptions) {
		if d > 0 {
			o.ttl = d
		}
	}
}

func WithShareMaxDownloads(n int) ShareOption {
	return func(o *shareOptions) {
		if n > 0 {
			o.maxDownloads = n
		}
	}
}

func WithShareOneTime() ShareOption {
	return func(o *shareOptions) {
		o.oneTime = true
	}
}

// ShareLink 表示服务端返回的分享链接信息。
type ShareLink struct {
	Token        string `json:"token"`         // 分享链接的唯一标识符
	Filename     string `json:"filename"`      // 被分享的文件名
	CreatedAt    string `json:"created_at"`    // 创建时间（RFC3339 格式）
	ExpiresAt    string `json:"expires_at"`    // 过期时间（RFC3339 格式）
	MaxDownloads int    `json:"max_downloads"` // 最大下载次数（0=不限）
	Downloads    int    `json:"downloads"`     // 已下载次数
	OneTime      bool   `json:"one_time"`      // 是否一次性链接
	Expired      bool   `json:"expired"`       // 是否已过期
}

// CreatedAtTime 返回 CreatedAt 的 time.Time 表示。
func (s *ShareLink) CreatedAtTime() (time.Time, error) {
	return time.Parse(time.RFC3339, s.CreatedAt)
}

// ExpiresAtTime 返回 ExpiresAt 的 time.Time 表示。
func (s *ShareLink) ExpiresAtTime() (time.Time, error) {
	return time.Parse(time.RFC3339, s.ExpiresAt)
}

// CreateShare 创建文件分享链接，支持 Option 模式配置参数。
func (c *FileClient) CreateShare(ctx context.Context, filename string, opts ...ShareOption) (*ShareLink, error) {
	if filename == "" {
		return nil, fmt.Errorf("filename 不能为空")
	}
	cfg := &shareOptions{
		ttl: 24 * time.Hour, // 默认 24 小时
	}
	for _, o := range opts {
		o(cfg)
	}
	reqBody := map[string]any{
		"filename":      filename,
		"ttl":           fmt.Sprintf("%ds", int64(cfg.ttl.Seconds())),
		"max_downloads": cfg.maxDownloads,
		"one_time":      cfg.oneTime,
	}

	var link ShareLink
	if err := c.doJSON(ctx, "POST", "/api/share", reqBody, &link); err != nil {
		return nil, fmt.Errorf("创建分享链接失败: %w", err)
	}
	return &link, nil
}

// ListShares 列出当前所有活跃的分享链接。
func (c *FileClient) ListShares(ctx context.Context) ([]ShareLink, error) {
	var result struct {
		Shares []ShareLink `json:"shares"`
	}
	if err := c.doJSON(ctx, "GET", "/api/shares", nil, &result); err != nil {
		return nil, fmt.Errorf("获取分享列表失败: %w", err)
	}
	return result.Shares, nil
}

// RevokeShare 撤销指定 token 的分享链接。
func (c *FileClient) RevokeShare(ctx context.Context, token string) error {
	if token == "" {
		return fmt.Errorf("token 不能为空")
	}
	apiPath := "/api/shares/" + url.PathEscape(token)
	var result doJSONResp
	if err := c.doJSON(ctx, "DELETE", apiPath, nil, &result); err != nil {
		return fmt.Errorf("撤销分享链接失败: %w", err)
	}
	return nil
}
