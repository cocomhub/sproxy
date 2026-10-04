// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"crypto/sha1" // #nosec G505 -- PikPak file hash 协议是 SHA-1（完整性校验，非安全用途）
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/cocomhub/sproxy/pkg/netutil"
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

// HybridCounters 返回 hybrid 计数指标（供 cloud Prometheus 导出）。
func (m *HybridMetrics) HybridCounters() map[string]int64 {
	if m == nil {
		return nil
	}
	return map[string]int64{
		"pikpak_hybrid_share_resolve_failed":   m.ShareResolveFailed.Load(),
		"pikpak_hybrid_share_segment_failed":   m.ShareSegmentFailed.Load(),
		"pikpak_hybrid_account_segment_failed": m.AccountSegmentFailed.Load(),
		"pikpak_hybrid_downgrade_total":        m.DowngradeTotal.Load(),
		"pikpak_hybrid_share_bytes_saved":      m.ShareBytesSaved.Load(),
		"pikpak_hybrid_boundary_overshoot":     m.BoundaryOvershoot.Load(),
	}
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
	// 注意：本实例是注册表单例（cloud manager 并发任务共享）——
	// **只保留只读配置**；单次下载私有状态（restoredIDs/currentTotal）放 downloadCtx，
	// 每次 Download 调用独立创建，防跨任务数据污染（🔴 Critical 修复）。
}

