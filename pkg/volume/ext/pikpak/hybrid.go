// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/integrity"
	"github.com/cocomhub/sproxy/pkg/media"
	"github.com/cocomhub/sproxy/pkg/netutil"
)

// driveKindFile / driveKindFolder 是网盘文件 kind 值（S1192：消除重复字面量）。
const (
	driveKindFile   = "drive#file"
	driveKindFolder = "drive#folder"
)

// hybridShareRatioMax 是分享区最大比例（恒 ≤0.5——即使实测分享直链可下 55% 也不超上限，
// 防 PikPak 收紧限制/边界波动导致 416）。
const hybridShareRatioMax = 0.5

// defaultHybridChunkSize 是分片大小（默认 32MiB：分片更细 → 中断恢复粒度更小，与 pikget 对齐）。
const defaultHybridChunkSize = 32 << 20 // 32MiB

// hybridWorkingSuffix 是下载期临时文件后缀（全部分片完成且权威校验通过后才 rename 到 destPath）。
const hybridWorkingSuffix = ".hybrid.downloading"

// hybridManifestSuffix 是进度文件（manifest）后缀。
const hybridManifestSuffix = ".hybrid"

// hybridStaleSuffix 是失效 manifest 的归档后缀（源身份不一致/工作文件缺失时归到此，保留证据）。
const hybridStaleSuffix = ".hybrid.stale"

// hybridChunkAttempts 是单分片总尝试次数（取链与下载共用；**取链失败同样退避重试**——
// 2026-10-11 实测：单次 FETCH 取链 TLS 握手超时曾让 1.3GB 任务整单失败）。
const hybridChunkAttempts = 3

// defaultChunkRetryBackoff 是分片重试退避基数（指数 + 抖动）。
const defaultChunkRetryBackoff = 500 * time.Millisecond

// maxWorkNameBytes 是单个文件名（含后缀）的字节上限：NTFS/ext4 限制 255 字节。
// 工作/清单文件要在此上限内，且**不丢失任何信息**——超长基名「从中间截断成目录」：
// 前半段作为子目录，后半段（含扩展名）作为文件名，两段拼接即完整原名。
//
// 实测背景：keepshare 长 URL + 中文 spx_target → cloudfilename 生成 ~250B 文件名，
// 再加 .hybrid.downloading（19B）超限（file name too long）。
const maxWorkNameBytes = 255

// workDirSplit 把超长基名拆成（目录段序列, 文件名段）：目录段**逐级递归拆分**（每段
// ≤ maxNameSegBytes），文件段持有最后一段（含扩展名，加后缀 ≤255）。全部段按序拼接
// == 原基名（**零信息丢失**，UTF-8 边界安全）。任意长度都可承载——目录天然不受单
// 文件名 255 限制，多段目录串行叠加。
//
// 返回 (dirSegs, fileSeg)：dirSegs 为空表示无需拆分（原名已够短）。
func workDirSplit(base string) ([]string, string) {
	if len(base)+len(hybridWorkingSuffix) <= maxWorkNameBytes {
		return nil, base // 短名：平铺不建子目录
	}
	// 文件段预算 = 255 - 工作后缀长（保证工作文件名 ≤255）。
	budget := max(maxWorkNameBytes-len(hybridWorkingSuffix), 1)
	// 尾部（含扩展名）整段保留为文件段：扩展名不应被拆进目录（失去落盘语义）。
	// 注意：扩展名本身超预算（异常长，如 URL 参数被当成 ext）→ **不剥 tail**（置空），
	// 否则 tail==base 会让 base 被切光后又重复拼回（拼接长度翻倍）。
	tail := ""
	if ext := path.Ext(base); len(ext) > 0 && len(ext) < budget {
		tail = base[len(base)-len(ext):]
		base = base[:len(base)-len(ext)]
	}
	var dirs []string
	for len(base) > 0 && len(base)+len(tail)+len(hybridWorkingSuffix) > maxWorkNameBytes {
		// 每段取可承载量的一半（远离 255；UTF-8 安全切）。
		seg := min(len(base), maxNameSegBytes)
		seg = max(utf8SafeCut(base, seg),
			// 极端单字节场景兜底
			1)
		dirs = append(dirs, base[:seg])
		base = base[seg:]
	}
	fileSeg := base + tail
	if fileSeg == "" {
		fileSeg = "download" // 兜底（理论不可达）
	}
	return dirs, fileSeg
}

// maxNameSegBytes 是目录段单段字节上限（≤255；取 240 留余量，保证段与段之间即使
// 系统加分隔符也不撞限）。
const maxNameSegBytes = 240

