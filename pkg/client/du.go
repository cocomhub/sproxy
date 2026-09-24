// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// du.go 提供 FileClient 的目录空间统计（GET /api/du）：
// 服务端按 owner 卷视图递归统计指定目录子树（dirs/files/size），
// 客户端封装 Du() 返回 DuResult。零服务端字段回归（新增端点）。

package client

import (
	"context"
	"fmt"
	"net/url"
)

// DuResult 是 GET /api/du 的统计结果。
type DuResult struct {
	Path  string `json:"path"`
	Dirs  int    `json:"dirs"`
	Files int    `json:"files"`
	Size  int64  `json:"size"`
}

// Du 查询远端目录的递归空间统计（默认当前卷根）。
// path 为空表示卷根（user 桶）；非空时随 cd/卷上下文定位。
func (c *FileClient) Du(ctx context.Context, path string) (*DuResult, error) {
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	c.appendVolumeQuery(q)
	var resp struct {
		Success bool      `json:"success"`
		Message string    `json:"message,omitempty"`
		Data    *DuResult `json:"data"`
	}
	if err := c.doJSON(ctx, "GET", "/api/du?"+q.Encode(), nil, &resp); err != nil {
		return nil, fmt.Errorf("获取目录统计失败: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("获取目录统计失败: %s", resp.Message)
	}
	if resp.Data == nil {
		return nil, fmt.Errorf("获取目录统计失败: 服务端未返回数据")
	}
	return resp.Data, nil
}
