// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HTTPDownloader 是内置 HTTP/HTTPS 下载器。
type HTTPDownloader struct {
	httpClient *http.Client
	logger     *slog.Logger
	// ValidateURLAfterDo 在 HTTP 请求完成后二次验证最终 URL 的 IP 是否安全。
	// 为 nil 时跳过验证（用于测试）。
	ValidateURLAfterDo func(u *url.URL) error
	// Timeout 是单次下载尝试的整体超时时间。0 表示不限制。
	Timeout time.Duration
	// IdleTimeout 是响应体读取的空闲超时：超过该时长未读到任何数据
	// 即中断本次尝试（可重试/续传）。0 表示不限制。
	IdleTimeout time.Duration
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

// errRangeMismatch 表示服务端返回的 Range 续传信息与本地部分文件不一致。
// 调用方应删除部分文件并回退到全量下载。
var errRangeMismatch = errors.New("range resume mismatch, fallback to full download")

// etagPath 返回与 partial 文件对应的 ETag 缓存文件路径。
func etagPath(partialPath string) string {
	return partialPath + ".etag"
}

// saveETag 将 ETag 写入 companion 文件（etag 为空时删除文件），返回写盘错误。
// 写盘失败意味着下次续传退化为"仅 Range 无 If-Range"，无法再校验远程内容一致性，
// 服务端文件已变时会拼接出混合文件，因此调用方必须显式记录失败。
func saveETag(etagPath, etag string) error {
	if etag == "" {
		return os.Remove(etagPath)
	}
	return os.WriteFile(etagPath, []byte(etag), 0644)
}

// loadETag 从 companion 文件读取缓存的 ETag。文件不存在或读取失败返回空字符串。
func loadETag(etagPath string) string {
	data, err := os.ReadFile(etagPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// extractETag 从响应头提取 ETag（保留原始值，含引号）。
// 注：ETag 在 HTTP 头中通常以双引号包裹（如 "abc123"），
// 完整保留以便 If-Range 发送时服务端能正确识别。
func extractETag(resp *http.Response) string {
	return strings.TrimSpace(resp.Header.Get("ETag"))
}

// finalizeDownload 将 .partial 文件收尾为最终文件：
//   - rename 前保存 ETag 伴侣（供中断续传的 If-Range 一致性校验；写盘失败必须记录，
//     否则续传退化为无 If-Range，服务端内容已变时可能拼接出混合文件）
//   - rename 后删除伴侣，避免残留被 diskUsageOfTask 计入账本偏差
//   - 恢复原始 mtime
//
// 三条完成路径（writeFullBody/handleRangeResume/finalizePartial）必须共用本函数，
// 防止某条路径漏写/漏删 ETag 伴侣导致续传拼接损坏。
func (d *HTTPDownloader) finalizeDownload(partialPath, destPath string, modTime time.Time, etag string) error {
	if etag != "" {
		if err := saveETag(etagPath(partialPath), etag); err != nil {
			d.getLogger().Warn("保存 ETag 伴侣失败，续传将无 If-Range 一致性校验",
				"path", etagPath(partialPath), "error", err)
		}
	}
	if err := os.Remove(destPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove existing dest: %w", err)
	}
	if err := os.Rename(partialPath, destPath); err != nil {
		return fmt.Errorf("rename partial to dest: %w", err)
	}
	_ = os.Remove(etagPath(partialPath))
	if modTime != (time.Time{}) {
		if err := os.Chtimes(destPath, modTime, modTime); err != nil {
			d.getLogger().Warn("设置文件修改时间失败", "path", destPath, "error", err)
		}
	}
	return nil
}

// Download 从 HTTP/HTTPS URL 下载文件到 destPath。
// 支持续下载（resume）通过 Range 头：存在 destPath+".partial" 且服务端支持
// Range 时追加写入；服务端不支持或文件已变化时回退全量下载。
// 调用方应在调用前通过 ValidateURLHost 校验 URL 安全性。
// http.Client 的 CheckRedirect 提供额外的防御层。
func (d *HTTPDownloader) Download(ctx context.Context, source string, destPath string, onProgress ProgressFunc) (*Result, error) {
	// 应用整体超时配置
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}

	// 检查是否存在部分文件，用于 Range 续传
	partialPath := destPath + ".partial"
	var existingSize int64
	var cachedETag string
	if fi, err := os.Stat(partialPath); err == nil {
		if fi.Size() > 0 {
			existingSize = fi.Size()
			cachedETag = loadETag(etagPath(partialPath))
		} else {
			// 空的部分文件没有续传价值，直接清理（连同 ETag 伴侣文件）
			_ = os.Remove(partialPath)
			_ = os.Remove(etagPath(partialPath))
		}
	}

	// 第一次请求：存在部分文件时携带 Range 和 If-Range（如果有缓存的 ETag）
	resp, err := d.doGet(ctx, source, existingSize, cachedETag)
	if err != nil {
		// doGet 已按确定性/网络错误分类（RetryableError 与非重试错误）
		return nil, err
	}
	if err2 := d.validateAfterDo(resp); err2 != nil {
		return nil, err2
	}
	// 在 defer 前包 idleReadCloser，使正常路径的 Close 能停止空闲定时器
	resp.Body = newIdleReadCloser(resp.Body, d.IdleTimeout)
	defer resp.Body.Close()

	if existingSize > 0 {
		switch resp.StatusCode {
		case http.StatusPartialContent:
			result, rerr := d.handleRangeResume(ctx, resp, partialPath, destPath, existingSize, cachedETag, onProgress)
			if !errors.Is(rerr, errRangeMismatch) {
				return result, rerr
			}
			// 部分文件与服务端不一致：丢弃后全量重下（本次调用内完成）
			d.getLogger().Info("range resume mismatch, fallback to full download",
				"url", source, "partial_size", existingSize)
			_ = resp.Body.Close()
			_ = os.Remove(partialPath)
			_ = os.Remove(etagPath(partialPath))
			return d.downloadFull(ctx, source, destPath, onProgress)

		case http.StatusOK:
			// 服务端不支持 Range（或 If-Range 验证失败导致回退 200），
			// 删除部分文件和 ETag 缓存，走全量下载路径
			_ = os.Remove(partialPath)
			_ = os.Remove(etagPath(partialPath))

		case http.StatusRequestedRangeNotSatisfiable:
			// 416：若部分文件已等于服务端总大小，仅当能确认内容一致（缓存的
			// ETag 与 416 响应 ETag 匹配）时才收尾；否则回退全量重下，防止
			// "同尺寸但内容已变"的陈旧 partial 被静默收尾为错误文件（数据损坏）。
			if total := parseSuffixRange(resp.Header.Get("Content-Range")); total == existingSize {
				respETag := extractETag(resp)
				if cachedETag != "" && respETag == cachedETag {
					result, rerr := d.finalizePartial(partialPath, destPath, resp, existingSize)
					if rerr != nil {
						_ = os.Remove(partialPath)
						_ = os.Remove(etagPath(partialPath))
						return nil, rerr
					}
					return result, nil
				}
				d.getLogger().Info("416 finalize skipped: cannot verify partial content identity, fallback to full download",
					"url", source, "partial_size", existingSize, "cached_etag", cachedETag != "", "resp_etag", respETag)
			}
			// 部分文件比服务端当前文件大（远程文件被替换/变短）或无法确认内容一致：
			// 删除陈旧 partial 后全量重下，而不是返回 416 让任务永久失败
			_ = resp.Body.Close()
			_ = os.Remove(partialPath)
			_ = os.Remove(etagPath(partialPath))
			return d.downloadFull(ctx, source, destPath, onProgress)

		default:
			_, _ = io.Copy(io.Discard, resp.Body)
			return nil, d.statusError(resp.StatusCode)
		}
	}

	if resp.StatusCode != http.StatusOK {
		// 非 200 时排空响应体，确保连接可复用
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, d.statusError(resp.StatusCode)
	}

	return d.writeFullBody(ctx, resp, destPath, onProgress)
}

// downloadFull 发起不带 Range 的全量下载（续传信息不一致时回退使用）。
func (d *HTTPDownloader) downloadFull(ctx context.Context, source, destPath string, onProgress ProgressFunc) (*Result, error) {
	resp, err := d.doGet(ctx, source, 0, "")
	if err != nil {
		return nil, err
	}
	if err2 := d.validateAfterDo(resp); err2 != nil {
		return nil, err2
	}
	// 在 defer 前包 idleReadCloser，使正常路径的 Close 能停止空闲定时器
	resp.Body = newIdleReadCloser(resp.Body, d.IdleTimeout)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, d.statusError(resp.StatusCode)
	}
	return d.writeFullBody(ctx, resp, destPath, onProgress)
}

// doGet 发起 GET 请求；existingSize>0 时携带 Range 和 If-Range 头。
// etag 为空时仅发送 Range；etag 非空时同时发送 If-Range 以校验远程内容未变。
// 错误分类：建请求错误（非法 URL 等，确定性）返回普通错误，Do 的网络错误返回
// RetryableError——避免管理器对不可能成功的请求空耗重试。
func (d *HTTPDownloader) doGet(ctx context.Context, source string, existingSize int64, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "sproxy-cloud-downloader/1.0")
	if existingSize > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", existingSize))
		if etag != "" {
			req.Header.Set("If-Range", etag)
		}
	}
	resp, err := d.getClient().Do(req)
	if err != nil {
		// 区分永久性错误（DNS NXDOMAIN、TLS 握手失败等）与可重试错误（超时、连接拒绝等）。
		// 永久性错误重试永远不会成功，避免空耗 maxRetries 次重试。
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, retryablef("http get: %w", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, retryablef("http get: %w", err)
		}
		return nil, fmt.Errorf("http get: %w", err)
	}
	return resp, nil
}

