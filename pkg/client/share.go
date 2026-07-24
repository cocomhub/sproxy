// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ShareLink 表示服务端返回的分享链接信息。
type ShareLink struct {
	Token        string `json:"token"`
	Filename     string `json:"filename"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
	MaxDownloads int    `json:"max_downloads"`
	Downloads    int    `json:"downloads"`
	OneTime      bool   `json:"one_time"`
	Expired      bool   `json:"expired"`
}

// CreateShare 创建文件分享链接，返回分享链接信息。
func (c *FileClient) CreateShare(ctx context.Context, filename string, ttl time.Duration, maxDownloads int, oneTime bool) (*ShareLink, error) {
	body := map[string]any{
		"filename":      filename,
		"ttl":           ttl.String(),
		"max_downloads": maxDownloads,
		"one_time":      oneTime,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resp, err := c.doRequest(ctx, "POST", "/api/share", bytes.NewReader(jsonBody), headers)
	if err != nil {
		return nil, fmt.Errorf("创建分享链接失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var result UploadResult
		if json.Unmarshal(respBody, &result) == nil && result.Message != "" {
			return nil, fmt.Errorf("创建分享链接失败: %s", result.Message)
		}
		return nil, fmt.Errorf("创建分享链接失败 (HTTP %d)", resp.StatusCode)
	}

	var link ShareLink
	if err := json.Unmarshal(respBody, &link); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return &link, nil
}

// ListShares 列出当前所有活跃的分享链接。
func (c *FileClient) ListShares(ctx context.Context) ([]*ShareLink, error) {
	resp, err := c.doRequest(ctx, "GET", "/api/shares", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("获取分享列表失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("获取分享列表失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Shares []*ShareLink `json:"shares"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return result.Shares, nil
}

// RevokeShare 撤销指定 token 的分享链接。
func (c *FileClient) RevokeShare(ctx context.Context, token string) error {
	apiPath := "/api/shares/" + url.PathEscape(token)
	resp, err := c.doRequest(ctx, "DELETE", apiPath, nil, nil)
	if err != nil {
		return fmt.Errorf("撤销分享链接失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("撤销分享链接失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result UploadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("解析响应失败: %s", string(body))
	}
	if !result.Success {
		return fmt.Errorf("撤销失败: %s", result.Message)
	}
	return nil
}
