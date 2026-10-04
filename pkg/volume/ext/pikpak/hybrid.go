// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// hybridShareRatioMax 是分享区最大比例（恒 ≤0.5——即使实测分享直链可下 55% 也不超上限，
// 防 PikPak 收紧限制/边界波动导致 416）。
const hybridShareRatioMax = 0.5

// defaultHybridChunkSize 是分片大小（默认 64MB，平衡并发与恢复粒度）。
const defaultHybridChunkSize = 64 << 20

// HybridMetrics 是混合下载指标（并入 CloudMetrics）。
type HybridMetrics struct {
	ShareResolveFailed   atomic.Int64 // 分享直链 resolve 失败
	ShareSegmentFailed   atomic.Int64 // 分享段 chunk 失败（转账号段）
	AccountSegmentFailed atomic.Int64 // 账号段 chunk 失败
	DowngradeTotal       atomic.Int64 // 分享段 → 账号段 降级次数
	ShareBytesSaved      atomic.Int64 // 分享区实际下载字节（免账号配额）
	BoundaryOvershoot    atomic.Int64 // 416 边界误判（分享区超限）
}

// HybridConfig 是 HybridDownloader 配置。
type HybridConfig struct {
	Resolver    *ShareResolver
	API         *API
	HTTPClient  *http.Client
	ChunkSize   int64   // 默认 64MB
	ShareRatio  float64 // 分享区比例（默认 0.5，恒 ≤0.5）
	Concurrency int     // 并行 chunk 数（默认 4）
	AutoDelete  bool    // 完成后永久删转存（释放 6GB 空间）
	Logger      *slog.Logger
	Metrics     *HybridMetrics
}

// HybridDownloader 是分享直链前段 + 账号流量后段的混合下载器。
// 分片并行：分享区 [0, shareEnd) 匿名直链下载 + 账号区 [shareEnd, total) 转存后 FETCH 直链下载。
// 每 chunk 独立 Range → os.WriteAt 写入预分配文件 → 全部成功 = 文件完整。
type HybridDownloader struct {
	resolver    *ShareResolver
	api         *API
	client      *http.Client
	chunkSize   int64
	shareRatio  float64
	concurrency int
	autoDelete  bool
	log         *slog.Logger
	metrics     *HybridMetrics

	// restoredID 记录本次转存文件 ID（供完成后永久删）。
	restoredMu sync.Mutex
	restoredID string
}

// NewHybridDownloader 创建混合下载器。
func NewHybridDownloader(cfg HybridConfig) (*HybridDownloader, error) {
	if cfg.Resolver == nil {
		return nil, fmt.Errorf("hybrid: resolver required")
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("hybrid: api required")
	}
	chunk := cfg.ChunkSize
	if chunk <= 0 {
		chunk = defaultHybridChunkSize
	}
	ratio := cfg.ShareRatio
	if ratio <= 0 || ratio > hybridShareRatioMax {
		ratio = hybridShareRatioMax
	}
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = 4
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &HybridDownloader{
		resolver: cfg.Resolver, api: cfg.API, client: client,
		chunkSize: chunk, shareRatio: ratio, concurrency: conc, autoDelete: cfg.AutoDelete,
		log: log, metrics: cfg.Metrics,
	}, nil
}

// Name 返回下载器名（注册表用）。
func (d *HybridDownloader) Name() string { return "pikpak-hybrid" }

// Supports 判断是否支持该 source（mypikpak/keepshare 分享 URL）。
func (d *HybridDownloader) Supports(source string) bool {
	_, err := parseShareID(source)
	return err == nil
}

// Download 混合下载：分享直链前段 + 账号流量后段（分片并行）。
func (d *HybridDownloader) Download(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc) (*Result, error) {
	return d.DownloadWithWriter(ctx, source, destPath, onProgress, nil)
}