// validateAfterDo 在请求完成后二次验证最终 URL 的 IP 是否安全（DNS 重绑定防御）。
func (d *HTTPDownloader) validateAfterDo(resp *http.Response) error {
	if fn := d.ValidateURLAfterDo; fn != nil {
		if err2 := fn(resp.Request.URL); err2 != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			return fmt.Errorf("ssrf post-request: %w", err2)
		}
	}
	return nil
}

// statusError 将非 2xx 状态转为错误；5xx 标记为可重试。
func (d *HTTPDownloader) statusError(code int) error {
	if code >= 500 {
		return retryablef("http status %d", code)
	}
	return fmt.Errorf("http status %d", code)
}

// writeFullBody 将 200 响应体写入 .partial 文件并在成功后原子重命名。
// 失败时保留 .partial，供下一次 Range 续传复用（避免中断后全量重下）。
func (d *HTTPDownloader) writeFullBody(ctx context.Context, resp *http.Response, destPath string, onProgress ProgressFunc) (*Result, error) {
	// 从 Last-Modified 响应头提取原始文件修改时间
	var modTime time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, parseErr := time.Parse(time.RFC1123, lm); parseErr == nil {
			modTime = t
		} else if t, parseErr := time.Parse(time.RFC1123Z, lm); parseErr == nil {
			modTime = t
		}
	}

	partialPath := destPath + ".partial"
	if dir := filepath.Dir(destPath); dir != "." {
		if err2 := os.MkdirAll(dir, 0755); err2 != nil {
			return nil, fmt.Errorf("create parent dir: %w", err2)
		}
	}

	// 全量下载：截断重建部分文件（成功后 rename 为最终文件）
	f, err := os.Create(partialPath)
	if err != nil {
		return nil, fmt.Errorf("create partial file: %w", err)
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
	const copyBufferSize = 32 * 1024
	buf := make([]byte, copyBufferSize)
	var downloaded int64
	for {
		n, readErr := tee.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
				// 写入失败（磁盘问题）：保留已写部分，错误不可重试
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
			// 读取失败（网络/超时）：保留 .partial 供续传，可重试
			return nil, retryablef("read body: %w", readErr)
		}
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close partial file: %w", err)
	}

	checksum := hex.EncodeToString(h.Sum(nil))
	etag := extractETag(resp)
	// 立即持久化 ETag 伴侣文件：使正常中断→续传流程中 loadETag 能读到非空 ETag，
	// 从而发送 If-Range 头校验远程内容一致性，避免盲追加导致混合文件。
	// 仅 Warn 失败（finalizeDownload 中还有一次兜底写入，此处持久化失败不影响
	// 当前下载完成，仅影响"续传时 If-Range 校验"的可用性）。
	if etag != "" {
		if err := saveETag(etagPath(partialPath), etag); err != nil {
			d.getLogger().Warn("保存 ETag 伴侣失败，续传将无 If-Range 一致性校验",
				"path", etagPath(partialPath), "error", err)
		}
	}
	if err := d.finalizeDownload(partialPath, destPath, modTime, etag); err != nil {
		return nil, err
	}
	return &Result{Size: downloaded, Checksum: checksum, ModTime: modTime, ETag: etag}, nil
}

