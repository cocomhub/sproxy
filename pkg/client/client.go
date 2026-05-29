// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// UploadResult 表示上传操作的响应结果。
type UploadResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Checksum string `json:"file_checksum,omitempty"`
}

type serverResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	FileCS  string `json:"file_checksum"`
}

// ProgressReader 是一个带进度回调的 io.ReadCloser 包装。
type ProgressReader struct {
	reader     io.Reader
	total      int64
	read       int64
	onProgress func(read, total int64)
}

// NewProgressReader 创建进度读取器。total <= 0 表示未知长度。
func NewProgressReader(reader io.Reader, total int64, onProgress func(read, total int64)) *ProgressReader {
	return &ProgressReader{
		reader:     reader,
		total:      total,
		onProgress: onProgress,
	}
}

func (pr *ProgressReader) Read(p []byte) (int, error) {
	n, err := pr.reader.Read(p)
	pr.read += int64(n)
	if pr.onProgress != nil && n > 0 {
		pr.onProgress(pr.read, pr.total)
	}
	return n, err
}

// Option 是 FileClient 构造选项。
type Option func(*FileClient)

// FileClient 是 sproxy 文件服务和加密隧道的 Go 客户端。
//
// 使用方式：
//
//	client := NewFileClient("http://localhost:18083")
//	result, err := client.Upload(ctx, "file.txt")
//	err := client.Download(ctx, "file.txt", "/tmp/file.txt")
type FileClient struct {
	serverURL     string
	httpClient    *http.Client
	tunnelClient  *tunnel.Client
	checkChecksum bool
	progressFn    func(label string, read, total int64)
	ChunkSize     int64
	logger        *slog.Logger
}

// NewFileClient 创建一个新的 sproxy 客户端。
//
// serverURL 是 sproxy 服务端地址，如 "http://localhost:18083"。
// 可以通过 Option 设置自定义 HTTP 客户端、隧道加密、超时等。
func NewFileClient(serverURL string, opts ...Option) *FileClient {
	c := &FileClient{
		serverURL:     strings.TrimRight(serverURL, "/"),
		httpClient:    &http.Client{Timeout: 300 * time.Second},
		checkChecksum: true,
		ChunkSize:     4 << 20, // 4 MiB
		logger:        slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithHTTPClient 设置自定义 HTTP 客户端。
func WithHTTPClient(hc *http.Client) Option {
	return func(c *FileClient) {
		c.httpClient = hc
	}
}

// WithTunnel 启用加密隧道传输，hexKey 需与 sproxy 服务端的 tunnel_key 一致。
func WithTunnel(hexKey string) Option {
	return func(c *FileClient) {
		tc, err := tunnel.NewClient(hexKey, c.serverURL+"/tunnel", c.httpClient.Timeout, c.logger)
		if err != nil {
			c.logger.Warn("创建隧道客户端失败", "error", err)
			return
		}
		c.tunnelClient = tc
	}
}

// WithTimeout 设置 HTTP 客户端超时。
func WithTimeout(d time.Duration) Option {
	return func(c *FileClient) {
		c.httpClient.Timeout = d
	}
}

// WithChecksum 设置是否启用文件校验。
func WithChecksum(enabled bool) Option {
	return func(c *FileClient) {
		c.checkChecksum = enabled
	}
}

// WithProgress 设置进度回调。label 是当前操作描述，read 是已处理字节数，total 是总字节数。
func WithProgress(fn func(label string, read, total int64)) Option {
	return func(c *FileClient) {
		c.progressFn = fn
	}
}

// WithLogger 设置 FileClient 内部使用的日志记录器。
// 当 logger 为 nil 时使用 slog.Default()。
func WithLogger(logger *slog.Logger) Option {
	return func(c *FileClient) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// calculateChecksum 计算文件的 SHA-256 十六进制摘要。
func calculateChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Upload 上传一个文件到 sproxy 服务端。
//
// 如果启用了 checksum 校验（默认开启），会在上传前计算文件的 SHA-256，
// 并通过 X-File-Checksum 请求头发送给服务端进行完整性校验。
// 如果配置了 tunnel_key，上传数据将通过加密隧道传输。
func (c *FileClient) Upload(ctx context.Context, filePath string) (*UploadResult, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("获取文件信息失败: %w", err)
	}
	fileSize := stat.Size()

	var fileChecksum string
	if c.checkChecksum {
		h := sha256.New()
		if _, err := io.Copy(h, file); err != nil {
			return nil, fmt.Errorf("计算 SHA-256 失败: %w", err)
		}
		fileChecksum = hex.EncodeToString(h.Sum(nil))
		c.logger.Debug("文件 SHA-256", "filepath", filePath, "checksum", fileChecksum)
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("重置文件指针失败: %w", err)
		}
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		defer pw.Close()
		defer mw.Close()
		part, err := mw.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			pw.CloseWithError(err)
			return
		}
		var src io.Reader = file
		if c.progressFn != nil {
			c.progressFn("上传", 0, fileSize)
			src = NewProgressReader(file, fileSize, func(read, total int64) {
				c.progressFn("上传", read, total)
			})
		}
		if _, err := io.Copy(part, src); err != nil {
			pw.CloseWithError(err)
			return
		}
	}()

	headers := make(http.Header)
	headers.Set("Content-Type", mw.FormDataContentType())
	if c.checkChecksum && fileChecksum != "" {
		headers.Set("X-File-Checksum", fileChecksum)
	}

	resp, err := c.doRequest(ctx, "POST", "/upload", pr, headers)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	// 非 2xx 不要直接当 JSON 解，避免把 "unauthorized" 之类的纯文本当成 success=false 静默吞掉
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("上传失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result UploadResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if !result.Success {
		return &result, fmt.Errorf("上传失败: %s", result.Message)
	}

	return &result, nil
}

