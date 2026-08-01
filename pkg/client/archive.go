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
	if len(files) == 0 {
		return fmt.Errorf("archive: files 列表不能为空")
	}
	if outputPath == "" {
		return fmt.Errorf("archive: outputPath 不能为空")
	}
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
	if dirname == "" {
		return fmt.Errorf("archive dir: dirname 不能为空")
	}
	if outputPath == "" {
		return fmt.Errorf("archive dir: outputPath 不能为空")
	}
	path := "/api/archive-dir?dirname=" + url.QueryEscape(dirname)
	return c.downloadToFile(ctx, http.MethodGet, path, nil, nil, outputPath)
}

// downloadToFile 执行 HTTP 请求并将响应体保存到本地文件。
// 使用原子写入模式：先写入 .tmp 临时文件，成功后再重命名为目标路径。
// 如果请求失败或写入失败，自动清理不完整文件。
func (c *FileClient) downloadToFile(ctx context.Context, method, urlPath string, body io.Reader, headers http.Header, outputPath string) error {
	if outputPath == "" {
		return fmt.Errorf("输出路径不能为空")
	}
	if containsPathTraversal(outputPath) {
		return fmt.Errorf("输出路径包含非法路径穿越: %s", outputPath)
	}
	resp, err := c.doRequest(ctx, method, urlPath, body, headers)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("请求失败 (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	tmpPath := outputPath + ".tmp"
	if ensureErr := ensureParentDir(outputPath); ensureErr != nil {
		return fmt.Errorf("创建输出目录失败: %w", ensureErr)
	}
	out, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}

	var src io.Reader = resp.Body
	if c.progressFn != nil {
		c.progressFn("下载", 0, resp.ContentLength)
		src = NewProgressReader(resp.Body, resp.ContentLength, func(read, total int64) {
			c.progressFn("下载", read, total)
		})
	}
	if _, copyErr := io.Copy(out, src); copyErr != nil {
		_ = out.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("写入文件失败: %w", copyErr)
	}
	if closeErr := out.Close(); closeErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("关闭文件失败: %w", closeErr)
	}
	if err = os.Rename(tmpPath, outputPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("重命名文件失败: %w", err)
	}
	return nil
}