// downloadCtx 是单次下载的私有状态（每次 Download 调用独立，非实例共享）。
// 修复：并发任务共享单例实例时，restoredIDs/currentTotal 跨任务污染
// （任务 B 账号区拿走任务 A 的转存文件 → 静默混合损坏）。
type downloadCtx struct {
	currentTotal int64      // 本次下载总大小（进度回调）
	restoredIDs  []string   // 本次转存的全部文件 ID（完成后永久删）
	restoredMu   sync.Mutex // 保护 restoredIDs（并发 chunk 串行转存）
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
		// 分享直链 CDN 限速 ~1MB/s（预览级）：16MB chunk 需 ~16s，并发下更慢；
		// Timeout 15min 防慢请求被误杀，IdleTimeout 30s 防半途卡死。
		// R19 门禁：Transport 用 netutil.IsolatedTransport() 基座（默认调校：拨号/TLS/连接池）
		// + 覆写 IdleConnTimeout，不裸构造 &http.Transport。
		tr := netutil.IsolatedTransport()
		tr.IdleConnTimeout = 30 * time.Second
		client = &http.Client{
			Timeout:   15 * time.Minute,
			Transport: tr,
		}
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

// HybridCounters 转发到实例 metrics（供 Prometheus 导出；metrics 可为 nil）。
// 修复（🟠3）：writeHybridMetrics 断言 HybridCounters() 需在 **下载器实例**上实现
// （此前只在 HybridMetrics 上，断言恒失败 → 指标永不导出）。
func (d *HybridDownloader) HybridCounters() map[string]int64 {
	if d.metrics == nil {
		return nil
	}
	return d.metrics.HybridCounters()
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
// sinkFactory 非空时，下载完成后把字节重放进 sink 记账（配额语义，对齐内置 HTTP 下载器）。
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
	d.log.Info("hybrid target", "share", shareID, "id", target.ID, "link", truncate(target.DirectLink, 60))

	total := target.Size
	if total <= 0 {
		return nil, fmt.Errorf("hybrid: unknown total size for %s", shareID)
	}
	// ②-⑥ 分片规划/预分配/并行下载/校验/删除
	return d.runHybrid(ctx, &downloadCtx{currentTotal: total}, shareID, target, total, destPath, onProgress, sinkFactory)
}

// runHybrid 执行混合下载主体（分享区 + 账号区分片并行）。
func (d *HybridDownloader) runHybrid(ctx context.Context, dc *downloadCtx, shareID string, target *ShareFile, total int64, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
	// 416 边界探测再分界：shareEnd = min(探测边界, total×shareRatio)。
	// 分享直链实际可下 ~55%（实测 750MB/1.28GB），但恒 ≤0.5 上限（防 PikPak 收紧）；
	// 探测边界 < ratio 上限时用探测值（分享区可下更多免配额）。
	shareEnd := d.computeShareEnd(ctx, target, total)
	d.log.Info("hybrid plan", "total", total, "share_end", shareEnd, "chunk", d.chunkSize)
	if err := preallocate(destPath, total); err != nil {
		return nil, fmt.Errorf("hybrid preallocate: %w", err)
	}
	chunks := planChunks(0, total, shareEnd, d.chunkSize)
	d.log.Info("hybrid chunks", "count", len(chunks))
	// 崩溃恢复：读 manifest，**校验源身份一致**（C4：同一分享/文件才跳过已完成）。
	manifest := d.loadValidManifest(destPath, shareID, target)
	if err := d.runChunks(ctx, dc, chunks, shareEnd, shareID, target, destPath, manifest, onProgress); err != nil {
		// G4：失败路径也清理已转存副本（AutoDelete 语义——失败任务不留 6GB 空间占用）。
		d.deleteRestoredPermanent(ctx, dc)
		return nil, err
	}
	d.log.Info("hybrid chunks done")
	removeManifest(destPath)
	checksum, err := sha256File(destPath)
	if err != nil {
		return nil, fmt.Errorf("hybrid hash: %w", err)
	}
	// C2 最终完整性校验：计算文件 SHA-1 与 target.Hash（PikPak file hash 为 40-hex SHA-1）
	// 交叉比对——失败即报错（不返回自洽 checksum 冒充成功）。
	if target.Hash != "" {
		sha1Hex, herr := sha1FileHex(destPath)
		if herr != nil {
			return nil, fmt.Errorf("hybrid sha1 verify: %w", herr)
		}
		if !strings.EqualFold(sha1Hex, strings.ToLower(target.Hash)) {
			// G4：校验失败也清理转存副本（AutoDelete 语义）。
			d.deleteRestoredPermanent(ctx, dc)
			return nil, fmt.Errorf("hybrid integrity check failed: file sha1 %s != target %s", sha1Hex, target.Hash)
		}
		d.log.Info("hybrid integrity verified (sha1 match)", "sha1", sha1Hex)
	}
	// sink 记账：sinkFactory 非空时把已落盘文件重放进 sink（配额语义，对齐内置 HTTP 下载器）。
	if sinkFactory != nil {
		fi, statErr := os.Stat(destPath)
		if statErr != nil {
			return nil, fmt.Errorf("hybrid stat for sink: %w", statErr)
		}
		sink, sinkErr := sinkFactory(discardWriter{}, fi.Size(), false)
		if sinkErr != nil {
			return nil, sinkErr
		}
		if replayErr := replayFileIntoSink(destPath, sink); replayErr != nil {
			sink.Finish(false, 0)
			d.deleteRestoredPermanent(ctx, dc) // 🟡：sink 重放失败也清理转存（AutoDelete 语义）
			return nil, replayErr
		}
		sink.Finish(true, 0)
	}
	d.deleteRestoredPermanent(ctx, dc)
	// 注意：Checksum 是**本地落盘文件的 SHA-256**（自证文件未被篡改/续传拼错），
	// 不是源身份哈希（源身份校验用 SHA-1 与 target.Hash 比对，已在上方 verify 完成）。
	// 消费方若拿本字段比对源 hash 会误判——仅作本地完整性指纹。
	return &Result{Size: total, Checksum: checksum, ModTime: time.Now()}, nil
}

// runChunks 并行执行所有 chunk：**分享区与账号区分池并行**（互不阻塞）——
// 分享直链 CDN 对连续 Range 限速（实测每请求 ~4s），账号区单独 pool 可同时下，
// 避免账号区被分享区排队阻塞（总时长 ≈ max(分享区, 账号区) 而非两者之和）。
func (d *HybridDownloader) runChunks(ctx context.Context, dc *downloadCtx, chunks []chunk, shareEnd int64, shareID string, target *ShareFile, destPath string, manifest *hybridManifest, onProgress downloader.ProgressFunc) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		prog     atomic.Int64
	)
	chunks = filterChunks(chunks, manifest) // 崩溃恢复：跳过已完成
	// 分池：分享区 pool 与账号区 pool 各 d.concurrency/2（最少 1）。
	poolSize := max(d.concurrency/2, 1)
	shareSem := make(chan struct{}, poolSize)
	acctSem := make(chan struct{}, poolSize)
	// C5：**先 spawn 全部 goroutine**，各自在 goroutine 内 acquire 对应池——
	// 循环内同步 acquire（FIFO 阻塞）会把账号 chunk 排在分享 chunk 之后（顺序化）。
	// 现在 share 满时循环立即推进到账号 chunk，双池真正并行（总时长 ≈ max(分享, 账号)）。
	for _, c := range chunks {
		wg.Add(1)
		sem := shareSem
		if c.offset >= shareEnd {
			sem = acctSem
		}
		go func(c chunk, sem chan struct{}) {
			defer wg.Done()
			sem <- struct{}{} // goroutine 内 acquire（不阻塞 spawn 循环）
			defer func() { <-sem }()
			cerr := d.downloadOneChunk(ctx, dc, c, shareEnd, shareID, target, destPath, &prog, onProgress)
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
func (d *HybridDownloader) downloadOneChunk(ctx context.Context, dc *downloadCtx, c chunk, shareEnd int64, shareID string, target *ShareFile, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	var cerr error
	if c.offset >= shareEnd {
		cerr = d.downloadAccountChunk(ctx, dc, c, shareID, target, destPath, prog, onProgress)
	} else {
		cerr = d.downloadShareChunk(ctx, dc, c, shareID, target, destPath, prog, onProgress)
		if cerr != nil {
			d.metricsInc(func(m *HybridMetrics) { m.ShareSegmentFailed.Add(1); m.DowngradeTotal.Add(1) })
			d.log.Warn("hybrid share chunk failed, downgrade to account", "offset", c.offset, "err", cerr)
			cerr = d.downloadAccountChunk(ctx, dc, c, shareID, target, destPath, prog, onProgress)
		}
	}
	if cerr == nil {
		// 完成：写 manifest（崩溃恢复跳过；记录源身份）。
		d.markChunkDone(dc, destPath, c, manifestSource{
			ShareID: shareID, FileID: target.ID, Hash: target.Hash, Size: target.Size,
		})
	}
	return cerr
}

// filterChunks 过滤掉 manifest 已完成的 chunk（崩溃恢复跳过，分享区免配额不浪费）。
func filterChunks(chunks []chunk, manifest *hybridManifest) []chunk {
	if manifest == nil {
		return chunks
	}
	out := make([]chunk, 0, len(chunks))
	for _, c := range chunks {
		if !manifest.Has(c.offset) {
			out = append(out, c)
		}
	}
	return out
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
		end := min(off+chunkSize, total)
		out = append(out, chunk{offset: off, length: end - off})
		off = end
	}
	return out
}

// downloadShareChunk 分享段 chunk：直链下载，失败重取直链重试一次，连续两错 → 转账号段。
// 简化：分享区 chunk 已 < shareEnd（416 探测保证 206），不再为 416 重试；
// 仅网络/直链过期错误重试（重新 resolve 新直链）。
func (d *HybridDownloader) downloadShareChunk(ctx context.Context, dc *downloadCtx, c chunk, shareID string, target *ShareFile, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	link := target.DirectLink
	for attempt := 1; attempt <= 2; attempt++ {
		err := d.downloadChunkRange(ctx, dc, c, link, destPath, prog, onProgress)
		if err == nil {
			d.metricsInc(func(m *HybridMetrics) { m.ShareBytesSaved.Add(c.length) })
			return nil
		}
		d.log.Warn("hybrid share chunk attempt failed", "offset", c.offset, "attempt", attempt, "err", err)
		if attempt == 2 {
			return err // 连续两次失败 → 转账号段
		}
		// 重新 resolve 拿新直链（expire 过期/网络错误）
		link = d.reResolveLink(ctx, shareID, target)
		if link == "" {
			return err
		}
	}
	return nil
}

// downloadAccountChunk 账号区 chunk：转存（幂等一次）→ 定位 → FETCH 直链 → Range 下载。
// 转存经 d.restoredMu 串行化（多账号 chunk 并行时只转存一次）+ FindInDrive 幂等检查。
func (d *HybridDownloader) downloadAccountChunk(ctx context.Context, dc *downloadCtx, c chunk, shareID string, target *ShareFile, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	// 🟡 账号区 chunk 韧性：失败重试 1 次（FETCH 链接每次新取，瞬时网络抖动可恢复）——
	// 与分享区重试对称，避免单次抖动整段放弃（manager 外层整任务重试兜底不够 chunk 级）。
	for attempt := 1; ; attempt++ {
		link, err := d.restoreAndLink(ctx, dc, shareID, target)
		if err != nil {
			return err
		}
		err = d.downloadChunkRange(ctx, dc, c, link, destPath, prog, onProgress)
		if err == nil {
			return nil
		}
		d.log.Warn("hybrid acct chunk attempt failed", "share", shareID, "offset", c.offset, "attempt", attempt, "err", err)
		if attempt == 2 {
			return err
		}
	}
}

// restoreAndLink 转存（幂等）+ 定位转存文件 + 拿 FETCH 直链。
// 幂等：restore 前先按**名字+大小**查网盘（FindInDrive 精确匹配）——已转存过则
// 跳过 RestoreShare 直接复用（避免多次 restore 累积同名副本占满 6GB 空间）；
// 未命中才 restore（restore 返回文件夹时进文件夹找文件）。
func (d *HybridDownloader) restoreAndLink(ctx context.Context, dc *downloadCtx, shareID string, target *ShareFile) (string, error) {
	dc.restoredMu.Lock()
	defer dc.restoredMu.Unlock()
	if len(dc.restoredIDs) > 0 {
		// 已转存：直接用记录的文件 id 拿直链（最后一个）
		last := dc.restoredIDs[len(dc.restoredIDs)-1]
		link, err := d.api.DownloadLink(ctx, last)
		if err != nil {
			return "", fmt.Errorf("hybrid fetch link (cached): %w", err)
		}
		return link, nil
	}
	// 幂等：先查同名同大小已存在的转存文件（避免重复 restore 累积副本），
	// 命中且 hash 一致则复用（抽取 helper 控制复杂度）。
	if link, ok := d.idempotentRestored(ctx, dc, target); ok {
		return link, nil
	}
	fid, owned, err := d.api.RestoreShare(ctx, shareID, []string{target.ID}, "")
	if err != nil {
		return "", fmt.Errorf("hybrid restore %s: %w", shareID, err)
	}
	// 定位：fid 可能是文件夹（Pack From Shared）或文件
	driveFile, err := d.locateRestored(ctx, fid)
	if err != nil {
		return "", err
	}
	if owned {
		// file_restore_own（源文件已在个人网盘，NH-P1）：不记入 restoredIDs——
		// AutoDelete 永久删只清本次 restore 副本，**绝不得删除用户自己的源文件**。
		d.log.Info("hybrid restore owned (skip delete registration)", "id", driveFile.ID)
	} else {
		dc.restoredIDs = append(dc.restoredIDs, driveFile.ID)
	}
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
func (d *HybridDownloader) deleteRestoredPermanent(ctx context.Context, dc *downloadCtx) {
	if !d.autoDelete {
		return
	}
	dc.restoredMu.Lock()
	ids := dc.restoredIDs
	dc.restoredIDs = nil
	dc.restoredMu.Unlock()
	if len(ids) > 0 {
		if err := d.api.DeletePermanent(ctx, ids); err != nil {
			d.log.Warn("hybrid delete restored failed", "ids", ids, "err", err)
		} else {
			d.log.Info("hybrid restored files deleted (permanent)", "count", len(ids))
		}
	}
}

// downloadChunkRange 单个 chunk Range 下载 → WriteAt(offset)。
// 校验：206/200 + 写入字节数 == chunk 长度（数据正确性保障）。
func (d *HybridDownloader) downloadChunkRange(ctx context.Context, dc *downloadCtx, c chunk, link, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	resp, err := d.doChunkRequest(ctx, c, link)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return d.writeChunkBody(resp, c, dc.currentTotal, destPath, prog, onProgress)
}

// verifyContentRange 校验响应对齐请求 offset。
// 206：要求 Content-Range: bytes <start>-<end>/<total>，start 必须 == c.offset、长度一致；
//
//	缺失/解析失败/不对齐时**保守报错**（fail-closed：不信任无对齐信息的 Range 响应）。
//
// 200：服务器忽略 Range 返回全文件——仅当 chunk 覆盖整个文件（offset==0 且 length==total）
//
//	时内容才正确（writeChunkBody 的 written==length 校验兜底）；部分 chunk 收到 200
//	即明确拒绝（正文是全文件，长度校验也会拦，这里给更清晰的错误）。
func (d *HybridDownloader) verifyContentRange(resp *http.Response, c chunk, total int64) error {
	if resp.StatusCode == http.StatusOK {
		if c.offset == 0 && c.length == total {
			return nil
		}
		return fmt.Errorf("hybrid chunk %d: HTTP 200 (Range ignored) for partial chunk", c.offset)
	}
	cr := resp.Header.Get("Content-Range")
	if cr == "" {
		return fmt.Errorf("hybrid chunk %d: missing Content-Range", c.offset)
	}
	var start, end, crTotal int64
	if _, err := fmt.Sscanf(cr, "bytes %d-%d/%d", &start, &end, &crTotal); err != nil {
		return fmt.Errorf("hybrid chunk %d: bad Content-Range %q", c.offset, cr)
	}
	if start != c.offset {
		return fmt.Errorf("hybrid chunk %d: Content-Range start %d != requested offset %d", c.offset, start, c.offset)
	}
	if end < start || end-start+1 != c.length {
		return fmt.Errorf("hybrid chunk %d: Content-Range %q length mismatch (want %d)", c.offset, cr, c.length)
	}
	return nil
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
// **先校验 Content-Range 起始与请求 offset 对齐**（C1）——若服务器返回 206 但
// 内容起始偏移非 c.offset（CDN 异变/多节点不一致/链接指向不同文件），字节会写错
// 位置而 written==length 照常通过 → 静默落盘损坏。必须在写入前拦截。
func (d *HybridDownloader) writeChunkBody(resp *http.Response, c chunk, total int64, destPath string, prog *atomic.Int64, onProgress downloader.ProgressFunc) error {
	if err := d.verifyContentRange(resp, c, total); err != nil {
		return err
	}
	f, err := os.OpenFile(destPath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	written, werr := copyRangeToFile(resp.Body, f, c.offset, c.length, prog, total, onProgress)
	if werr != nil {
		return werr
	}
	if written != c.length {
		return fmt.Errorf("hybrid chunk %d: wrote %d, want %d", c.offset, written, c.length)
	}
	d.log.Info("hybrid chunk done", "offset", c.offset, "bytes", written)
	return nil
}

// copyRangeToFile 把 Range 响应体按偏移写入文件（WriteAt），返回写入字节数。
func copyRangeToFile(r io.Reader, f *os.File, offset int64, want int64, prog *atomic.Int64, total int64, onProgress downloader.ProgressFunc) (int64, error) {
	buf := make([]byte, 1<<20)
	var written int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], offset+written); werr != nil {
				return written, werr
			}
			written += int64(n)
			prog.Add(int64(n))
			if onProgress != nil {
				onProgress(prog.Load(), total) // I1：传 total（非 -1），UI 总进度可显示
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return written, rerr
		}
	}
	return written, nil
}

// discardWriter 是 sink 包装目标（文件已直接写盘，sink 只做记账，无需实际写）。
type discardWriter struct{}

// Write 丢弃内容（仅触发 sink 记账）。
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// preallocate 预分配文件（os.Truncate → sparse，不占实际空间直到写入）。
func preallocate(path string, size int64) error {
	// 🔴 Critical 修复：崩溃恢复彻底失效——os.Create 的 O_TRUNC 会**清空已有文件**，
	// 而它在 loadValidManifest 之前执行 → 恢复时已下 chunk 先被清零、又被 manifest 跳过
	// → 文件残缺（hash 存在报错 / hash 缺失静默零洞损坏）。
	// 改用 O_RDWR|O_CREATE（不清零），Truncate(total) 预分配；新下载路径也被全量 chunk
	// 覆盖，无副作用；崩溃恢复则保留已下字节供续传。
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Truncate(size)
}

// pickLargestShareFile 挑最大的视频文件（复用 pickLargestVideo 语义）。
func pickLargestShareFile(files []ShareFile) *ShareFile {
	// 修复（🟡5）：实际筛视频文件（Kind=drive#file + 视频扩展）——此前只按 size 选最大
	// （I3 空锁：Kind 已解析未使用，多文件分享可能选到图片/文件夹）。
	if best := largestVideoFile(files); best != nil {
		return best
	}
	// 兜底：无视频文件时取最大文件（保持兼容单文件分享）
	return largestAnyFile(files)
}

// largestVideoFile 挑最大的视频文件（drive#file + 视频扩展）。
func largestVideoFile(files []ShareFile) *ShareFile {
	var best *ShareFile
	for i := range files {
		if files[i].Kind != "" && files[i].Kind != "drive#file" {
			continue
		}
		if !hasVideoExt(files[i].Name) {
			continue
		}
		if best == nil || files[i].Size > best.Size {
			best = &files[i]
		}
	}
	return best
}

// largestAnyFile 挑最大的普通文件（兜底：无视频时）。
func largestAnyFile(files []ShareFile) *ShareFile {
	var best *ShareFile
	for i := range files {
		if files[i].Kind == "" || files[i].Kind == "drive#file" {
			if best == nil || files[i].Size > best.Size {
				best = &files[i]
			}
		}
	}
	return best
}

// shareURLOf 构造分享 URL（重 resolve 用）。
func shareURLOf(shareID string) string {
	return "https://mypikpak.com/s/" + shareID
}

// sha1FileHex 计算文件 SHA-1（PikPak file hash 为 40-hex SHA-1，用于最终完整性校验）。
func sha1FileHex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha1.New() // #nosec G401 -- PikPak hash 协议要求 SHA-1
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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

// probeBoundary 探测分享直链的 416 边界（可下上限）。
// 二分探测：在 [0, total) 中找「最大可下 offset」（206 响应的最大起始点）。
// 返回该边界（后续分享区 chunk 严格 < 该值，保证 206 不 416）。
// 全 Range 可用（boundary == total）时返回 total。
func (d *HybridDownloader) probeBoundary(ctx context.Context, link string, total int64) (int64, error) {
	if total <= 0 {
		return 0, fmt.Errorf("probe: invalid total %d", total)
	}
	// 探测粒度：1MB 步进（平衡精度与请求数）。
	const probeStep = 1 << 20
	// 二分：lo = 可下（含 0），hi = 不可下（或 total 全可下）。
	lo, hi := int64(0), total
	// 先测 hi-1：若 206（全可下）直接返回 total。
	if d.probeRangeOK(ctx, link, hi-1, total) {
		return total, nil
	}
	for lo+probeStep < hi {
		mid := lo + (hi-lo)/2
		mid -= mid % probeStep
		if mid <= lo {
			break
		}
		if d.probeRangeOK(ctx, link, mid, total) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// probeRangeOK 单点探测：Range 起始 offset 是否 206。
// G1：Range 终点**钳制在文件内**（total-1）——此前 `offset+probeSize-1` 越过 EOF，
// 严格服务器对越过 EOF 的 Range 返回 416 ⇒ 「全 Range 可下」被误判为不可下，
// shareEnd 偏小（仅省流量效率，数据安全不受影响）。探测只关心**起始点**是否 206。
func (d *HybridDownloader) probeRangeOK(ctx context.Context, link string, offset, total int64) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://mypikpak.com/")
	end := offset + probeSize - 1
	if end > total-1 {
		end = total - 1
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, end))
	resp, err := d.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, probeSize))
	return resp.StatusCode == http.StatusPartialContent
}

