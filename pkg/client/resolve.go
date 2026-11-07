// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// resolve.go 客户端侧的通用 URL 解析（GET /api/resolve）：
// 把「URL → 资源信息（大小/类型/文件清单）」能力提供给外部调用方（sdserver 等），
// 避免各处自行拼 HTTP 请求（签名/隧道/错误映射统一由 FileClient 处理）。
//
// 服务端能力单一实现在 pkg/downloader.URLResolver 扩展（pikpak 分享为其中一个扩展），
// 本方法只是 SDK 侧薄封装（不重复实现解析逻辑）。

import (
	"context"
	"fmt"
	neturl "net/url"
)

// ResolvedFile 是解析出的单个文件条目（与服务端 downloader.ResolvedFile JSON 对齐）。
type ResolvedFile struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	Type       string `json:"type,omitempty"`
	Hash       string `json:"hash,omitempty"`
	DirectLink string `json:"direct_link,omitempty"`
}

// ResolveInfo 是 URL 解析结果（与服务端 downloader.ResolveInfo JSON 对齐）。
type ResolveInfo struct {
	URL      string         `json:"url"`
	Kind     string         `json:"kind"`
	Filename string         `json:"filename,omitempty"`
	Size     int64          `json:"size,omitempty"`
	Files    []ResolvedFile `json:"files,omitempty"`
}

// ResolveURL 调 GET /api/resolve?url=... 解析任意 URL 的资源信息。
// 服务端按 URL 识别扩展（分享类返回多文件清单，未识别返回通用单文件信息）。
func (c *FileClient) ResolveURL(ctx context.Context, rawURL string) (*ResolveInfo, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("resolve: url 不能为空")
	}
	var out ResolveInfo
	path := "/api/resolve?url=" + neturl.QueryEscape(rawURL)
	if err := c.DoJSON(ctx, "GET", path, nil, &out); err != nil {
		return nil, fmt.Errorf("resolve url: %w", err)
	}
	return &out, nil
}