// Download 从 sproxy 服务端下载文件并保存到本地。
//
// outputPath 指定本地保存路径；为空时使用 filename。
// 如果启用了 checksum 校验（默认开启），会在下载后验证服务端返回的 X-File-Checksum。
// 如果配置了 tunnel_key，下载数据将通过加密隧道传输。
func (c *FileClient) Download(ctx context.Context, filename, outputPath string) error {
	if outputPath == "" {
		outputPath = filename
	}

	urlPath := "/download?" + url.Values{"filename": {filename}}.Encode()
	headers := make(http.Header)

	resp, err := c.doRequest(ctx, "GET", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("下载失败 (状态码: %d): %s", resp.StatusCode, string(body))
	}

	// 从响应解析收到的 checksum（服务端在 X-File-Checksum 返回）
	serverCS := resp.Header.Get("X-File-Checksum")
	contentLength := resp.ContentLength

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer out.Close()

	var src io.Reader = resp.Body
	if c.progressFn != nil {
		c.progressFn("下载", 0, contentLength)
		src = NewProgressReader(resp.Body, contentLength, func(read, total int64) {
			c.progressFn("下载", read, total)
		})
	}
	if _, err := io.Copy(out, src); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	if c.checkChecksum && serverCS != "" {
		c.logger.Debug("下载文件校验", "filename", outputPath, "server_checksum", serverCS)
		localCS, err := calculateChecksum(outputPath)
		if err != nil {
			return fmt.Errorf("计算本地 SHA-256 失败: %w", err)
		}
		if serverCS != localCS {
			return fmt.Errorf("文件校验失败: 服务端 %s, 本地 %s", serverCS, localCS)
		}
		c.logger.Debug("文件校验通过", "checksum", serverCS)
	}

	return nil
}

// Delete 从 sproxy 服务端删除文件。
//
// 如果启用了 checksum 校验（默认开启），会先计算本地文件的 SHA-256，
// 通过 X-File-Checksum 请求头发送给服务端进行身份验证。
// 注意：你需要有本地文件副本才能删除。
// 如果配置了 tunnel_key，删除请求将通过加密隧道传输。
func (c *FileClient) Delete(ctx context.Context, filename string) error {
	urlPath := "/delete?" + url.Values{"filename": {filename}}.Encode()
	headers := make(http.Header)

	if c.checkChecksum {
		fileChecksum, err := calculateChecksum(filename)
		if err != nil {
			return fmt.Errorf("计算文件 SHA-256 失败: %w", err)
		}
		headers.Set("X-File-Checksum", fileChecksum)
	}

	resp, err := c.doRequest(ctx, "POST", urlPath, nil, headers)
	if err != nil {
		return fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("删除失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result serverResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}

	if !result.Success {
		return fmt.Errorf("删除失败: %s", result.Message)
	}

	return nil
}

// FileInfo 表示远端单个文件的元信息（与服务端 listFiles 响应对齐）。
type FileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
}

// List 列出 sproxy 服务端上的文件，返回 name + size + checksum 的结构化列表。
//
// 如果配置了 tunnel_key，列表请求将通过加密隧道传输。
func (c *FileClient) List(ctx context.Context) ([]FileInfo, error) {
	headers := make(http.Header)
	resp, err := c.doRequest(ctx, "GET", "/api/files", nil, headers)
	if err != nil {
		return nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("列出文件失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Files []FileInfo `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	return result.Files, nil
}

// TunnelDo 通过加密隧道发送一个 HTTP 请求。
//
// 使用方式与标准 http.Client.Do 相同。需要先通过 WithTunnel 配置隧道密钥。
// 如果未配置隧道密钥，返回错误。
func (c *FileClient) TunnelDo(req *http.Request) (*http.Response, error) {
	if c.tunnelClient == nil {
		return nil, fmt.Errorf("未配置隧道密钥，请使用 WithTunnel 选项创建 FileClient")
	}
	return c.tunnelClient.Do(req)
}

// doRequest 统一发送 HTTP 请求：当配置了隧道客户端时走加密隧道，否则直连。
//
// urlPath 是相对路径，如 "/upload" 或 "/download?filename=test.txt"。
// 隧道模式下 URL 保持相对路径，由服务端隧道 handler 本地路由；
// 直连模式下拼接 serverURL + urlPath 构造完整 URL。
func (c *FileClient) doRequest(ctx context.Context, method, urlPath string, body io.Reader, headers http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, urlPath, body)
	if err != nil {
		return nil, fmt.Errorf("创建请求失败: %w", err)
	}
	for k, vals := range headers {
		for _, v := range vals {
			req.Header.Add(k, v)
		}
	}

	if c.tunnelClient != nil {
		// 隧道模式：使用相对 URL，隧道客户端处理加密
		return c.tunnelClient.Do(req)
	}

	// 直连模式：补全 server URL
	fullURL := c.serverURL + urlPath
	req.URL, err = url.Parse(fullURL)
	if err != nil {
		return nil, fmt.Errorf("解析 URL 失败: %w", err)
	}
	return c.httpClient.Do(req)
}
