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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HTTPDownloader 是内置 HTTP/HTTPS 下载器。
type HTTPDownloader struct {
	httpClient *http.Client
	logger     *slog.Logger
	// ValidateURLAfterDo 在 HTTP 请求完成后二次验证最终 URL 的 IP 是否安全。
	// 为 nil 时跳过验证（用于测试）。
	ValidateURLAfterDo func(u *url.URL) error
	// Timeout 是单次下载的整体超时时间。0 表示不限制。
	Timeout time.Duration
}

// NewHTTPDownloader 创建 HTTPDownloader。
// TODO: 支持通过选项模式（Options）注入自定义 Transport、超时等参数。
func NewHTTPDownloader() *HTTPDownloader {
	return newHTTPDownloaderWithClient(nil)
}

// newHTTPDownloaderWithClient 创建 HTTPDownloader，使用指定的 http.Client。
// client 为 nil 时使用默认客户端。
func newHTTPDownloaderWithClient(client *http.Client) *HTTPDownloader {
	d := &HTTPDownloader{
		logger: slog.Default(),
	}
	if client != nil {
		d.httpClient = client
	} else {
		d.httpClient = &http.Client{
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
		}
	}
	d.ValidateURLAfterDo = validateURLHostAfterDo
	return d
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
// 支持续下载（resume）通过 Range 头。
// 调用方应在调用前通过 ValidateURLHost 校验 URL 安全性。
// http.Client 的 CheckRedirect 提供额外的防御层。
func (d *HTTPDownloader) Download(ctx context.Context, source string, destPath string, onProgress ProgressFunc) (*Result, error) {
	// 应用超时配置
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}

	// 检查是否存在部分文件，用于 Range 续传
	partialPath := destPath + ".partial"
	var existingSize int64
	if fi, err := os.Stat(partialPath); err == nil && fi.Size() > 0 {
		existingSize = fi.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "sproxy-cloud-downloader/1.0")

	// 如果存在部分文件，添加 Range 请求头
	if existingSize > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingSize))
	}

	resp, err := d.getClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	// DNS 重绑定攻击防御：二次验证实际连接 IP 是否安全。
	// 下载前 ValidateURLHost 已解析一次，但 DNS 可能在两次解析间变化。
	// 此处验证 resp.Request.URL 的 host（可能因重定向而改变）。
	if fn := d.ValidateURLAfterDo; fn != nil {
		if err2 := fn(resp.Request.URL); err2 != nil {
			// 排空响应体再返回
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil, fmt.Errorf("ssrf post-request: %w", err2)
		}
	}

	// 处理 Range 续传响应
	if existingSize > 0 {
		switch resp.StatusCode {
		case http.StatusPartialContent:
			return d.handleRangeResume(ctx, resp, partialPath, destPath, existingSize, onProgress)
		case http.StatusOK:
			// 服务端不支持 Range，删除部分文件，走全量下载路径
			os.Remove(partialPath)

		case http.StatusRequestedRangeNotSatisfiable:
			// 416，删除部分文件，走全量下载路径
			os.Remove(partialPath)

		default:
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil, fmt.Errorf("http status %d", resp.StatusCode)
		}
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
		if err2 := os.MkdirAll(dir, 0755); err2 != nil {
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

// handleRangeResume 处理 Range 续传场景：追加写入部分文件并校验。
func (d *HTTPDownloader) handleRangeResume(ctx context.Context, resp *http.Response, partialPath, destPath string, existingSize int64, onProgress ProgressFunc) (*Result, error) {
	// 验证 Content-Range 头
	cr := resp.Header.Get("Content-Range")
	if cr == "" {
		return nil, fmt.Errorf("missing Content-Range header for 206 response")
	}
	// 解析 Content-Range 格式: "bytes <start>-<end>/<total>"
	var startByte, endByte, totalSize int64
	if n, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &startByte, &endByte, &totalSize); err != nil || n != 3 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("invalid Content-Range header: %q", cr)
	}
	if startByte != existingSize {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("Content-Range start %d does not match expected %d", startByte, existingSize)
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

	// 计算已有部分文件的 SHA-256
	partialData, err := os.ReadFile(partialPath)
	if err != nil {
		return nil, fmt.Errorf("read partial file: %w", err)
	}

	// 创建统一的 hasher，写入已有部分数据
	h := sha256.New()
	h.Write(partialData)

	// 以追加模式打开部分文件
	f, err := os.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open partial file for append: %w", err)
	}
	defer f.Close()

	// 使用 TeeReader 同时计算追加部分的 SHA-256
	tee := io.TeeReader(resp.Body, h)

	const copyBufferSize = 32 * 1024
	buf := make([]byte, copyBufferSize)
	var downloaded int64
	for {
		n, readErr := tee.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return nil, fmt.Errorf("write to partial file: %w", err)
			}
			downloaded += int64(n)
			if onProgress != nil {
				onProgress(existingSize+downloaded, totalSize)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			f.Close()
			return nil, fmt.Errorf("read body: %w", readErr)
		}
	}

	// 关闭文件
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close partial file: %w", err)
	}

	// 计算完整文件的 SHA-256（已有部分 + 新增部分已在同一个 hasher 中）
	fullChecksum := hex.EncodeToString(h.Sum(nil))

	// 重命名部分文件为目标文件
	if err := os.Rename(partialPath, destPath); err != nil {
		return nil, fmt.Errorf("rename partial to dest: %w", err)
	}

	// 设置文件修改时间
	if modTime != (time.Time{}) {
		if err := os.Chtimes(destPath, modTime, modTime); err != nil {
			d.getLogger().Warn("设置文件修改时间失败", "path", destPath, "error", err)
		}
	}

	downloadedTotal := existingSize + downloaded
	return &Result{Size: downloadedTotal, Checksum: fullChecksum, ModTime: modTime}, nil
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
