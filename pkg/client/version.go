// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	apiPath := "/api/versions?filename=" + url.QueryEscape(filename)
	resp, err := c.doRequest(ctx, "GET", apiPath, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("获取版本列表失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("获取版本列表失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Versions []VersionInfo `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return result.Versions, nil
}

// RestoreVersion 恢复文件到指定版本。
func (c *FileClient) RestoreVersion(ctx context.Context, filename, versionID string) error {
	apiPath := fmt.Sprintf("/api/versions/restore?filename=%s&version_id=%s",
		url.QueryEscape(filename), url.QueryEscape(versionID))
	resp, err := c.doRequest(ctx, "POST", apiPath, nil, nil)
	if err != nil {
		return fmt.Errorf("恢复版本失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result UploadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("解析响应失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	if !result.Success {
		return fmt.Errorf("恢复失败: %s", result.Message)
	}
	return nil
}

// DeleteVersion 删除文件的指定版本。
func (c *FileClient) DeleteVersion(ctx context.Context, filename, versionID string) error {
	apiPath := fmt.Sprintf("/api/versions?filename=%s&version_id=%s",
		url.QueryEscape(filename), url.QueryEscape(versionID))
	resp, err := c.doRequest(ctx, "DELETE", apiPath, nil, nil)
	if err != nil {
		return fmt.Errorf("删除版本失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result UploadResult
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("解析响应失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}
	if !result.Success {
		return fmt.Errorf("删除失败: %s", result.Message)
	}
	return nil
}