// probeSize 是边界探测请求的探测字节数（1KB，足够拿状态码）。
const probeSize = 1024

// hybridManifest 是分片下载完成清单（崩溃恢复用）。
type hybridManifest struct {
	Total    int64           `json:"total"`
	ShareEnd int64           `json:"share_end"`
	Chunks   map[int64]int64 `json:"chunks"` // offset → length
	Source   manifestSource  `json:"source"`
}

// manifestSource 记录源分享身份（C4：恢复时校验源一致，否则全量重下）。
type manifestSource struct {
	ShareID string `json:"share_id"`
	FileID  string `json:"file_id"`
	Hash    string `json:"hash"`
	Size    int64  `json:"size"`
}

// manifestPath 返回 manifest 文件路径（destPath + ".hybrid"）。
func manifestPath(destPath string) string { return destPath + ".hybrid" }

// loadManifest 读取 manifest（不存在返回 nil）。
func loadManifest(destPath string) *hybridManifest {
	data, err := os.ReadFile(manifestPath(destPath))
	if err != nil {
		return nil
	}
	var m hybridManifest
	if json.Unmarshal(data, &m) != nil || m.Chunks == nil {
		return nil
	}
	return &m
}

// SourceMatches 判断 manifest 源身份与当前目标一致（C4）。
func (m *hybridManifest) SourceMatches(shareID string, target *ShareFile) bool {
	if m == nil {
		return false
	}
	if m.Source.ShareID != shareID || m.Source.FileID != target.ID {
		return false
	}
	if m.Source.Size != target.Size {
		return false
	}
	if m.Source.Hash != "" && target.Hash != "" && m.Source.Hash != target.Hash {
		return false
	}
	return true
}