// DownloadWithWriter 实现 downloader.WriterDownloader（sink 记账）。
func (d *HybridDownloader) DownloadWithWriter(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
	shareID, err := parseShareID(source)
	if err != nil {
		return nil, err
	}
	d.log.Info("hybrid download start", "share", shareID)
	// ① 匿名 resolve（拿分享直链 + 文件元信息）
	meta, err := d.resolver.Resolve(ctx, source)
	if err != nil {
		d.metricsInc(func(m *HybridMetrics) { m.ShareResolveFailed.Add(1) })
		return nil, fmt.Errorf("hybrid resolve %s: %w (fallback to full account download available)", shareID, err)
	}
	target := pickLargestShareFile(meta.Files)
	if target == nil || target.DirectLink == "" {
		d.metricsInc(func(m *HybridMetrics) { m.ShareResolveFailed.Add(1) })
		return nil, fmt.Errorf("hybrid: no share direct link for %s", shareID)
	}

	total := target.Size
	if total <= 0 {
		return nil, fmt.Errorf("hybrid: unknown total size for %s", shareID)
	}
	// ②-⑥ 分片规划/预分配/并行下载/校验/删除
	return d.runHybrid(ctx, shareID, target, total, destPath, onProgress, sinkFactory)
}

// runHybrid 执行混合下载主体（分享区 + 账号区分片并行）。
func (d *HybridDownloader) runHybrid(ctx context.Context, shareID string, target *ShareFile, total int64, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
	shareEnd := int64(float64(total) * d.shareRatio)
	if shareEnd > total {
		shareEnd = total
	}
	d.log.Info("hybrid plan", "total", total, "share_end", shareEnd, "chunk", d.chunkSize)
	if err := preallocate(destPath, total); err != nil {
		return nil, fmt.Errorf("hybrid preallocate: %w", err)
	}
	chunks := planChunks(0, total, shareEnd, d.chunkSize)
	d.log.Info("hybrid chunks", "count", len(chunks))
	if err := d.runChunks(ctx, chunks, shareEnd, shareID, target, destPath, onProgress); err != nil {
		return nil, err
	}
	d.log.Info("hybrid chunks done")
	checksum, err := sha256File(destPath)
	if err != nil {
		return nil, fmt.Errorf("hybrid hash: %w", err)
	}
	d.deleteRestoredPermanent(ctx)
	return &Result{Size: total, Checksum: checksum, ModTime: time.Now()}, nil
}

// runChunks 并行执行所有 chunk：**分享区与账号区分池并行**（互不阻塞）——
// 分享直链 CDN 对连续 Range 限速（实测每请求 ~4s），账号区单独 pool 可同时下，
// 避免账号区被分享区排队阻塞（总时长 ≈ max(分享区, 账号区) 而非两者之和）。
func (d *HybridDownloader) runChunks(ctx context.Context, chunks []chunk, shareEnd int64, shareID string, target *ShareFile, destPath string, onProgress downloader.ProgressFunc) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		prog     atomic.Int64
	)
	// 分池：分享区 pool 与账号区 pool 各 d.concurrency/2（最少 1）。
	poolSize := d.concurrency / 2
	if poolSize < 1 {
		poolSize = 1
	}
	shareSem := make(chan struct{}, poolSize)
	acctSem := make(chan struct{}, poolSize)
	for _, c := range chunks {
		wg.Add(1)
		sem := shareSem
		if c.offset >= shareEnd {
			sem = acctSem
		}
		sem <- struct{}{}
		go func(c chunk, sem chan struct{}) {
			defer wg.Done()
			defer func() { <-sem }()
			cerr := d.downloadOneChunk(ctx, c, shareEnd, shareID, target, destPath, &prog, onProgress)
			if cerr != nil {
				d.metricsInc(func(m *HybridMetrics) { m.AccountSegmentFailed.Add(1) })
				mu.Lock()
				if firstErr == nil {
					firstErr = cerr
				}
				mu.Unlock()
			}
		}(c, sem)
	}
	wg.Wait()
	return firstErr
}

// downloadOneChunk 单个 chunk：分享区（含降级）或账号区。
func (d *HybridDownloader) downloadOneChunk(ctx context.Context, c chunk, shareEnd int64, shareID string, target *ShareFile, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	if c.offset >= shareEnd {
		return d.downloadAccountChunk(ctx, c, shareID, target.ID, destPath, prog, onProgress)
	}
	cerr := d.downloadShareChunk(ctx, c, shareID, target.ID, target.DirectLink, destPath, prog, onProgress)
	if cerr != nil {
		d.metricsInc(func(m *HybridMetrics) { m.ShareSegmentFailed.Add(1); m.DowngradeTotal.Add(1) })
		d.log.Warn("hybrid share chunk failed, downgrade to account", "offset", c.offset, "err", cerr)
		return d.downloadAccountChunk(ctx, c, shareID, target.ID, destPath, prog, onProgress)
	}
	return nil
}