// utf8SafeCut 返回不超过 n 且不劈开 UTF-8 字符的切点。
func utf8SafeCut(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// WorkPaths 返回（工作文件, 清单文件, 目录）路径。
// 超长基名「从中间截断成目录」：目录段作为**逐级子目录**（保留原名各段），文件段作为
// 实际文件名（保留原名尾部 + 扩展名）——全段拼接即完整原名，零信息丢失、不超 255。
func WorkPaths(destPath string) (working, manifest, dir string) {
	dir = filepath.Dir(destPath)
	dirs, name := workDirSplit(filepath.Base(destPath))
	for _, d := range dirs {
		dir = filepath.Join(dir, d)
	}
	return filepath.Join(dir, name+hybridWorkingSuffix),
		filepath.Join(dir, name+hybridManifestSuffix), dir
}

// WorkingPath 返回下载期临时文件路径（分片 WriteAt 目标；全部完成并校验通过后 rename 到 destPath）。
func WorkingPath(destPath string) string {
	w, _, _ := WorkPaths(destPath)
	return w
}

// StaleManifestPath 返回失效 manifest 的归档路径（保留进度证据，rebuild 修复命令可参考）。
func StaleManifestPath(destPath string) string {
	w, _, _ := WorkPaths(destPath)
	return w + hybridStaleSuffix
}

// ManifestPath 返回进度文件（manifest）路径——与 CLI/rebuild 共用同一命名，不重复硬编码。
func ManifestPath(destPath string) string {
	_, m, _ := WorkPaths(destPath)
	return m
}

// ChunkError 是分片级失败（带 offset/length）——CLI 据此提示「缺哪一片/可续传」，
// 不再只给一句网络错误。
type ChunkError struct {
	Offset int64 // 分片起始偏移
	Length int64 // 分片长度
	Err    error // 底层错误（errors.Is/As 可追溯 ctx/网络哨兵）
}

// Error 实现 error（含偏移与底层原因）。
func (e *ChunkError) Error() string {
	return fmt.Sprintf("hybrid chunk @%d+%d: %v", e.Offset, e.Length, e.Err)
}

// Unwrap 暴露底层错误（ctx 取消/网络错误/业务哨兵可被 errors.Is/As 识别）。
func (e *ChunkError) Unwrap() error { return e.Err }

// HybridMetrics 是混合下载指标（并入 CloudMetrics）。
type HybridMetrics struct {
	ShareResolveFailed   atomic.Int64 // 分享直链 resolve 失败
	ShareSegmentFailed   atomic.Int64 // 分享段 chunk 失败（转账号段）
	AccountSegmentFailed atomic.Int64 // 账号段 chunk 失败
	DowngradeTotal       atomic.Int64 // 分享段 → 账号段 降级次数
	FallbackTotal        atomic.Int64 // **整任务**委托降级下载器次数（round-8：与 chunk 级
	// 降级区分，运维可观测「匿名路径整体失效」）
	ShareBytesSaved   atomic.Int64 // 分享区实际下载字节（免账号配额）
	AccountBytesUsed  atomic.Int64 // 账号区实际下载字节（多账号分片总量，运维可观测配额分摊）
	BoundaryOvershoot atomic.Int64 // 416 边界误判（分享区超限）
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
		"pikpak_hybrid_fallback_total":         m.FallbackTotal.Load(),
		"pikpak_hybrid_share_bytes_saved":      m.ShareBytesSaved.Load(),
		"pikpak_hybrid_account_bytes_used":     m.AccountBytesUsed.Load(),
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
	// ChunkRetryBackoff 是分片取链/下载失败的重试退避基数（指数 + 抖动）；<=0 → 默认 500ms。
	ChunkRetryBackoff time.Duration
	// ChunkProgress 是 per-chunk 进度回调（nil = 不回调）。pikget 用它逐行显示每个分片
	// （编号/来源链/状态/进度/速率）。下载开始前会为每个 chunk 发一次 pending 事件。
	ChunkProgress ChunkProgressFunc
	// GCIDStats 是 GCID 校验命中统计（决策用：官方分块粒度规律）。nil=不统计。
	GCIDStats *integrity.GCIDVerifyStats
	// AccountPool 是多账号会话文件池（配额轮换 + 转存副本按账号分摊）。
	// nil = 单账号现状（账号区全部 chunk 走当前登录态/单 API）。非 nil 时账号区
	// chunk 按 Select round-robin 分配到多账号，每账号转存一次副本后并行 FETCH 下载。
	AccountPool *AccountPool
	// Fallback 是**匿名分享路径整体失败**时的降级下载器（如旧 PikpakDownloader 完整账号下载）。
	// 设计 §1.2：分享直链任何失败不阻断任务——resolve 失败/无直链/未知大小时委托降级
	// （chunk 级降级已内建：分享段失败转账号段）。nil = 无降级（失败如实上报）。
	Fallback downloader.Downloader
}

// HybridDownloader 是分享直链前段 + 账号流量后段的混合下载器。
// 分片并行：分享区 [0, shareEnd) 匿名直链下载 + 账号区 [shareEnd, total) 转存后 FETCH 直链下载。
// 每 chunk 独立 Range → os.WriteAt 写入预分配文件 → 全部成功 = 文件完整。
type HybridDownloader struct {
	resolver      *ShareResolver
	api           *API
	client        *http.Client
	chunkSize     int64
	shareRatio    float64
	concurrency   int
	autoDelete    bool
	log           *slog.Logger
	metrics       *HybridMetrics
	chunkProgress ChunkProgressFunc          // per-chunk 进度回调（pikget 逐行显示；nil=不回调）
	pool          *AccountPool               // 多账号会话池（nil = 单账号现状）
	acctSems      map[string]chan struct{}   // 每账号 1 个 sem（同账号串行，不同账号并发）
	acctSemMu     sync.Mutex                 // 保护 acctSems
	gcidStats     *integrity.GCIDVerifyStats // GCID 校验命中统计（nil=不统计）
	fallback      downloader.Downloader      // 匿名路径整体失败的降级下载器（设计 §1.2；nil=无降级）
	chunkBackoff  time.Duration              // 分片重试退避基数（0 = 默认；见 chunkRetryDelay）
	normalizer    *ShareSourceNormalizer     // keepshare 镜像归一（nil = 原样用）
	// 注意：本实例是注册表单例（cloud manager 并发任务共享）——
	// **只保留只读配置**；单次下载私有状态（lease/currentTotal/reusedID）放 downloadCtx，
	// 每次 Download 调用独立创建，防跨任务数据污染（🔴 Critical 修复）。
}

// downloadCtx 是单次下载的私有状态（每次 Download 调用独立，非实例共享）。
// 修复：并发任务共享单例实例时，restore 释放/currentTotal 跨任务污染
// （任务 B 账号区拿走任务 A 的转存文件 → 静默混合损坏）。
type downloadCtx struct {
	currentTotal int64         // 本次下载总大小（进度回调）
	lease        *RestoreLease // 转存副本释放（单一机制，round-10：收归 deleteRestoredPermanent）
	reusedID     string        // 缓存 idempotent/owned **未登记**命中的文件 ID（round-7 性能：
	// 避免每账号 chunk 重跑全盘 walk；round-8 改为 ID 而非直链——每次 DownloadLink 重取
	// 新链接防 TTL 过期。不登记即不删，NH-P1 不变）
	reusedMu   sync.Mutex // 保护 reusedID（round-11 Critical：并发账号 chunk 无锁读写竞态）
	manifestMu sync.Mutex // manifest 写串行化（markChunkDone）
	// 单次下载不变上下文（Sonar S107 收敛，2026-10-05）：shareID/target/destPath/prog/
	// onProgress 本是下载期常量，从函数签名收进 dc——函数签名降回 ≤7 参数。
	shareID    string
	sourceURL  string // 原始分享 URL（供 manifest 记录，CLI 恢复用）
	shareToken string // /s/<id>/<token> 的子路径 token（round-11：RefreshLink 重取需带 token，
	// 否则 token 分享中途重试 shareDetail 缺 token → 降级）
	target *ShareFile
	// destPath 是**最终目标路径**（用户请求的落盘位置）。
	// 下载期真实写盘目标是 destPath + ".hybrid.downloading"（临时文件）——分片 WriteAt 全部落在
	// 临时文件；全部 chunk 成功并完整性校验通过后才 rename 到 destPath。
	// 好处：文件未完成时直观可见（.hybrid.downloading 后缀），中断/失败不会残留看似完整的文件；
	// 成功 rename 原子完成（同目录 rename，跨文件系统时回退 copy+remove）。
	// 崩溃恢复：manifest 与预分配都作用在 .hybrid.downloading 临时文件上（续传天然对齐）。
	destPath      string
	working       string // 下载期写盘目标 = destPath + ".hybrid.downloading"（见 openWorking）
	prog          atomic.Int64
	onProgress    downloader.ProgressFunc
	chunkProgress ChunkProgressFunc // per-chunk 回调（pikget 逐行显示每个分片）
	chunkOffsets  []int64           // 所有 chunk offset（升序，编号映射）
	shareEnd      int64             // 分享区边界（chunk 来源判定）
	// 下载审计统计（用户裁定 2026-10-28）：起止时刻 + 分通道字节（并发分片原子累加）。
	// 统计口径：chunk **成功**时的完整长度（重试/续传跳过的块不计入本次会话字节）。
	startedAt time.Time
	anonBytes atomic.Int64
	acctBytes atomic.Int64
	// 多账号分片（round-12）：accounted 记录已转存副本的账号（name→转存文件 ID），
	// 每账号首次选中时 Use 转存一次，后续该账号的 chunk 复用直链（避免 N×重复转存占空间）。
	accounted map[string]string
	acctMu    sync.Mutex // 保护 accounted
}

// NewHybridDownloader 创建混合下载器。
// Resolver 可为 nil（**drive-only 模式**：DownloadDriveFile 下载账号网盘已有文件，
// 不需要分享解析）；分享下载（Download）必须提供 Resolver——由调用方保证。
func NewHybridDownloader(cfg HybridConfig) (*HybridDownloader, error) {
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
		log: log, metrics: cfg.Metrics, pool: cfg.AccountPool, fallback: cfg.Fallback,
		chunkProgress: cfg.ChunkProgress,
		gcidStats:     cfg.GCIDStats,
		chunkBackoff:  cfg.ChunkRetryBackoff,
		normalizer:    NewShareSourceNormalizer(client),
		acctSems:      make(map[string]chan struct{}),
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

// GCIDStats 返回 GCID 校验统计（metrics 导出用；nil=未装配）。
func (d *HybridDownloader) GCIDStats() *integrity.GCIDVerifyStats { return d.gcidStats }

// IntegrityMode 声明完整性归属：hybrid 走 GCID 权威复算（① 态），
// 未命中时回落本地自洽（② 态）——与旧 PikpakDownloader 语义一致。
func (d *HybridDownloader) IntegrityMode() downloader.IntegrityMode { return downloader.ModeLocalOnly }

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
// 匿名分享路径**整体失败**（resolve 失败/无直链/未知大小）时委托 Fallback 降级下载器
// （设计 §1.2：分享直链任何失败不阻断任务）；无 Fallback 则如实上报错误。
func (d *HybridDownloader) DownloadWithWriter(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory) (*Result, error) {
	// 归一 keepshare 镜像 → 真实分享 URL（否则匿名 resolve 用容器 id 恒 NOT_FOUND）
	source = d.normalizeSource(ctx, source)
	shareID, err := parseShareID(source)
	if err != nil {
		return nil, err
	}
	d.log.Info("hybrid download start", "share", shareID)
	// ① 匿名 resolve（拿分享直链 + 文件元信息）
	meta, err := d.resolver.Resolve(ctx, source)
	if err != nil {
		d.metricsInc(func(m *HybridMetrics) { m.ShareResolveFailed.Add(1) })
		return d.fallbackDownload(ctx, source, destPath, onProgress, sinkFactory,
			fmt.Errorf("hybrid resolve %s: %w", shareID, err))
	}
	// 目标文件：无 spx_target ⇒ 最大视频（既有）；有 ⇒ 按名选中（分卷全套）
	target := pickShareFileByTarget(meta.Files, parseShareTarget(source))
	if target == nil || target.DirectLink == "" {
		if parseShareTarget(source) != "" {
			d.metricsInc(func(m *HybridMetrics) { m.ShareResolveFailed.Add(1) })
			return d.fallbackDownload(ctx, source, destPath, onProgress, sinkFactory,
				fmt.Errorf("hybrid: share %s has no direct link for target %q", shareID, parseShareTarget(source)))
		}
		d.metricsInc(func(m *HybridMetrics) { m.ShareResolveFailed.Add(1) })
		return d.fallbackDownload(ctx, source, destPath, onProgress, sinkFactory,
			fmt.Errorf("hybrid: no share direct link for %s", shareID))
	}
	// 日志卫生：只记目标文件 ID，不记直链 URL（签名 URL 进日志是泄露面，round-7）。
	d.log.Info("hybrid target", "share", shareID, "id", target.ID)

	total := target.Size
	if total <= 0 {
		return d.fallbackDownload(ctx, source, destPath, onProgress, sinkFactory,
			fmt.Errorf("hybrid: unknown total size for %s", shareID))
	}
	// ②-⑥ 分片规划/预分配/并行下载/校验/删除
	dc := &downloadCtx{
		currentTotal: total, lease: NewRestoreLease(d.api, d.pool, d.autoDelete, d.log),
		shareID: shareID, sourceURL: source, target: target, destPath: destPath, onProgress: onProgress,
		chunkProgress: d.chunkProgress,
		accounted:     make(map[string]string),
		startedAt:     time.Now(),
	}
	// 续传进度基准：读 .hybrid manifest 累计已完成 chunk 字节，预置 prog——
	// 恢复时已完成 chunk 被 filterChunks 跳过，若 prog 从 0 起进度回调会从头计数
	// （实际续传是生效的，只是显示从 0 开始）。预置后进度从已完成处继续。
	if _, tok := parseShareIDWithToken(source); tok != "" {
		dc.shareToken = tok // round-11：token 分享的 re-resolve 需带 token
	}
	// 多账号运行时对账（round-14）：CLI 运行期 `sproxy pikpak account add/remove` 写入 secrets
	// 卷后经 RefreshAccounts 生效——与旧下载器 download() 一致（否则 hybrid 池固定为启动快照，
	// F5/接线不对称：CLI 添加账号后旧下载器可用、hybrid 不识别）。List 失败用现有列表继续。
	if d.pool != nil {
		if rerr := d.pool.RefreshAccounts(ctx); rerr != nil {
			d.log.Warn("hybrid refresh accounts", "err", rerr)
		}
	}
	return d.runHybrid(ctx, dc, sinkFactory)
}

// DriveFileRequest 是「下载账号网盘里已有文件」的请求（复用 hybrid 账号区分片内核）。
//
// 与分享下载的差别只有一处：文件已在网盘，**全部字节走账号区**（shareEnd = 0）——
// 分片规划/manifest 续传/GCID 权威校验/临时文件原子 rename 完全复用 runHybrid，
// 不另写下载实现（用户裁定 2026-10-28：pikget 生产路径已验证，禁止重复实现）。
type DriveFileRequest struct {
	FileID      string // 账号网盘文件 ID（每 chunk 经 api.DownloadLink 取新 FETCH 直链）
	FileName    string
	Hash        string // 官方 hash（GCID）；非空时做权威校验
	Size        int64
	DestPath    string
	OnProgress  downloader.ProgressFunc
	SinkFactory downloader.SinkFactory
	// 源文件时间（网盘 API 带外；零值 = 未知）。转存产物 mtime 用 ModifiedTime。
	CreatedTime  time.Time
	ModifiedTime time.Time
}

// DownloadDriveFile 用账号区 Range 分片下载账号网盘文件（manifest 断点续传）。
func (d *HybridDownloader) DownloadDriveFile(ctx context.Context, req DriveFileRequest) (*Result, error) {
	if req.FileID == "" || req.Size <= 0 || req.DestPath == "" {
		return nil, fmt.Errorf("hybrid drive: invalid request (id=%q size=%d dest=%q)", req.FileID, req.Size, req.DestPath)
	}
	dc := &downloadCtx{
		currentTotal: req.Size,
		lease:        NewRestoreLease(d.api, d.pool, d.autoDelete, d.log),
		shareID:      "", // drive 模式无分享身份（manifest 源身份用 FileID）
		sourceURL:    "",
		target: &ShareFile{ID: req.FileID, Name: req.FileName, Hash: req.Hash, Size: req.Size,
			CreatedTime: req.CreatedTime, ModifiedTime: req.ModifiedTime},
		destPath:      req.DestPath,
		onProgress:    req.OnProgress,
		chunkProgress: d.chunkProgress,
		accounted:     make(map[string]string),
		startedAt:     time.Now(),
	}
	// drive 模式：文件 id 已在网盘 → 直接复用（不转存、不删副本）。
	dc.reusedID = req.FileID
	return d.runHybridWithShareEnd(ctx, dc, req.SinkFactory, 0)
}

// normalizeSource 归一分享源（keepshare 镜像 → 真实 mypikpak URL）；失败时原样返回
// （调用方后续 resolve 报错可见；下载器不能阻断非分享 URL 的通用路径）。
func (d *HybridDownloader) normalizeSource(ctx context.Context, source string) string {
	if d.normalizer == nil {
		return source
	}
	out, err := d.normalizer.NormalizeShareSource(ctx, source)
	if err != nil {
		d.log.Warn("hybrid keepshare 归一失败（按原样继续，resolve 会报 NOT_FOUND）", "err", err)
		return source
	}
	if out != source {
		d.log.Info("hybrid share source normalized", "from", source[:min(len(source), 90)], "to", out[:min(len(out), 90)])
	}
	return out
}

// fallbackDownload 匿名路径整体失败时委托降级下载器（设计 §1.2）；无降级则返回原错误。
func (d *HybridDownloader) fallbackDownload(ctx context.Context, source, destPath string, onProgress downloader.ProgressFunc, sinkFactory downloader.SinkFactory, err error) (*Result, error) {
	if d.fallback == nil {
		return nil, err
	}
	d.metricsInc(func(m *HybridMetrics) { m.FallbackTotal.Add(1) }) // round-8：整任务降级可观测
	d.log.Warn("hybrid anonymous path failed, delegating to fallback downloader", "err", err)
	if wd, ok := d.fallback.(downloader.WriterDownloader); ok {
		return wd.DownloadWithWriter(ctx, source, destPath, onProgress, sinkFactory)
	}
	return d.fallback.Download(ctx, source, destPath, onProgress)
}

// runHybrid 执行混合下载主体（分享区 + 账号区分片并行）。

// hybridSinkReplay 把已落盘文件重放进 sink（配额记账；失败清理转存副本）。
func (d *HybridDownloader) hybridSinkReplay(ctx context.Context, dc *downloadCtx, sinkFactory downloader.SinkFactory) error {
	fi, statErr := os.Stat(dc.destPath)
	if statErr != nil {
		return fmt.Errorf("hybrid stat for sink: %w", statErr)
	}
	sink, sinkErr := sinkFactory(discardWriter{}, fi.Size(), false)
	if sinkErr != nil {
		return sinkErr
	}
	if replayErr := replayFileIntoSink(dc.destPath, sink); replayErr != nil {
		sink.Finish(false, 0)
		dc.lease.Release(ctx) // 🟡：sink 重放失败也清理转存（AutoDelete 语义）
		return replayErr
	}
	sink.Finish(true, 0)
	return nil
}

// prepareManifest 加载/创建 manifest 并预置续传进度基准（已完成 chunk 字节入 prog）。
func (d *HybridDownloader) prepareManifest(dc *downloadCtx, shareEnd int64) *hybridManifest {
	manifest := d.loadValidManifest(dc.destPath, dc.shareID, dc.target)
	// 两文件配套出现：开始下载立刻创建空 manifest——中断/退出也 .hybrid 与
	// .hybrid.downloading 成对存在（直观可续）。已有同源 manifest 保留（崩溃续传）。
	if manifest == nil {
		manifest = d.initManifest(dc, shareEnd)
	}
	// 续传进度基准：已完成 chunk 字节预置进 prog（进度从已完成处继续，不从头计数）。
	var done int64
	for _, ln := range manifest.Chunks {
		done += ln
	}
	dc.prog.Store(done)
	return manifest
}

// verifyHybridGCID 对临时文件做 GCID 权威复算（候选分块命中官方 hash → true）。
// 未命中 → 清理转存副本（G4：失败不留 6GB 空间）并返回错误（不冒充成功）。
func (d *HybridDownloader) verifyHybridGCID(ctx context.Context, dc *downloadCtx) (bool, error) {
	if dc.target.Hash == "" {
		return false, nil // 无权威 hash：不校验（回落本地自洽）
	}
	matched, gerr := integrity.VerifyGCIDStats(dc.working, integrity.GCIDCandidates, dc.target.Hash, d.gcidStats)
	if gerr != nil {
		return false, fmt.Errorf("hybrid gcid verify: %w", gerr)
	}
	if !matched {
		dc.lease.Release(ctx) // G4：校验失败也清理转存副本（AutoDelete 语义）
		return false, fmt.Errorf("hybrid integrity check failed: %w (file gcid != target %s)", ErrIntegrityMismatch, dc.target.Hash)
	}
	d.log.Info("hybrid integrity verified (gcid match)")
	return true, nil
}

func (d *HybridDownloader) runHybrid(ctx context.Context, dc *downloadCtx, sinkFactory downloader.SinkFactory) (*Result, error) {
	// 416 边界探测再分界：shareEnd = min(探测边界, total×shareRatio)。
	// 分享直链实际可下 ~55%（实测 750MB/1.28GB），但恒 ≤0.5 上限（防 PikPak 收紧）；
	// 探测边界 < ratio 上限时用探测值（分享区可下更多免配额）。
	shareEnd := d.computeShareEnd(ctx, dc.target, dc.currentTotal)
	return d.runHybridWithShareEnd(ctx, dc, sinkFactory, shareEnd)
}

// runHybridWithShareEnd 是 runHybrid 的主体（shareEnd 由调用方给定）：
// 分享下载用 computeShareEnd 结果；账号网盘文件下载（DownloadDriveFile）传 0（全账号区）。
func (d *HybridDownloader) runHybridWithShareEnd(ctx context.Context, dc *downloadCtx, sinkFactory downloader.SinkFactory, shareEnd int64) (*Result, error) {
	integrityVerified := false // GCID 权威复算命中标记（Result 三态用）
	// 写盘目标：临时文件 destPath + ".hybrid.downloading"（分片 WriteAt 全落这里，
	// 完成并校验通过后才 rename 到最终 destPath）。manifest 随临时文件（destPath+".hybrid"
	// 不变——源身份/恢复逻辑零改动）。
	dc.working = WorkingPath(dc.destPath)
	// WorkPaths 超长名会把文件名拆成「子目录 + 文件段」→ 子目录可能尚不存在，
	// 下载开始前先 MkdirAll（preallocate 打开文件前目录必须存在）。
	if err := ensureWorkDir(dc.working); err != nil {
		return nil, err
	}
	d.log.Info("hybrid plan", "total", dc.currentTotal, "share_end", shareEnd, "chunk", d.chunkSize)
	chunks := planChunks(0, dc.currentTotal, shareEnd, d.chunkSize)
	d.log.Info("hybrid chunks", "count", len(chunks))
	dc.chunkOffsets = make([]int64, 0, len(chunks))
	dc.shareEnd = shareEnd
	for _, c := range chunks {
		dc.chunkOffsets = append(dc.chunkOffsets, c.offset)
	}
	// 崩溃恢复：读 manifest（源身份 + 工作文件一致性 + 零尾扫描），缺失则创建空 manifest
	// （两文件配套），续传进度基准预置 prog。抽 helper 控制认知复杂度。
	// **必须在 preallocate 之前**：预分配会让「工作文件存在且大小=total」恒真——先分配再校验，
	// 「成品已改名 + manifest 残留」这类异常状态会通过校验 → 跳过全部缺失分片 →
	// 产出大小正确但内容残缺的文件（静默损坏）。
	manifest := d.prepareManifest(dc, shareEnd)
	if err := preallocate(dc.working, dc.currentTotal); err != nil {
		return nil, fmt.Errorf("hybrid preallocate: %w", err)
	}
	if err := d.runChunks(ctx, dc, chunks, shareEnd, manifest); err != nil {
		// G4：失败路径也清理已转存副本（AutoDelete 语义——失败任务不留 6GB 空间占用）。
		dc.lease.Release(ctx)
		return nil, err
	}
	d.log.Info("hybrid chunks done")
	checksum, err := sha256File(dc.working)
	if err != nil {
		return nil, fmt.Errorf("hybrid hash: %w", err)
	}
	// C2 最终完整性校验：**GCID 权威复算**（PikPak file hash 是 GCID = sha1(concat(sha1(分块)))）。
	// 抽 helper 控制认知复杂度（gocognit）——命中权威 → ModeAuthority；未命中 → 报错。
	var verifyErr error
	integrityVerified, verifyErr = d.verifyHybridGCID(ctx, dc)
	if verifyErr != nil {
		return nil, verifyErr
	}
	// 校验通过 → 临时文件原子 rename 到最终 destPath（同目录原子；跨 FS 回退 copy+remove）。
	if err := os.Rename(dc.working, dc.destPath); err != nil {
		return nil, fmt.Errorf("hybrid finalize rename: %w", err)
	}
	// 定稿后才清 manifest（此前在 verify 之前删 → 校验失败即丢续传信息，重跑只能全量重下）。
	removeManifest(dc.destPath)
	// sink 记账：sinkFactory 非空时把已落盘文件重放进 sink（配额语义）。
	if sinkFactory != nil {
		if serr := d.hybridSinkReplay(ctx, dc, sinkFactory); serr != nil {
			return nil, serr
		}
	}
	dc.lease.Release(ctx)
	// 注意：Checksum 是**本地落盘文件的 SHA-256**（自证文件未被篡改/续传拼错），
	// 不是源身份哈希（源身份校验用 GCID 与 target.Hash 比对，已在上方 verify 完成）。
	// 消费方若拿本字段比对源 hash 会误判——仅作本地完整性指纹。
	res := &Result{Size: dc.currentTotal, Checksum: checksum, ChecksumAlgorithm: downloader.IntegrityChecksumSHA256,
		ModTime:           sourceModTime(dc.target, dc.startedAt),
		SourceCreatedTime: sourceCreatedTime(dc.target),
		StartedAt:         dc.startedAt,
		FinishedAt:        time.Now(),
		AnonymousBytes:    dc.anonBytes.Load(),
		AccountBytes:      dc.acctBytes.Load()}
	res.BytesPerSecond = averageBPS(res.StartedAt, res.FinishedAt, res.AnonymousBytes+res.AccountBytes)
	// 完整性三态（与旧 PikpakDownloader finalizeDownload 一致）：
	//   GCID 权威命中（① 态）→ ModeAuthority + 带外权威 hash——语义校验管道据此
	//   识别 hybrid 产出为权威可信，不再重复复算；
	//   未命中 → 显式 ModeLocalOnly（② 态本地自洽，由语义校验器兜底）——
	//   不保持零值 ModeUnknown（语义不明，审计面不一致）。
	if dc.target.Hash != "" && integrityVerified {
		res.Integrity = downloader.ModeAuthority
		res.AuthorityHash = dc.target.Hash
		res.AuthorityHashAlgorithm = downloader.IntegrityAlgPikpakGCID // 下载方声明
	} else {
		res.Integrity = downloader.ModeLocalOnly
	}
	return res, nil
}

// runChunks 并行执行所有 chunk：**分享区与账号区分池并行**（互不阻塞）——
// 分享直链 CDN 对连续 Range 限速（实测每请求 ~4s），账号区单独 pool 可同时下，
// 避免账号区被分享区排队阻塞（总时长 ≈ max(分享区, 账号区) 而非两者之和）。
func (d *HybridDownloader) runChunks(ctx context.Context, dc *downloadCtx, chunks []chunk, shareEnd int64, manifest *hybridManifest) error {
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	// fail-fast：首个分片失败即取消其余分片——不再白耗带宽/账号配额，也避免「部分分片继续写盘
	// 但任务已注定失败」；已下分片仍在 manifest（可续传）。
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	chunks = filterChunks(chunks, manifest) // 崩溃恢复：跳过已完成
	d.emitPendingChunks(dc, chunks)         // per-chunk pending 事件（进度条先行注册行）
	// **每 source 单 worker 并发**（用户明示）：同一 source（share 匿名直链/单账号直链）
	// 内部限速（CDN/账号级），并发无叠加收益；**不同 source 之间真正并行**。
	// 实现：share 源 sem(1) + acct 源 sem(1)（单账号/未细分账号）；多账号时
	// downloadAccountChunkMulti 内再按账号 Select 后 per-account 串行（不同账号并发）。
	// 首个错误记录 + fail-fast 取消（闭包抽离，降认知复杂度）。
	recordErr := func(c chunk, cerr error) {
		mu.Lock()
		first := firstErr == nil
		if first {
			firstErr = &ChunkError{Offset: c.offset, Length: c.length, Err: cerr}
		}
		mu.Unlock()
		if first {
			cancel() // 首个失败即取消其余分片（不再白耗带宽/配额）
		}
	}
	shareSem := make(chan struct{}, 1)
	acctSem := make(chan struct{}, 1)
	// **先 spawn 全部 goroutine**，各自在 goroutine 内 acquire 对应池——循环内同步
	// acquire（FIFO 阻塞）会把账号 chunk 排在分享 chunk 之后（顺序化）。
	// 现在 share 满时循环立即推进到账号 chunk，双源真正并行（总时长 ≈ max(分享, 账号)）。
	for _, c := range chunks {
		wg.Add(1)
		sem := shareSem
		if c.offset >= shareEnd {
			sem = acctSem
		}
		go func(c chunk, sem chan struct{}) {
			defer wg.Done()
			d.runChunkWithSem(cctx, dc, c, shareEnd, sem, recordErr)
		}(c, sem)
	}
	wg.Wait()
	return firstErr
}

// emitPendingChunks 为待下载分片发 pending 事件（per-chunk 回调；pikget 据此注册所有分片行）。
func (d *HybridDownloader) emitPendingChunks(dc *downloadCtx, chunks []chunk) {
	if dc.chunkProgress == nil {
		return
	}
	for _, c := range chunks {
		dc.chunkProgress(ChunkInfo{
			Index: dc.chunkIndex(c.offset), Offset: c.offset, Length: c.length,
			Source: d.chunkSource(dc, c), Phase: "pending", Done: 0, Total: c.length,
		})
	}
}

// runChunkWithSem 在并发槽内下载单个分片：取槽（响应 fail-fast 取消）→ 下载 → 错误上报。
func (d *HybridDownloader) runChunkWithSem(cctx context.Context, dc *downloadCtx, c chunk, shareEnd int64, sem chan struct{}, onErr func(chunk, error)) {
	if !acquireOrDone(cctx, sem) {
		return // fail-fast 已取消：未开始的分片直接跳过（不发事件）
	}
	defer func() { <-sem }()
	if cerr := d.downloadOneChunk(cctx, dc, c, shareEnd); cerr != nil {
		d.metricsInc(func(m *HybridMetrics) { m.AccountSegmentFailed.Add(1) })
		onErr(c, cerr)
	}
}

// acquireOrDone 获取并发槽；ctx 结束时返回 false（fail-fast 后未开始的分片直接跳过）。
func acquireOrDone(ctx context.Context, sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// downloadOneChunk 单个 chunk：分享区（含降级）或账号区。
func (d *HybridDownloader) downloadOneChunk(ctx context.Context, dc *downloadCtx, c chunk, shareEnd int64) error {
	idx := dc.chunkIndex(c.offset)
	var cerr error
	// 标记 downloading（per-chunk 回调，pikget 显示该分片开始下载）
	d.chunkPhase(dc, idx, c, "downloading", 0, nil)
	if c.offset >= shareEnd {
		cerr = d.downloadAccountChunk(ctx, dc, c)
		if cerr == nil {
			dc.acctBytes.Add(c.length)
		}
	} else {
		cerr = d.downloadShareChunk(ctx, dc, c)
		if cerr == nil {
			dc.anonBytes.Add(c.length) // 匿名（免账号配额）通道
		} else {
			d.metricsInc(func(m *HybridMetrics) { m.ShareSegmentFailed.Add(1); m.DowngradeTotal.Add(1) })
			d.log.Warn("hybrid share chunk failed, downgrade to account", "offset", c.offset, "err", cerr)
			cerr = d.downloadAccountChunk(ctx, dc, c)
			if cerr == nil {
				dc.acctBytes.Add(c.length) // 分享区降级到账号通道
			}
		}
	}
	switch {
	case cerr == nil:
		// 完成：写 manifest（崩溃恢复跳过；记录源身份）。
		d.markChunkDone(dc, c, manifestSource{
			ShareID: dc.shareID, FileID: dc.target.ID, Hash: dc.target.Hash, Size: dc.target.Size, URL: dc.sourceURL,
		})
		d.chunkPhase(dc, idx, c, "done", c.length, nil)
	case ctx.Err() != nil:
		// fail-fast 取消的兄弟分片：区别于真失败（进度条显示 cancelled，不误报 failed）。
		d.chunkPhase(dc, idx, c, "cancelled", 0, cerr)
	default:
		d.chunkPhase(dc, idx, c, "failed", 0, cerr)
	}
	return cerr
}

// ensureWorkDir 确保工作文件所在目录存在（WorkPaths 超长名拆出的子目录）。
func ensureWorkDir(working string) error {
	dir := filepath.Dir(working)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hybrid workdir: %w", err)
	}
	return nil
}

// chunkIndex 返回 chunk offset 对应的编号（0 起，按 offset 升序）。
// 简化：runChunks 里 chunks 已是 offset 升序，记录在 dc.chunkOrder 里。
func (dc *downloadCtx) chunkIndex(offset int64) int {
	for i, off := range dc.chunkOffsets {
		if off == offset {
			return i
		}
	}
	return 0
}

// chunkPhase 发 per-chunk 事件（pikget 逐行显示）。
// src 标识来源链：share（匿名分享直链）/ acct（账号直链，多账号时选中账号名在
// tryMultiAccountChunk 内更新为 acct:<name>）。

// chunkSource 返回 chunk 的来源链标识（share=匿名分享直链；acct=账号直链）。
func (d *HybridDownloader) chunkSource(dc *downloadCtx, c chunk) string {
	if c.offset < dc.shareEnd {
		return "share"
	}
	return "acct"
}

func (d *HybridDownloader) chunkPhase(dc *downloadCtx, idx int, c chunk, phase string, done int64, cerr error) {
	if dc.chunkProgress == nil {
		return
	}
	src := "acct"
	if c.offset < dc.shareEnd {
		src = "share"
	}
	dc.chunkProgress(ChunkInfo{
		Index: idx, Offset: c.offset, Length: c.length,
		Source: src, Phase: phase, Done: done, Total: c.length, Err: cerr,
	})
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

func (d *HybridDownloader) downloadShareChunk(ctx context.Context, dc *downloadCtx, c chunk) error {
	link := dc.target.DirectLink
	for attempt := 1; attempt <= 2; attempt++ {
		err := d.downloadChunkRange(ctx, dc, c, link)
		if err == nil {
			d.metricsInc(func(m *HybridMetrics) { m.ShareBytesSaved.Add(c.length) })
			return nil
		}
		d.log.Warn("hybrid share chunk attempt failed", "offset", c.offset, "attempt", attempt, "err", err)
		if attempt == 2 {
			return err // 连续两次失败 → 转账号段
		}
		// 重新 resolve 拿新直链（expire 过期/网络错误）
		link = d.reResolveLink(ctx, dc)
		if link == "" {
			return err
		}
	}
	return nil
}

// downloadAccountChunk 账号区 chunk：转存（幂等一次）→ 定位 → FETCH 直链 → Range 下载。
// 转存经 RestoreLease 内部锁串行化（多账号 chunk 并行时只转存一次）+ FindInDrive 幂等检查。
//
// 多账号分片（round-12）：d.pool 非 nil 时，每 chunk 经 pool.Select（round-robin + 配额预检）
// 选一个账号；该账号首次选中时 Use 内转存一次（dc.accounted 去重），后续该账号的 chunk
// 复用已转存 fid 的 FETCH 直链（每 chunk 重取新直链防 TTL）。不同账号的 chunk 天然并行。
func (d *HybridDownloader) downloadAccountChunk(ctx context.Context, dc *downloadCtx, c chunk) error {
	// 多账号分片（round-12+）：d.pool 非 nil 且池内有账号时走多账号路由。
	// 空池回退单账号路径（round-14：与旧下载器语义一致——已装配池但未添加账号时
	// 用当前 CLI 登录态，不报「no account」硬错；池非空但配额不足才如实报错）。
	if d.pool != nil && len(d.pool.Accounts()) > 0 {
		return d.downloadAccountChunkMulti(ctx, dc, c)
	}
	// 🟡 账号区 chunk 韧性：**取链与下载都退避重试**（hybridChunkAttempts 次）——每次重试
	// 都重新取 FETCH 直链（新链接可绕过 TTL/限流/瞬断）。此前取链失败直接 return：
	// 单次 TLS 握手超时就终止整个任务（2026-10-11 实测 1.3GB/43 片任务因一片取链超时失败）。
	// 仅**永久错误**（未登录/空间不足/无账号/分享失效/不支持）不重试。
	var lastErr error
	for attempt := 1; attempt <= hybridChunkAttempts; attempt++ {
		if attempt > 1 && !sleepCtx(ctx, d.chunkRetryDelay(attempt-1)) {
			return lastErr
		}
		link, err := d.restoreAndLink(ctx, dc)
		if err != nil {
			lastErr = err
			if isPermanentChunkErr(err) {
				return err
			}
			d.log.Warn("hybrid acct link attempt failed", "share", dc.shareID, "offset", c.offset, "attempt", attempt, "err", err)
			continue
		}
		lastErr = d.downloadChunkRange(ctx, dc, c, link)
		if lastErr == nil {
			return nil
		}
		if isPermanentChunkErr(lastErr) {
			return lastErr
		}
		d.log.Warn("hybrid acct chunk attempt failed", "share", dc.shareID, "offset", c.offset, "attempt", attempt, "err", lastErr)
	}
	return lastErr
}

// downloadAccountChunkMulti 多账号分片：Select 选账号 → Use 内转存（去重）+ FETCH 下载。
// 失败重试 1 次；账号级失败 MarkFailed 冷却 + 重 Select（换账号）。
func (d *HybridDownloader) downloadAccountChunkMulti(ctx context.Context, dc *downloadCtx, c chunk) error {
	for attempt := 1; ; attempt++ {
		// 配额预检按**chunk 长度**（round-13）：账号只下载其分到的 chunk——转存整文件占的是
		// 该账号**网盘空间**（不占每日下载配额），Select 的 neededBytes 语义是「本次要下的字节」。
		// 此前按整文件大小预检，文件大于单账号剩余配额时池能力充沛也 ErrNoAccountAvailable，
		// 多账号分摊大文件的核心场景被自己堵死；按 chunk 预检让 round-robin 按剩余配额自平衡，
		// 某账号配额耗尽后 Select 自动跳过换下一账号。
		acct, serr := d.pool.Select(ctx, c.length)
		if serr != nil {
			return fmt.Errorf("hybrid multi-account: no account available: %w", serr)
		}
		// **每账号单 worker**：同账号 chunk 串行（账号级限速，并发无叠加）；
		// 不同账号之间并发（各自限速独立）。
		acctSem := d.acctSem(acct.Name)
		acctSem <- struct{}{}
		err := d.tryMultiAccountChunk(ctx, dc, c, acct)
		<-acctSem
		if err == nil {
			return nil
		}
		if attempt == hybridChunkAttempts || isPermanentChunkErr(err) {
			return err
		}
	}
}

// chunkRetryDelay 返回第 retry 次分片重试前的退避（retry 从 1 起；指数 + 抖动，封顶）。
func (d *HybridDownloader) chunkRetryDelay(retry int) time.Duration {
	base := d.chunkBackoff
	if base <= 0 {
		base = defaultChunkRetryBackoff
	}
	return retryPolicy{Backoff: base}.delay(retry)
}

// isPermanentChunkErr 判断分片错误是否不可重试（重试也不会变好）：登录失效/空间不足/
// 无可用账号/分享失效/不支持。网络瞬断、HTTP 4xx/5xx、取链失败一律可重试
// （重试会重新取链：链接 TTL 过期/限流/握手超时都可恢复）。
func isPermanentChunkErr(err error) bool {
	return errors.Is(err, ErrNotLoggedIn) || errors.Is(err, ErrStorageFull) ||
		errors.Is(err, ErrNoAccountAvailable) || errors.Is(err, ErrShareNotFound) ||
		errors.Is(err, ErrUnsupported)
}

// isAccountFaultErr 判断取链失败是否属**账号级过错**（需冷却该账号）：
// 登录失效/空间不足。传输层瞬断（TLS 握手超时/连接重置）与 API 临时 5xx 不算——
// 否则一次网络抖动会冤枉健康账号，冷却期白扣一个可用账号。
func isAccountFaultErr(err error) bool {
	if _, ok := errors.AsType[net.Error](err); ok { // 传输层瞬断（*net.OpError / *url.Error 均实现 net.Error）
		return false
	}
	return errors.Is(err, ErrNotLoggedIn) || errors.Is(err, ErrStorageFull)
}

// tryMultiAccountChunk 单次尝试：取账号直链 + 下载 chunk + 记账。失败按过错分类：
// 取链失败（转存/直链——账号级过错）MarkFailed 冷却；下载失败不冷却（网络抖动可重试，
// 重试时 Select 按最新用量自动换账号——其他任务并发扣减使剩余不足时会跳过）。
func (d *HybridDownloader) tryMultiAccountChunk(ctx context.Context, dc *downloadCtx, c chunk, acct *Account) error {
	link, linkErr := d.multiAccountLink(ctx, dc, acct)
	if linkErr != nil {
		// 仅账号级过错才 MarkFailed 冷却（传输层瞬断不算——见 isAccountFaultErr）。
		if ctx.Err() == nil && isAccountFaultErr(linkErr) {
			_ = d.pool.MarkFailed(ctx, acct.Name)
		}
		d.log.Warn("hybrid multi-account use/link failed", "acct", acct.Name, "err", linkErr)
		return linkErr
	}
	if err := d.downloadChunkRange(ctx, dc, c, link); err != nil {
		d.log.Warn("hybrid multi-account chunk attempt failed", "acct", acct.Name, "offset", c.offset, "err", err)
		return err
	}
	// 成功记账（配额扣减；persist 失败仅配额缓存漂移，下次 Select 预检兜底）
	if rerr := d.pool.RecordUsage(ctx, acct.Name, c.length); rerr != nil {
		d.log.Warn("hybrid multi-account record usage", "acct", acct.Name, "err", rerr)
	}
	d.metricsInc(func(m *HybridMetrics) { m.AccountBytesUsed.Add(c.length) }) // 账号区字节可观测
	return nil
}

// acctSem 返回某账号的 per-account semaphore（容量 1：同账号 chunk 串行，不同账号并发）。
func (d *HybridDownloader) acctSem(name string) chan struct{} {
	d.acctSemMu.Lock()
	defer d.acctSemMu.Unlock()
	if sem, ok := d.acctSems[name]; ok {
		return sem
	}
	sem := make(chan struct{}, 1)
	d.acctSems[name] = sem
	return sem
}

// multiAccountLink 为 chunk 获取账号直链。round-13 Critical 修复：**全部会话相关 API 调用
// 都放在 Use（sessionMu）内**——此前 accounted 命中路径的 DownloadLink 在 Use 外执行，
// 并发 Use 切换会话后 API 缓存的是上一账号 token，以错账号 token 签名请求正确 fid → 403
// → MarkFailed 冤枉健康账号 + 重试链可能双双失败导致整任务失败。Use 串行化后每次重读
// 当前账号凭证，会话亲和。
//
// 该账号已转存（accounted 命中）→ 复用 fid 直链；否则 Use 内转存一次并拿直链
// （每次重取新直链防 TTL）。accounted 去重回答「每账号只转存一次」（避免 N×重复转存占空间）。
func (d *HybridDownloader) multiAccountLink(ctx context.Context, dc *downloadCtx, acct *Account) (string, error) {
	var link string
	useErr := d.pool.Use(ctx, acct.Name, func() error {
		// 会话已切到 acct：清 token 缓存，让后续 REST 从新账号凭据文件重读（C1；含 accounted
		// 命中路径——上一次 Use 缓存的可能是其他账号 token）。
		d.api.ResetToken()
		// 锁内二次确认：并发 chunk 同账号首转存时只转一次（Use 串行化保证第二份读到已写入值）。
		dc.acctMu.Lock()
		f, ok := dc.accounted[acct.Name]
		dc.acctMu.Unlock()
		if !ok {
			var rerr error
			f, rerr = d.restoreForAccount(ctx, dc, acct.Name)
			if rerr != nil {
				return rerr
			}
			dc.acctMu.Lock()
			dc.accounted[acct.Name] = f
			dc.acctMu.Unlock()
		}
		l, lerr := d.api.DownloadLink(ctx, f)
		if lerr != nil {
			return fmt.Errorf("hybrid multi-account link %s: %w", acct.Name, lerr)
		}
		link = l
		return nil
	})
	if useErr != nil {
		return "", useErr
	}
	return link, nil
}

// resolvedFileFromTarget 把分享目标文件转成幂等检查口径（与旧下载器 resolveFileMeta 对应）。
func resolvedFileFromTarget(t *ShareFile) *downloader.ResolvedFile {
	if t == nil {
		return nil
	}
	return &downloader.ResolvedFile{ID: t.ID, Name: t.Name, Size: t.Size, Hash: t.Hash}
}

// restoreForAccount 在 Use 会话内转存分享 + 定位转存文件，返回转存文件 ID（该账号网盘内）。
// 复用单账号 restoreAndLink 的幂等逻辑（FindInDrive 命中 + RestoreShare + locateRestored）。
// 转存副本的 AutoDelete 登记按账号 TrackIn（Release 逐账号会话删除，round-13 Critical）。
func (d *HybridDownloader) restoreForAccount(ctx context.Context, dc *downloadCtx, acctName string) (string, error) {
	// 幂等：先查同名同大小同 hash 已存在的转存文件（该账号网盘内）
	if _, id, ok := d.idempotentRestored(ctx, dc, acctName); ok {
		return id, nil
	}
	fid, owned, err := d.api.RestoreShare(ctx, dc.shareID, []string{dc.target.ID}, "")
	if err != nil {
		return "", fmt.Errorf("hybrid restore %s: %w", dc.shareID, err)
	}
	driveFile, err := d.locateRestored(ctx, fid)
	if err != nil {
		return "", err
	}
	if !owned {
		dc.lease.TrackIn(acctName, driveFile.ID) // AutoDelete 释放时按该账号会话永久删
	}
	return driveFile.ID, nil
}

// restoreAndLink 转存（幂等）+ 定位转存文件 + 拿 FETCH 直链。
// 幂等：restore 前先按**名字+大小**查网盘（FindInDrive 精确匹配）——已转存过则
// 跳过 RestoreShare 直接复用（避免多次 restore 累积同名副本占满 6GB 空间）；
// 未命中才 restore（restore 返回文件夹时进文件夹找文件）。
func (d *HybridDownloader) restoreAndLink(ctx context.Context, dc *downloadCtx) (string, error) {
	if dc.lease.HasTracked() {
		// 已转存：直接用记录的文件 id 拿直链（最近一个；每次 DownloadLink 重取新链接防 TTL 过期）
		last := dc.lease.LastID()
		link, err := d.api.DownloadLink(ctx, last)
		if err != nil {
			return "", fmt.Errorf("hybrid fetch link (cached): %w", err)
		}
		return link, nil
	}
	// round-7 性能：idempotent/owned **未登记**命中缓存文件 ID——避免每账号 chunk 重跑
	// FindInDrive 全盘 walk（不登记即不删，NH-P1 语义不变）。与 lease 缓存路径一致：每次经
	// DownloadLink 重取**新 FETCH 直链**（round-8 修正：缓存直链会在 TTL 过期后让长下载
	// 全部后续账号 chunk 失败——链接必须按 chunk 刷新）。
	dc.reusedMu.Lock()
	if dc.reusedID != "" {
		link, err := d.api.DownloadLink(ctx, dc.reusedID)
		dc.reusedMu.Unlock()
		if err != nil {
			return "", fmt.Errorf("hybrid fetch link (reused): %w", err)
		}
		return link, nil
	}
	dc.reusedMu.Unlock()
	// 幂等：先查同名同大小已存在的转存文件（避免重复 restore 累积副本），
	// 命中且 hash 一致则复用（抽取 helper 控制复杂度；"" = 当前会话/单账号）。
	if link, id, ok := d.idempotentRestored(ctx, dc, ""); ok {
		dc.reusedMu.Lock()
		dc.reusedID = id
		dc.reusedMu.Unlock()
		return link, nil
	}
	fid, owned, err := d.api.RestoreShare(ctx, dc.shareID, []string{dc.target.ID}, "")
	if err != nil {
		return "", fmt.Errorf("hybrid restore %s: %w", dc.shareID, err)
	}
	// 定位：fid 可能是文件夹（Pack From Shared）或文件
	driveFile, err := d.locateRestored(ctx, fid)
	if err != nil {
		return "", err
	}
	if owned {
		// file_restore_own（源文件已在个人网盘，NH-P1）：不记入 lease——
		// AutoDelete 永久删只清本次 restore 副本，**绝不得删除用户自己的源文件**。
		d.log.Info("hybrid restore owned (skip delete registration)", "id", driveFile.ID)
	} else {
		dc.lease.Track(driveFile.ID) // 本次 restore 副本（AutoDelete 时 Release 永久删）
	}
	link, err := d.api.DownloadLink(ctx, driveFile.ID)
	if err != nil {
		return "", fmt.Errorf("hybrid fetch link: %w", err)
	}
	if owned {
		dc.reusedMu.Lock()
		dc.reusedID = driveFile.ID // round-7 性能 + round-8 修正：缓存文件 ID（每 chunk 重取新直链）
		dc.reusedMu.Unlock()
	}
	return link, nil
}

// locateRestored 定位转存文件：fid 若是文件夹（Pack From Shared），列其内容找
// 目标 mp4（RestoreShare 对单文件可能包文件夹）；否则直接用 fid。
func (d *HybridDownloader) locateRestored(ctx context.Context, fid string) (*FileMeta, error) {
	// 收敛：与旧下载器共用 restore_idempotent.go 的 locateRestoredFileIn（唯一实现）。
	// 空 wantName = 按「视频优先 → 第一个文件」挑选（hybrid 原语义）。
	return locateRestoredFileIn(ctx, d.api, fid, "")
}

// hasVideoExt 判断视频扩展名（委托 pkg/media.IsVideoName 单一事实源，2026-10-21 收敛）。
func hasVideoExt(name string) bool {
	return media.IsVideoName(name)
}

// 注：转存副本释放已收归 RestoreLease（restore.go，round-10 用户裁决）——
// 单一删除语义 DeletePermanent，hybrid/旧下载器/未来扩展共用，不再各自实现。

// downloadChunkRange 单个 chunk Range 下载 → WriteAt(offset)。
// 校验：206/200 + 写入字节数 == chunk 长度（数据正确性保障）。
func (d *HybridDownloader) downloadChunkRange(ctx context.Context, dc *downloadCtx, c chunk, link string) error {
	resp, err := d.doChunkRequest(ctx, c, link)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return d.writeChunkBody(resp, c, dc)
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
func (d *HybridDownloader) writeChunkBody(resp *http.Response, c chunk, dc *downloadCtx) error {
	if err := d.verifyContentRange(resp, c, dc.currentTotal); err != nil {
		return err
	}
	f, err := os.OpenFile(dc.working, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	// per-chunk 进度：下载中字节实时回调。**该 chunk 的 Done 必须是本地累计**（written），
	// 不能是全局 prog（否则每个 chunk 都显示全局累计 → 超出 64MB + 全部一起涨）。
	idx := dc.chunkIndex(c.offset)
	progWithChunk := func(downloaded, total int64) {
		if dc.onProgress != nil {
			dc.onProgress(downloaded, total)
		}
	}
	chunkLocal := func(chunkDone int64) {
		if dc.chunkProgress != nil {
			dc.chunkProgress(ChunkInfo{
				Index: idx, Offset: c.offset, Length: c.length,
				Source: d.chunkSource(dc, c), Phase: "downloading",
				Done: chunkDone, Total: c.length,
			})
		}
	}
	written, werr := copyRangeToFile(resp.Body, f, c.offset, c.length, &dc.prog, dc.currentTotal, progWithChunk, chunkLocal)
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
func copyRangeToFile(r io.Reader, f *os.File, offset int64, want int64, prog *atomic.Int64, total int64, onProgress downloader.ProgressFunc, onChunkLocal func(chunkDone int64)) (int64, error) {
	buf := make([]byte, 1<<20)
	var written int64
	for {
		n, rerr := r.Read(buf)
		if n > 0 {
			// 写盘 + 进度（全局 prog + per-chunk 本地）；写错返回错误
			wn, werr := writeChunkBytes(f, buf[:n], offset, written, prog, total, onProgress, onChunkLocal)
			if werr != nil {
				return written, werr
			}
			written = wn
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

// writeChunkBytes 写一个缓冲块并更新进度（全局 + per-chunk），返回新 written。
func writeChunkBytes(f *os.File, p []byte, offset, base int64, prog *atomic.Int64, total int64, onProgress downloader.ProgressFunc, onChunkLocal func(chunkDone int64)) (int64, error) {
	if _, werr := f.WriteAt(p, offset+base); werr != nil {
		return 0, werr
	}
	base += int64(len(p))
	prog.Add(int64(len(p)))
	if onProgress != nil {
		onProgress(prog.Load(), total)
	}
	if onChunkLocal != nil {
		onChunkLocal(base)
	}
	return base, nil
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
		if files[i].Kind != "" && files[i].Kind != driveKindFile {
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
		if files[i].Kind == "" || files[i].Kind == driveKindFile {
			if best == nil || files[i].Size > best.Size {
				best = &files[i]
			}
		}
	}
	return best
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
	end := min(offset+probeSize-1, total-1)
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

// ChunkInfo 是单个分片（worker）的进度信息，per-chunk 回调传递。
// Source 标识该 chunk 由哪条链下载：share（匿名分享直链）/ acct:<name>（账号直链）。
// Phase: pending / downloading / done / failed。
type ChunkInfo struct {
	Index  int    // chunk 编号（0 起，按 offset 排序）
	Offset int64  // 文件偏移
	Length int64  // chunk 大小
	Source string // share / acct:<账号名> / acct（单账号）
	Phase  string // pending / downloading / done / failed
	Done   int64  // 该 chunk 已下字节
	Total  int64  // 该 chunk 总字节
	Err    error  // failed 时错误（nil 表示非失败）
}

// ChunkProgressFunc 是 per-chunk 进度回调（pikget 用于逐行显示每个分片）。
type ChunkProgressFunc func(info ChunkInfo)

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
	// URL 是原始分享 URL（含 token 时原样保留）——**仅供 CLI 恢复用**（rebuild 免重输 URL）。
	// 不参与 SourceMatches 判定（同一分享可有多种 URL 形态，且旧 manifest 无此字段）。
	URL string `json:"url,omitempty"`
}

// loadManifest 读取 manifest（不存在返回 nil）。
func loadManifest(destPath string) *hybridManifest {
	data, err := os.ReadFile(ManifestPath(destPath))
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

// initManifest 创建并落盘初始 manifest（空 Chunks + 源身份）——
// 下载开始即与 .hybrid.downloading 配套出现，中断/立即退出也两个文件都在。
func (d *HybridDownloader) initManifest(dc *downloadCtx, shareEnd int64) *hybridManifest {
	m := &hybridManifest{
		Total: dc.currentTotal, ShareEnd: shareEnd,
		Chunks: map[int64]int64{},
		Source: manifestSource{ShareID: dc.shareID, FileID: dc.target.ID, Hash: dc.target.Hash, Size: dc.target.Size, URL: dc.sourceURL},
	}
	writeManifest(dc.destPath, m)
	return m
}

// sourceModTime 返回源文件修改时间：优先提供方带外 modified，其次 created；
// 都未知时回落 fallback（下载开始时刻——**不是**完成时刻，避免时间语义漂移），
// 仍未设置则取当前时刻。用户裁定 2026-10-28：不得用「下载完成时刻」冒充源时间。
func sourceModTime(target *ShareFile, fallback time.Time) time.Time {
	if target != nil {
		if !target.ModifiedTime.IsZero() {
			return target.ModifiedTime
		}
		if !target.CreatedTime.IsZero() {
			return target.CreatedTime
		}
	}
	if !fallback.IsZero() {
		return fallback
	}
	return time.Now()
}

// sourceCreatedTime 返回源文件创建时间（未知 → 零值，消费方不得顶替）。
func sourceCreatedTime(target *ShareFile) time.Time {
	if target == nil {
		return time.Time{}
	}
	return target.CreatedTime
}

// averageBPS 平均带宽（字节/秒）；任一时刻为零或耗时为 0 → 0（不编造）。
func averageBPS(start, end time.Time, total int64) int64 {
	if start.IsZero() || end.IsZero() || total <= 0 {
		return 0
	}
	secs := end.Sub(start).Seconds()
	if secs <= 0 {
		return 0
	}
	return int64(float64(total) / secs)
}

// markChunkDone 原子记录 chunk 完成（追加到 manifest；首次记录源身份）。
func (d *HybridDownloader) markChunkDone(dc *downloadCtx, c chunk, src manifestSource) {
	dc.manifestMu.Lock()
	defer dc.manifestMu.Unlock()
	m := loadManifest(dc.destPath)
	// 🟠4 加固：磁盘 manifest 源与本次不一致（异常路径残留旧源）→ 重建空 manifest（不合并旧 chunk）。
	if m != nil && !m.SourceMatches(src.ShareID, &ShareFile{ID: src.FileID, Size: src.Size, Hash: src.Hash}) {
		m = &hybridManifest{Chunks: map[int64]int64{}}
	}
	if m == nil {
		m = &hybridManifest{Chunks: map[int64]int64{}}
	}
	m.Chunks[c.offset] = c.length
	m.Source = src
	writeManifest(dc.destPath, m)
}

// removeManifest 完成时删除 manifest。
func removeManifest(destPath string) {
	_ = os.Remove(ManifestPath(destPath))
}

// reResolveLink 重新 resolve 拿新直链，并核对新目标与首次 target 一致（C3）。
// 返回新直链；不一致（ID/hash/size 变化）返回空（调用方放弃重试）。
func (d *HybridDownloader) reResolveLink(ctx context.Context, dc *downloadCtx) string {
	// round-8 Minor：RefreshLink 轻量重取**单文件**直链（shareDetail 一次 + 单 file_info），
	// 不再全量重列所有文件（大分享每次 chunk 重试 O(N)）；round-11：带 shareToken（token 分享）。
	link, hash, err := d.resolver.RefreshLink(ctx, dc.shareID, dc.target.ID, dc.shareToken)
	if err != nil {
		d.log.Warn("hybrid re-resolve failed, abort retry", "share", dc.shareID, "file", dc.target.ID, "err", err)
		return ""
	}
	// C3：重取后核对新直链内容一致（分享被换 → 拒绝重试，防与已成功 chunk 拼接混合损坏）。
	if dc.target.Hash != "" && hash != "" && hash != dc.target.Hash {
		d.log.Warn("hybrid re-resolve hash changed, abort retry", "old_hash", dc.target.Hash, "new_hash", hash)
		return ""
	}
	return link
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

// loadValidManifest 读 manifest 并校验可用性（C4 源身份 + 工作文件一致性）：
//   - 源身份不一致（不同分享/文件）→ 归档旧 manifest，返回 nil（全量重下）；
//   - 工作文件缺失或大小 != total（用户实测状态：数据文件被改名成最终名后 manifest 残留）→
//     归档 manifest，返回 nil——绝不能凭 manifest 假装续传：跳过缺失分片会产出
//     「大小正确但内容残缺」的文件（静默损坏），且最终校验失败后反复重试。
//
// 归档（而非删除）到 .hybrid.stale：保留源身份/分片清单证据，rebuild 命令可据此定点修复。
func (d *HybridDownloader) loadValidManifest(destPath, shareID string, target *ShareFile) *hybridManifest {
	m := loadManifest(destPath)
	if m == nil {
		return nil
	}
	if !m.SourceMatches(shareID, target) {
		d.archiveManifest(destPath, "manifest source mismatch, full re-download", "manifest_source", m.Source.ShareID)
		return nil
	}
	if !workingFileMatches(destPath, m.Total) {
		d.archiveManifest(destPath, "working file missing or size mismatch, full re-download",
			"total", m.Total, "working", WorkingPath(destPath))
		return nil
	}
	// 记录一致还不够：磁盘上可能有「被跳过但未写入」的零洞（预分配让大小恒等于目标，
	// 崩溃/断电时 manifest 可能先于数据落盘）——零尾扫描剔除，交由本次下载补齐（自愈）。
	d.dropZeroTailChunks(destPath, m)
	return m
}

// dropZeroTailChunks 剔除「manifest 记录完成但磁盘上尾部连续为零」的分片（未写完）。
// 直接信任 manifest 会跳过这些块 → 大小正确但内容残缺（静默损坏）＋校验反复失败。
// 剔除后 markChunkDone 会重新记录，本次下载补下这些块。
func (d *HybridDownloader) dropZeroTailChunks(destPath string, m *hybridManifest) {
	if m == nil || len(m.Chunks) == 0 {
		return
	}
	// 用 manifest 自己记录的块长度（不是下载器当前 chunkSize——旧 manifest 可能是别的大小）。
	spans := make([]ChunkSpan, 0, len(m.Chunks))
	for off, ln := range m.Chunks {
		if ln <= 0 || off < 0 || off >= m.Total {
			continue
		}
		spans = append(spans, ChunkSpan{Offset: off, Length: min(ln, m.Total-off)})
	}
	suspect, err := ScanZeroTail(WorkingPath(destPath), spans, ZeroScanOptions{})
	if err != nil {
		d.log.Warn("hybrid manifest zero-tail scan failed, trust manifest", "err", err)
		return
	}
	if len(suspect) == 0 {
		return
	}
	for _, s := range suspect {
		delete(m.Chunks, s.Offset)
	}
	d.log.Warn("hybrid manifest chunks dropped (zero tail, will re-download)", "count", len(suspect))
	writeManifest(destPath, m)
}

// writeManifest 落盘 manifest（错误忽略：进度文件问题只影响续传效率，不影响正确性）。
func writeManifest(destPath string, m *hybridManifest) {
	if m == nil {
		return
	}
	if data, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(ManifestPath(destPath), data, 0o644)
	}
}

// workingFileMatches 判断工作文件（临时文件）存在且大小与 manifest.Total 一致。
func workingFileMatches(destPath string, total int64) bool {
	if total <= 0 {
		return false
	}
	fi, err := os.Stat(WorkingPath(destPath))
	return err == nil && !fi.IsDir() && fi.Size() == total
}

// archiveManifest 把当前 manifest 归档为 .hybrid.stale（覆盖上一个归档）并告警——
// 保留进度证据供诊断/rebuild，不静默丢弃。
func (d *HybridDownloader) archiveManifest(destPath, reason string, args ...any) {
	d.log.Warn("hybrid "+reason, args...)
	_ = os.Rename(ManifestPath(destPath), StaleManifestPath(destPath))
}

// driveFileID 返回 FileMeta 的 ID（nil 安全，日志用）。

// idempotentRestored 幂等检查：网盘已有同名同大小且 hash 一致的转存 → 复用并记录，返回 (link, true)。
// 收敛：判据唯一实现在 restore_idempotent.go 的 checkRestoreCopy（与旧下载器共用）。
// acctName 为账号名（"" = 当前会话/单账号），用于按账号登记 AutoDelete 副本（多账号 Release 逐会话删）。
func (d *HybridDownloader) idempotentRestored(ctx context.Context, dc *downloadCtx, acctName string) (link, id string, ok bool) {
	rc := checkRestoreCopy(ctx, d.api, resolvedFileFromTarget(dc.target))
	if !rc.OK || rc.File == nil {
		return "", "", false
	}
	existing := rc.File
	// hash 双方都有且不等 → 拒绝复用（不同文件，防内容错配）。
	if dc.target.Hash != "" && existing.Hash != "" && existing.Hash != dc.target.Hash {
		d.log.Warn("hybrid restore skip rejected: hash mismatch",
			"id", existing.ID, "target_hash", dc.target.Hash, "drive_hash", existing.Hash)
		return "", "", false
	}
	// NH-P1 加固：只登记确认是 restore 副本的文件供 AutoDelete 删除（用户自有文件
	// 同内容 → 只复用、绝不登记删除——判据在 restore_idempotent.go 唯一实现）。
	if rc.IsRestoreCopy {
		dc.lease.TrackIn(acctName, existing.ID) // restore 副本（AutoDelete 时按账号会话 Release 永久删）
		// 已知限制（2026-10-05 评审记录，非本轮引入）：并发任务共享同一分享时，两任务可能
		// 都 idempotent 命中同一 restore 副本并各自 Track——先完成方 Release 删除后，
		// 后完成方在途 chunk 会失败。属跨任务协调问题（需 per-task 租约/删除引用计数）。
	} else {
		d.log.Info("hybrid restore reuse (non-restore-copy, skip delete registration)",
			"id", existing.ID, "name", existing.Name)
	}
	// 取直链（每次重取新链接防 TTL 过期；复用既有副本不 Track 删除——副本非本次新建）。
	link, lerr := d.api.DownloadLink(ctx, existing.ID)
	if lerr != nil {
		return "", "", false
	}
	return link, existing.ID, true
}