// Has 判断 chunk（offset）是否已完成。
func (m *hybridManifest) Has(offset int64) bool {
	if m == nil {
		return false
	}
	_, ok := m.Chunks[offset]
	return ok
}

// markChunkDone 原子记录 chunk 完成（追加到 manifest；首次记录源身份）。
func (d *HybridDownloader) markChunkDone(dc *downloadCtx, destPath string, c chunk, src manifestSource) {
	dc.restoredMu.Lock()
	defer dc.restoredMu.Unlock()
	m := loadManifest(destPath)
	// 🟠4 加固：磁盘 manifest 源与本次不一致（异常路径残留旧源）→ 重建空 manifest（不合并旧 chunk）。
	if m != nil && !m.SourceMatches(src.ShareID, &ShareFile{ID: src.FileID, Size: src.Size, Hash: src.Hash}) {
		m = &hybridManifest{Chunks: map[int64]int64{}}
	}
	if m == nil {
		m = &hybridManifest{Chunks: map[int64]int64{}}
	}
	m.Chunks[c.offset] = c.length
	m.Source = src
	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	_ = os.WriteFile(manifestPath(destPath), data, 0o644)
}

// removeManifest 完成时删除 manifest。
func removeManifest(destPath string) {
	_ = os.Remove(manifestPath(destPath))
}