// handleRangeResume 处理 Range 续传场景：追加写入部分文件并校验。
// 当服务端返回的 Content-Range 与本地部分文件不一致时返回 errRangeMismatch，
// 由调用方回退全量下载。
func (d *HTTPDownloader) handleRangeResume(ctx context.Context, resp *http.Response, partialPath, destPath string, existingSize int64, cachedETag string, onProgress ProgressFunc) (*Result, error) {
	cr := resp.Header.Get("Content-Range")
	if cr == "" {
		return nil, errRangeMismatch
	}
	// 解析 Content-Range 格式: "bytes <start>-<end>/<total>"
	var startByte, endByte, totalSize int64
	if n, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &startByte, &endByte, &totalSize); err != nil || n != 3 {
		return nil, errRangeMismatch
	}
	if startByte != existingSize {
		return nil, errRangeMismatch
	}
	// 交叉校验响应 ETag 与发送的 If-Range：合规服务端在 If-Range 匹配时返回相同
	// ETag 的 206；若返回不同 ETag，说明服务端忽略 If-Range 且内容已变，继续追加
	// 会产生混合文件，必须回退全量下载。
	if respETag := extractETag(resp); respETag != "" && cachedETag != "" && respETag != cachedETag {
		return nil, errRangeMismatch
	}

	var modTime time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, parseErr := time.Parse(time.RFC1123, lm); parseErr == nil {
			modTime = t
		} else if t, parseErr := time.Parse(time.RFC1123Z, lm); parseErr == nil {
			modTime = t
		}
	}

	// 流式计算已有部分文件的 SHA-256（不整体读入内存）
	// 本地文件 I/O 错误不可重试（与 write to partial file 的分类一致），重试只会空耗
	h := sha256.New()
	if pf, err := os.Open(partialPath); err == nil {
		if _, copyErr := io.Copy(h, pf); copyErr != nil {
			pf.Close()
			return nil, fmt.Errorf("hash existing partial: %w", copyErr)
		}
		pf.Close()
	} else {
		return nil, fmt.Errorf("open partial file: %w", err)
	}

	// 以追加模式打开部分文件
	f, err := os.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("open partial file for append: %w", err)
	}
	defer f.Close()

	tee := io.TeeReader(resp.Body, h)

	const copyBufferSize = 32 * 1024
	buf := make([]byte, copyBufferSize)
	var downloaded int64
	for {
		n, readErr := tee.Read(buf)
		if n > 0 {
			if _, err := f.Write(buf[:n]); err != nil {
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
			// 读取中断（网络/超时）：保留部分文件，供下一次续传
			return nil, retryablef("read body: %w", readErr)
		}
	}

	// 校验总大小：服务端声明的 total 与本地已写入不一致时回退全量
	downloadedTotal := existingSize + downloaded
	if totalSize > 0 && downloadedTotal != totalSize {
		return nil, errRangeMismatch
	}

	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close partial file: %w", err)
	}

	fullChecksum := hex.EncodeToString(h.Sum(nil))
	etag := extractETag(resp)
	// 立即持久化 ETag 伴侣文件（与 writeFullBody 一致）
	if etag != "" {
		if err := saveETag(etagPath(partialPath), etag); err != nil {
			d.getLogger().Warn("保存 ETag 伴侣失败，续传将无 If-Range 一致性校验",
				"path", etagPath(partialPath), "error", err)
		}
	}
	if err := d.finalizeDownload(partialPath, destPath, modTime, etag); err != nil {
		return nil, err
	}
	return &Result{Size: downloadedTotal, Checksum: fullChecksum, ModTime: modTime, ETag: etag}, nil
}

