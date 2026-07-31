// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"fmt"
	"net/url"

	"context"
)

// VersionInfo 表示服务端返回的版本信息。
type VersionInfo struct {
	Filename  string `json:"filename"`
	VersionID int64  `json:"version_id"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ListVersions 返回指定文件的版本历史。
func (c *FileClient) ListVersions(ctx context.Context, filename string) ([]VersionInfo, error) {
	if filename == "" {
		return nil, fmt.Errorf("filename 不能为空")
	}
	apiPath := "/api/versions?" + url.Values{"filename": {filename}}.Encode()
	var result struct {
		Versions []VersionInfo `json:"versions"`
	}
	if err := c.doJSON(ctx, "GET", apiPath, nil, &result); err != nil {
		return nil, fmt.Errorf("获取版本列表失败: %w", err)
	}
	return result.Versions, nil
}

// RestoreVersion 恢复文件到指定版本。
func (c *FileClient) RestoreVersion(ctx context.Context, filename string, versionID int64) error {
	if filename == "" {
		return fmt.Errorf("filename 不能为空")
	}
	apiPath := "/api/versions/restore?" + url.Values{
		"filename":   {filename},
		"version_id": {fmt.Sprintf("%d", versionID)},
	}.Encode()
	var result doJSONResp
	if err := c.doJSON(ctx, "POST", apiPath, nil, &result); err != nil {
		return fmt.Errorf("恢复版本失败: %w", err)
	}
	return nil
}

// DeleteVersion 删除文件的指定版本。
func (c *FileClient) DeleteVersion(ctx context.Context, filename string, versionID int64) error {
	if filename == "" {
		return fmt.Errorf("filename 不能为空")
	}
	apiPath := "/api/versions?" + url.Values{
		"filename":   {filename},
		"version_id": {fmt.Sprintf("%d", versionID)},
	}.Encode()
	var result doJSONResp
	if err := c.doJSON(ctx, "DELETE", apiPath, nil, &result); err != nil {
		return fmt.Errorf("删除版本失败: %w", err)
	}
	return nil
}