// reResolveLink 重新 resolve 拿新直链，并核对新目标与首次 target 一致（C3）。
// 返回新直链；不一致（ID/hash/size 变化）返回空（调用方放弃重试）。
func (d *HybridDownloader) reResolveLink(ctx context.Context, shareID string, target *ShareFile) string {
	meta, err := d.resolver.Resolve(ctx, shareURLOf(shareID))
	if err != nil || meta == nil {
		return ""
	}
	nt := pickLargestShareFile(meta.Files)
	if nt == nil || nt.DirectLink == "" {
		return ""
	}
	// C3：重取后核对新直链指向同一文件（防分享被换 → 与已成功 chunk 拼接混合损坏）。
	if nt.ID != target.ID {
		d.log.Warn("hybrid re-resolve file changed, abort retry", "old_id", target.ID, "new_id", nt.ID)
		return ""
	}
	if nt.Size != target.Size {
		d.log.Warn("hybrid re-resolve size changed, abort retry", "old_size", target.Size, "new_size", nt.Size)
		return ""
	}
	if target.Hash != "" && nt.Hash != "" && nt.Hash != target.Hash {
		d.log.Warn("hybrid re-resolve hash changed, abort retry", "old_hash", target.Hash, "new_hash", nt.Hash)
		return ""
	}
	return nt.DirectLink
}

