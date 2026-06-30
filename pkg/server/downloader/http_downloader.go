// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// HTTPDownloader 是内置 HTTP/HTTPS 下载器。
type HTTPDownloader struct {
	httpClient *http.Client
}

// 确保 HTTPDownloader 实现了 Downloader 接口。
var _ Downloader = (*HTTPDownloader)(nil)

// Download 从 HTTP/HTTPS URL 下载文件到 destPath。
// 调用方应在调用前通过 ValidateURLHost 校验 URL 安全性。
// http.Client 的 CheckRedirect 提供额外的防御层。
func (d *HTTPDownloader) Download(ctx context.Context, source string, destPath string, onProgress ProgressFunc) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := d.getClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return nil, fmt.Errorf("create file: %w", err)
	}
	defer f.Close()

	h := sha256.New()
	tee := io.TeeReader(resp.Body, h)

	var totalSize int64
	if resp.ContentLength > 0 {
		totalSize = resp.ContentLength
	} else {
		totalSize = -1
	}

	// 写入带进度回调的文件
	buf := make([]byte, 32*1024)
	var downloaded int64
	for {
		n, readErr := tee.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				return nil, fmt.Errorf("write file: %w", err)
			}
			downloaded += int64(n)
			if onProgress != nil {
				onProgress(downloaded, totalSize)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, fmt.Errorf("read body: %w", readErr)
		}
	}

	checksum := hex.EncodeToString(h.Sum(nil))
	return &Result{Size: downloaded, Checksum: checksum}, nil
}

// Supports 判断是否支持 HTTP/HTTPS 协议。
func (d *HTTPDownloader) Supports(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// Name 返回下载器名称。
func (d *HTTPDownloader) Name() string { return "http" }

// getClient 返回 HTTP 客户端，惰性初始化。
// CheckRedirect 提供 SSRF 重定向保护（防御深度）。
// 入口层 ValidateURLHost 已阻止内部地址，此处防止重定向到内部地址。
func (d *HTTPDownloader) getClient() *http.Client {
	if d.httpClient != nil {
		return d.httpClient
	}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: safeCheckRedirect(),
	}
}