// finalizePartial 处理服务端返回 416 且 total==existingSize 的场景：
// 本地部分文件已完整，直接计算哈希并收尾为最终文件。
func (d *HTTPDownloader) finalizePartial(partialPath, destPath string, resp *http.Response, existingSize int64) (*Result, error) {
	var modTime time.Time
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, parseErr := time.Parse(time.RFC1123, lm); parseErr == nil {
			modTime = t
		} else if t, parseErr := time.Parse(time.RFC1123Z, lm); parseErr == nil {
			modTime = t
		}
	}

	h := sha256.New()
	pf, err := os.Open(partialPath)
	if err != nil {
		// 本地文件 I/O 错误不可重试（与 handleRangeResume 的注释一致），重试只会
		// 在每次 Download 里先 os.Remove(partialPath) 丢弃唯一可续传的资产。
		return nil, fmt.Errorf("open partial file: %w", err)
	}
	if _, copyErr := io.Copy(h, pf); copyErr != nil {
		pf.Close()
		return nil, fmt.Errorf("hash partial file: %w", copyErr)
	}
	pf.Close()

	etag := extractETag(resp)
	if err := d.finalizeDownload(partialPath, destPath, modTime, etag); err != nil {
		return nil, err
	}
	return &Result{Size: existingSize, Checksum: hex.EncodeToString(h.Sum(nil)), ModTime: modTime, ETag: etag}, nil
}