// computeShareEnd 计算分享区边界：min(416 探测边界, total×shareRatio)。
func (d *HybridDownloader) computeShareEnd(ctx context.Context, target *ShareFile, total int64) int64 {
	shareEnd := min(int64(float64(total)*d.shareRatio), total)
	if probed, perr := d.probeBoundary(ctx, target.DirectLink, total); perr == nil {
		if probed < shareEnd {
			shareEnd = probed
			d.metricsInc(func(m *HybridMetrics) { m.BoundaryOvershoot.Add(1) })
		}
		d.log.Info("hybrid boundary probed", "total", total, "probed", probed, "share_end", shareEnd)
	} else {
		d.log.Warn("hybrid boundary probe failed, use ratio default", "err", perr)
	}
	return shareEnd
}

// loadValidManifest 读 manifest 并校验源身份一致（C4）；不一致返回 nil（全量重下）。
func (d *HybridDownloader) loadValidManifest(destPath, shareID string, target *ShareFile) *hybridManifest {
	m := loadManifest(destPath)
	if m != nil && !m.SourceMatches(shareID, target) {
		d.log.Warn("hybrid manifest source mismatch, full re-download", "manifest_source", m.Source.ShareID)
		removeManifest(destPath) // 🟠4：清旧源 manifest，防 markChunkDone 合并污染
		return nil
	}
	return m
}