// metricsInc 安全更新指标（metrics 可为 nil）。
func (d *HybridDownloader) metricsInc(fn func(m *HybridMetrics)) {
	if d.metrics != nil {
		fn(d.metrics)
	}
}

// chunk 是一个下载分片。
type chunk struct {
	offset int64
	length int64
}

// planChunks 把 [0,total) 切成 chunk（每片独立 Range，WriteAt 写入）。
func planChunks(start, total, shareEnd, chunkSize int64) []chunk {
	var out []chunk
	for off := start; off < total; {
		end := off + chunkSize
		if end > total {
			end = total
		}
		out = append(out, chunk{offset: off, length: end - off})
		off = end
	}
	return out
}

// downloadShareChunk 分享段 chunk：直链下载，失败重取直链重试，连续两错 → 返回错误（转账号段）。
func (d *HybridDownloader) downloadShareChunk(ctx context.Context, c chunk, shareID, fileID, directLink, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	link := directLink
	for attempt := 1; attempt <= 2; attempt++ {
		err := d.downloadChunkRange(ctx, c, link, destPath, prog, onProgress)
		if err == nil {
			d.metricsInc(func(m *HybridMetrics) { m.ShareBytesSaved.Add(c.length) })
			return nil
		}
		d.log.Warn("hybrid share chunk attempt failed", "offset", c.offset, "attempt", attempt, "err", err)
		if attempt == 2 {
			return err // 连续两次失败 → 转账号段
		}
		// 重新 resolve 拿新直链（expire 过期/网络错误）
		meta, nerr := d.resolver.Resolve(ctx, shareURLOf(shareID))
		if nerr != nil || meta == nil {
			return err
		}
		nt := pickLargestShareFile(meta.Files)
		if nt == nil || nt.DirectLink == "" {
			return err
		}
		link = nt.DirectLink
	}
	return nil
}

// downloadAccountChunk 账号区 chunk：转存（一次）→ 定位（文件夹/文件）→ FETCH 直链 → Range 下载。
// 转存经 d.restoredMu 串行化（多账号 chunk 并行时只转存一次，避免重复占空间）；
// 定位处理 RestoreShare 返回「Pack From Shared 文件夹」的情况（列文件夹找目标文件）。
func (d *HybridDownloader) downloadAccountChunk(ctx context.Context, c chunk, shareID, fileID, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	link, err := d.restoreAndLink(ctx, shareID, fileID)
	if err != nil {
		return err
	}
	return d.downloadChunkRange(ctx, c, link, destPath, prog, onProgress)
}

// restoreAndLink 转存（一次）+ 定位转存文件 + 拿 FETCH 直链。
func (d *HybridDownloader) restoreAndLink(ctx context.Context, shareID, fileID string) (string, error) {
	d.restoredMu.Lock()
	defer d.restoredMu.Unlock()
	if d.restoredID != "" {
		// 已转存：直接用记录的文件 id 拿直链
		link, err := d.api.DownloadLink(ctx, d.restoredID)
		if err != nil {
			return "", fmt.Errorf("hybrid fetch link (cached): %w", err)
		}
		return link, nil
	}
	fid, err := d.api.RestoreShare(ctx, shareID, []string{fileID}, "")
	if err != nil {
		return "", fmt.Errorf("hybrid restore %s: %w", shareID, err)
	}
	// 定位：fid 可能是文件夹（Pack From Shared）或文件
	driveFile, err := d.locateRestored(ctx, fid)
	if err != nil {
		return "", err
	}
	d.restoredID = driveFile.ID
	link, err := d.api.DownloadLink(ctx, driveFile.ID)
	if err != nil {
		return "", fmt.Errorf("hybrid fetch link: %w", err)
	}
	return link, nil
}

