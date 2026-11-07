// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// RebuildChecksums 请求服务端从对象 meta sidecar 重建指定 owner 的 checksum 台账
// （运维修复面 `POST /api/checksums/rebuild`），返回回填条数。owner 为空 ⇒ 服务端按请求主体。
func (c *FileClient) RebuildChecksums(ctx context.Context, owner string) (int, error) {
	urlPath := "/api/checksums/rebuild"
	if owner != "" {
		urlPath += "?owner=" + url.QueryEscape(owner)
	}
	var resp struct {
		Success bool   `json:"success"`
		Owner   string `json:"owner"`
		Entries int    `json:"entries"`
		Message string `json:"message"`
	}
	if err := c.doJSON(ctx, http.MethodPost, urlPath, struct{}{}, &resp); err != nil {
		return 0, fmt.Errorf("重建 checksum 台账失败: %w", err)
	}
	if !resp.Success {
		return 0, fmt.Errorf("重建 checksum 台账失败: %s", resp.Message)
	}
	return resp.Entries, nil
}
