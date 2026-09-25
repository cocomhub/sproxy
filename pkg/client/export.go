// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// export.go 提供卷备份导出的客户端面（roadmap 11.7-5 / 11.3-② 配套）：
//
//	GET /api/volumes/export?volume=<name> → application/x-tar 流式响应 → 原子落盘
//	（tmp + Rename，与 Archive.downloadToFile 同语义；流式边读边写不整卷入内存）。
//
// 权限由服务端 fileRouteRead 收口（导出 = 读卷）；请求复用 doRequest（SproxySig
// 签名 / 隧道 / xfer 既有认证管线）。卷名为空 = 全卷视图（服务端导出 owner 全部
// 可见卷，与 download 的 auto 语义一致）。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
)

// ExportVolume 导出卷 tar 流到本地文件（GET /api/volumes/export）。
//
// volume 为空时导出 owner 全部可见卷（服务端无卷语义装配时导出默认租户 user 桶）；
// outputPath 为本地目标 .tar 文件路径（父目录自动创建；原子落盘，失败不残留半成品）。
func (c *FileClient) ExportVolume(ctx context.Context, volume, outputPath string) error {
	if outputPath == "" {
		return fmt.Errorf("输出路径不能为空")
	}
	if containsPathTraversal(outputPath) {
		return fmt.Errorf("输出路径包含非法路径穿越: %s", outputPath)
	}

	q := url.Values{}
	if volume != "" {
		q.Set("volume", volume)
	}
	urlPath := "/api/volumes/export"
	if enc := q.Encode(); enc != "" {
		urlPath += "?" + enc
	}

	resp, err := c.doRequest(ctx, http.MethodGet, urlPath, nil, nil)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		return fmt.Errorf("导出失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	// 原子落盘：先写 .tmp，成功后再 Rename（对齐 downloadToFile 的既有语义——
	// 请求失败/写入失败自动清理，不残留半成品）。
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
		return fmt.Errorf("写入导出流失败: %w", copyErr)
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