// driveFileID 返回 FileMeta 的 ID（nil 安全，日志用）。

// idempotentRestored 幂等检查：网盘已有同名同大小且 hash 一致的转存 → 复用并记录，返回 (link, true)。
// 无 hash 可比对（目标/网盘 hash 缺失）时保守返回 false（不信任 size 匹配，走 restore）。
func (d *HybridDownloader) idempotentRestored(ctx context.Context, dc *downloadCtx, target *ShareFile) (string, bool) {
	existing, err := d.api.FindInDrive(ctx, target.Name, target.Size)
	if err != nil || existing == nil {
		return "", false
	}
	if target.Hash == "" || existing.Hash == "" || existing.Hash != target.Hash {
		d.log.Warn("hybrid restore skip rejected: hash mismatch (or missing)",
			"id", existing.ID, "target_hash", target.Hash, "drive_hash", existing.Hash)
		return "", false
	}
	// NH-P1 加固（2026-10-05）：**只登记确认是 restore 副本的文件**供 AutoDelete 删除。
	// FindInDrive 匹配的是「同名+同大小+同 hash」——无法区分「上次 hybrid 遗留的 restore 副本」
	// 与「用户自己网盘里同内容的文件」（自分享最典型）。若一律登记，AutoDelete 会永久删除
	// 用户的自有文件（数据丢失，round-6 对抗评审）。判别：restore 副本位于「Pack From Shared」
	// 文件夹（实测），用户自有文件在普通位置——parent 不是 Pack From Shared 的只复用、不登记。
	if d.isRestoreCopy(ctx, existing) {
		dc.restoredIDs = append(dc.restoredIDs, existing.ID)
	} else {
		d.log.Info("hybrid restore reuse (non-restore-copy, skip delete registration)",
			"id", existing.ID, "name", existing.Name)
	}
	link, lerr := d.api.DownloadLink(ctx, existing.ID)
	if lerr != nil {
		return "", false
	}
	return link, true
}

// isRestoreCopy 判断网盘文件是否为 hybrid restore 副本：parent 是「Pack From Shared」文件夹。
// 查不到 parent 元数据时保守返回 false（只复用不删，绝不误删用户自有文件）。
func (d *HybridDownloader) isRestoreCopy(ctx context.Context, f *FileMeta) bool {
	if f == nil || f.ParentID == "" {
		return false
	}
	parent, err := d.api.FindByID(ctx, f.ParentID)
	if err != nil {
		d.log.Warn("hybrid restore copy check failed (conservative: not delete)", "parent", f.ParentID, "err", err)
		return false
	}
	return parent.Kind == "drive#folder" && strings.EqualFold(parent.Name, "Pack From Shared")
}
