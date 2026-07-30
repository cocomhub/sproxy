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
	body, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		return fmt.Errorf("序列化请求失败: %w", err)
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")

	return c.downloadToFile(ctx, http.MethodPost, "/api/archive", bytes.NewReader(body), headers, outputPath)
}

// ArchiveDir 将服务器端指定目录打包下载到本地文件。
func (c *FileClient) ArchiveDir(ctx context.Context, dirname, outputPath string) error {
	path := "/api/archive-dir?dirname=" + url.QueryEscape(dirname)
	return c.downloadToFile(ctx, http.MethodGet, path, nil, nil, outputPath)
}

// downloadToFile 执行 HTTP 请求并将响应体保存到本地文件。
// 如果请求失败或写入失败，自动清理不完整文件。
func (c *FileClient) downloadToFile(ctx context.Context, method, urlPath string, body io.Reader, headers http.Header, outputPath string) error {
	resp, err := c.doRequest(ctx, method, urlPath, body, headers)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("请求失败 (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建输出文件失败: %w", err)
	}
	defer out.Close()

	if _, err = io.Copy(out, resp.Body); err != nil {
		os.Remove(outputPath)
		return fmt.Errorf("写入文件失败: %w", err)
	}
	return nil
}
