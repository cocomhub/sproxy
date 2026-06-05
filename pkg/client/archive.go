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
	"os"
)

// Archive 将服务器端指定的文件列表打包下载到本地文件。
// files: 服务端文件路径列表；outputPath: 本地目标 .tar.gz 文件路径。
func (c *FileClient) Archive(ctx context.Context, files []string, outputPath string) error {
	body, _ := json.Marshal(map[string]any{"files": files})

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")

	resp, err := c.doRequest(ctx, "POST", "/api/archive", bytes.NewReader(body), headers)
	if err != nil {
		return fmt.Errorf("归档请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("归档失败 (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建输出文件失败: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// ArchiveDir 将服务器端指定目录打包下载到本地文件。
func (c *FileClient) ArchiveDir(ctx context.Context, dirname, outputPath string) error {
	path := "/api/archive-dir?dirname=" + url.QueryEscape(dirname)
	resp, err := c.doRequest(ctx, "GET", path, nil, nil)
	if err != nil {
		return fmt.Errorf("归档请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("归档失败 (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建输出文件失败: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}
