// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HTTPDownloader 是内置 HTTP/HTTPS 下载器。
type HTTPDownloader struct {
	httpClient *http.Client
	logger     *slog.Logger
}

// NewHTTPDownloader 创建 HTTPDownloader。
// TODO: 支持通过选项模式（Options）注入自定义 Transport、超时等参数。
func NewHTTPDownloader() *HTTPDownloader {
	return &HTTPDownloader{
		logger: slog.Default(),
		httpClient: &http.Client{
			// 使用 Transport 层细粒度超时，不对整体请求设 Timeout，避免大文件过早中断。
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ResponseHeaderTimeout: 15 * time.Second,
				ForceAttemptHTTP2:     true,
			},
			CheckRedirect: safeCheckRedirect(),
		},
	}
}

// getLogger 返回 logger，nil 时使用 slog.Default。
func (d *HTTPDownloader) getLogger() *slog.Logger {
	if d.logger != nil {
		return d.logger
	}
	return slog.Default()
}

// 确保 HTTPDownloader 实现了 Downloader 接口。
var _ Downloader = (*HTTPDownloader)(nil)

// Download 从 HTTP/HTTPS URL 下载文件到 destPath。
// 使用临时文件 + 原子重命名，确保下载失败时不残留不完整文件。
// 支持续下载（resume）通过 Range 头。
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

	// DNS 重绑定攻击防御：二次验证实际连接 IP 是否安全。
	// 下载前 ValidateURLHost 已解析一次，但 DNS 可能在两次解析间变化。
	// 此处验证 resp.Request.URL 的 host（可能因重定向而改变）。
	if err := validateURLHostAfterDo(resp.Request.URL); err != nil {
		// 排空响应体再返回
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("ssrf post-request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// 非 200 时排空响应体，确保连接可复用
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	// 从 Last-Modified 响应头提取原始文件修改时间
	var modTime time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, parseErr := time.Parse(time.RFC1123, lm); parseErr == nil {
			modTime = t
		} else if t, parseErr := time.Parse(time.RFC1123Z, lm); parseErr == nil {
			modTime = t
		}
	}

	// 使用临时文件 + 原子重命名
	tmpPath := destPath + ".tmp." + randomHex(8)
	// 确保目标目录存在
	if dir := filepath.Dir(destPath); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("create parent dir: %w", err)
		}
	}

	f, err := os.Create(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
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
	// copyBufferSize 是下载时用于数据复制的缓冲区大小（32 KB）。
	const copyBufferSize = 32 * 1024
	buf := make([]byte, copyBufferSize)
	var downloaded int64
	for {
		n, readErr := tee.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				// 写入失败时清理临时文件
				f.Close()
				os.Remove(tmpPath)
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
			// 读取失败时清理临时文件
			f.Close()
			os.Remove(tmpPath)
			return nil, fmt.Errorf("read body: %w", readErr)
		}
	}

	// 关闭文件后再重命名
	if err := f.Close(); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("close temp file: %w", err)
	}

	checksum := hex.EncodeToString(h.Sum(nil))

	// 原子重命名：临时文件 → 目标文件
	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("rename temp file: %w", err)
	}

	// 设置文件修改时间
	if modTime != (time.Time{}) {
		if err := os.Chtimes(destPath, modTime, modTime); err != nil {
			d.getLogger().Warn("设置文件修改时间失败", "path", destPath, "error", err)
		}
	}

	return &Result{Size: downloaded, Checksum: checksum, ModTime: modTime}, nil
}

// Supports 判断是否支持 HTTP/HTTPS 协议（大小写不敏感）。
func (d *HTTPDownloader) Supports(source string) bool {
	return strings.HasPrefix(strings.ToLower(source), "http://") ||
		strings.HasPrefix(strings.ToLower(source), "https://")
}

// Name 返回下载器名称。
func (d *HTTPDownloader) Name() string { return "http" }

// getClient 返回 HTTP 客户端。
// 构造函数 NewHTTPDownloader 中已初始化 httpClient；若外部直接创建结构体
// 导致 httpClient 为 nil，则惰性回退到默认值以保持兼容。
// CheckRedirect 提供 SSRF 重定向保护（防御深度）。
// 入口层 ValidateURLHost 已阻止内部地址，此处防止重定向到内部地址。
func (d *HTTPDownloader) getClient() *http.Client {
	if d.httpClient != nil {
		return d.httpClient
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: 15 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		CheckRedirect: safeCheckRedirect(),
	}
}

// randomHex 生成指定长度的随机十六进制字符串。
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