// locateRestored 定位转存文件：fid 若是文件夹（Pack From Shared），列其内容找
// 目标 mp4（RestoreShare 对单文件可能包文件夹）；否则直接用 fid。
func (d *HybridDownloader) locateRestored(ctx context.Context, fid string) (*FileMeta, error) {
	df, err := d.api.FindByID(ctx, fid)
	if err != nil {
		return nil, fmt.Errorf("hybrid locate restored: %w", err)
	}
	if df.Kind == "drive#folder" {
		// 列文件夹内容找视频文件
		files, lerr := d.api.List(ctx, fid)
		if lerr != nil {
			return nil, fmt.Errorf("hybrid list restored folder: %w", lerr)
		}
		for i := range files {
			if files[i].Kind == "drive#file" && hasVideoExt(files[i].Name) {
				return &files[i], nil
			}
		}
		// 回退：第一个文件
		for i := range files {
			if files[i].Kind == "drive#file" {
				return &files[i], nil
			}
		}
		return nil, fmt.Errorf("hybrid: no file in restored folder %s", fid)
	}
	return df, nil
}

// hasVideoExt 判断视频扩展名。
func hasVideoExt(name string) bool {
	low := strings.ToLower(name)
	for _, ext := range []string{".mp4", ".mkv", ".ts", ".m4v", ".webm", ".avi", ".mov", ".flv", ".wmv", ".m2ts"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

// deleteRestoredPermanent 永久删除本次转存（AutoDelete 时；释放 6GB 空间）。
func (d *HybridDownloader) deleteRestoredPermanent(ctx context.Context) {
	if !d.autoDelete {
		return
	}
	d.restoredMu.Lock()
	id := d.restoredID
	d.restoredID = ""
	d.restoredMu.Unlock()
	if id != "" {
		if err := d.api.DeletePermanent(ctx, []string{id}); err != nil {
			d.log.Warn("hybrid delete restored failed", "id", id, "err", err)
		} else {
			d.log.Info("hybrid restored file deleted (permanent)", "id", id)
		}
	}
}

// downloadChunkRange 单个 chunk Range 下载 → WriteAt(offset)。
// 校验：206/200 + 写入字节数 == chunk 长度（数据正确性保障）。
func (d *HybridDownloader) downloadChunkRange(ctx context.Context, c chunk, link, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	resp, err := d.doChunkRequest(ctx, c, link)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return d.writeChunkBody(resp, c, destPath, prog, onProgress)
}

// doChunkRequest 发起 Range 请求并校验状态码。
func (d *HybridDownloader) doChunkRequest(ctx context.Context, c chunk, link string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://mypikpak.com/") // 分享直链 CDN 校验来源
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", c.offset, c.offset+c.length-1))
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("hybrid chunk %d: HTTP %d", c.offset, resp.StatusCode)
	}
	return resp, nil
}

// writeChunkBody 把响应体按偏移写入预分配文件（WriteAt），校验写入字节数。
func (d *HybridDownloader) writeChunkBody(resp *http.Response, c chunk, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	f, err := os.OpenFile(destPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], c.offset+written); werr != nil {
				return werr
			}
			written += int64(n)
			prog.Add(int64(n))
			if onProgress != nil {
				onProgress(prog.Load(), -1)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if written != c.length {
		return fmt.Errorf("hybrid chunk %d: wrote %d, want %d", c.offset, written, c.length)
	}
	d.log.Info("hybrid chunk done", "offset", c.offset, "bytes", written)
	return nil
}

// preallocate 预分配文件（os.Truncate → sparse，不占实际空间直到写入）。
func preallocate(path string, size int64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

// pickLargestShareFile 挑最大的视频文件（复用 pickLargestVideo 语义）。
func pickLargestShareFile(files []ShareFile) *ShareFile {
	var best *ShareFile
	for i := range files {
		if best == nil || files[i].Size > best.Size {
			best = &files[i]
		}
	}
	return best
}

// shareURLOf 构造分享 URL（重 resolve 用）。
func shareURLOf(shareID string) string {
	return "https://mypikpak.com/s/" + shareID
}

// newSHA256 创建 SHA-256 hash（结果 hex）。
func newSHA256() *sha256Hasher { return &sha256Hasher{h: sha256.New()} }

type sha256Hasher struct {
	h interface {
		Write(p []byte) (int, error)
		Sum(b []byte) []byte
	}
}

// Write 实现 io.Writer。
func (s *sha256Hasher) Write(p []byte) (int, error) { return s.h.Write(p) }

// Hex 返回 hex 编码摘要。
func (s *sha256Hasher) Hex() string { return hex.EncodeToString(s.h.Sum(nil)) }