// parseSuffixRange 解析 416 响应中的 "bytes */<total>" 形式 Content-Range。
// 解析失败返回 -1。
func parseSuffixRange(cr string) int64 {
	if !strings.HasPrefix(cr, "bytes */") {
		return -1
	}
	var total int64
	if _, err := fmt.Sscanf(cr, "bytes */%d", &total); err != nil {
		return -1
	}
	return total
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

// idleReadCloser 在响应体读取超过 IdleTimeout 无数据时主动关闭底层连接，
// 使阻塞的 Read 返回错误，避免下载永久挂起。
// 定时器在首次 Read 时才启动（而非响应头到达时），因此下载前的本地工作
// （如续传时哈希已有 partial）不计入空闲预算，避免大文件续传被误判超时活锁。
type idleReadCloser struct {
	io.ReadCloser
	mu    sync.Mutex
	timer *time.Timer
	idle  time.Duration
}

func newIdleReadCloser(body io.ReadCloser, idle time.Duration) io.ReadCloser {
	if idle <= 0 {
		return body
	}
	return &idleReadCloser{ReadCloser: body, idle: idle}
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.timer == nil {
		r.timer = time.AfterFunc(r.idle, func() { _ = r.ReadCloser.Close() })
	}
	r.mu.Unlock()
	n, err := r.ReadCloser.Read(p)
	if n > 0 && err == nil {
		r.mu.Lock()
		if r.timer != nil {
			r.timer.Reset(r.idle)
		}
		r.mu.Unlock()
	}
	return n, err
}

func (r *idleReadCloser) Close() error {
	r.mu.Lock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.mu.Unlock()
	return r.ReadCloser.Close()
}
