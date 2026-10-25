// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// stage_downloader.go —— **窗口级缓存下载器**（2026-10-25 重构 v2：blocklet 聚合窗口 +
// 动态前瞻预下载）。
//
// 背景（真机实测）：百度网盘账号级限速 ~7.8MB/s。旧 v1「整块持续窗口追加」有两个不足：
//   - 下载粒度仍是「整块」，读 target 段前必须等水位从块头推进到该段（200MB 块 ⇒
//     首窗命中要等整块前面的窗口顺序下载完）——顺序读 2.1GB 实测 551s（3.9MB/s，
//     平均仅半速）；
//   - 无法跳读（seek 到块中部要等水位追上）。
//
// v2 设计（用户裁定 2026-10-25）：
//   - **下载前先按 blocklet 密文边界聚合窗口**（planCacheWindows，见 stage_window.go）：
//     目标 4MiB/窗，窗口边界恒落在 blocklet 上（GCM 段不可分割，不切分 blocklet）；
//   - **每个窗口独立 Range 下载 + 独立缓存文件**（缓存名 = chunk key + 窗口位点）；
//     读某个 blocklet ⇒ 只下它所属窗口（跳读 O(窗口数)，不依赖顺序水位）；
//   - **动态前瞻**：根据读进度维护「未下载窗口数 ≥ lookAhead」。lookAhead 初始 = 1，
//     消费快（客户端 1s > 1 窗）⇒ 增加；慢 ⇒ 回落。维持未下载窗口数与带宽匹配：
//     窗口 4MiB @ 带宽 8MB/s = 0.5s/窗 ⇒ 客户端 1s 消费 2 窗则 lookAhead ≥ 2。
//
// 语义边界（安全，与 v1 一致）：
//   - 只缓存密文窗口（盘上不落明文）；读路径 GCM 认证，篡改 fail-closed；
//   - 同一窗口同时只有一个下载者（inflight 去重）；sem 满排队不丢弃；
//   - 写/删文件后 invalidate（同前缀失效）；
//   - 无 RangeReader 能力回退旧整块 OpenRead 路径（零回归）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// stageDL 是窗口级缓存下载器（每 SecretdataFS 一份）。
//
// **缓存位置按逻辑路径组织（2026-10-25 用户裁定）**：窗口缓存文件落
// `<SpanCacheDir>/stage/<逻辑目录路径>/<原始文件名>.<chunkIndex>.W<窗口位点>.blk`，
// 支持超大 staging 空间下全量缓存（文件名即窗口索引，重启后可复用）。
type stageDL struct {
	dir    string // 缓存根（<SpanCacheDir>/stage）
	budget int64  // 单块字节上限（超过则不下，回源）
	fs     *SecretdataFS

	mu sync.Mutex
	// inflight 正在下载的窗口 key（同窗只一个下载者）。
	inflight map[string]struct{}
	// plan cache：rel|chunkIndex → 该块窗口规划（惰性计算一次）。
	plans map[string][]cacheWindow
	// ahead 是动态前瞻跟踪器（每文件一个；按读进度维护未下载窗口数 ≥ lookAhead）。
	ahead *lookAheadTracker
	// blobCache 是窗口密文内存 LRU（2026-10-25 实测：读窗口与写窗口边界不对齐 ⇒ 每
	// 读窗口重复读 2 个 4MiB 窗口文件 + 2 次 sha256 ⇒ 6083 次/40s ⇒ 每 chunk 2.1s；
	// 内存缓存命中后零磁盘零 sha256）。nil = 关闭（零回归）。
	blobCache *windowBlobCache
	// sem 限制**并发窗口下载数**：网盘账号级限速 ~8MB/s；16 并发可在带宽内更快补足
	// 前瞻窗口（实测 8 并发时客户端读等窗口 ~8s/波；16 并发减半）。
	sem chan struct{}
}

// newStageDL 构造窗口级缓存下载器；dir 为空/预算≤0 返回 nil（关闭，零回归）。
func newStageDL(dir string, budget int64, fs *SecretdataFS) *stageDL {
	if dir == "" || budget <= 0 || fs == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "secretdata: 创建缓存目录失败（关闭窗口下载）: %v\n", err)
		return nil
	}
	return &stageDL{
		dir: dir, budget: budget, fs: fs,
		inflight:  map[string]struct{}{},
		plans:     map[string][]cacheWindow{},
		ahead:     newLookAheadTracker(defaultLookAheadConfig()),
		blobCache: newWindowBlobCache(0),
		sem:       make(chan struct{}, 16),
	}
}

// stageWinKey 是窗口级缓存 key（文件逻辑路径 + 块序号 + 窗口位点）。
// 与 stageWinPath 一一对应（key 内的逻辑路径段用于组织目录，块/窗位点用于定名）。
func (d *stageDL) stageWinKey(e *metaEntry, rel string, ci shardseal.ChunkInfo, w *cacheWindow) string {
	return fmt.Sprintf("%s|%d|W%d|%d|%d", rel, ci.Index, w.Index, w.EncStart, w.EncEnd)
}

// stageWinPath 返回窗口缓存文件路径：**按原始文件目录 + 块 + 窗口位点组织**（用户裁定）：
// `<dir>/<rel 目录>/<原始文件名>.<chunkIndex>.W<窗口位点>.blk`。
// 窗口位点 = 窗口在块内的序号（0-based）——超大 staging 下全量缓存可索引/复用。
func (d *stageDL) stageWinPath(key string) string {
	parts := strings.SplitN(key, "|", 5)
	if len(parts) < 5 {
		return filepath.Join(d.dir, "blob-"+hex.EncodeToString(sha256sum(key)))
	}
	rel, chunkIdx, wIdx, encStart, encEnd := parts[0], parts[1], parts[2], parts[3], parts[4]
	relDir, base := path.Split(rel)
	// 文件名含块 + 窗口位点（encStart-encEnd 便于调试/审计；sha256 定长安全名防路径穿越）。
	name := fmt.Sprintf("%s.%s.W%s.%s-%s.blk", base, chunkIdx, wIdx, encStart, encEnd)
	return filepath.Join(d.dir, filepath.FromSlash(relDir), name)
}

// sha256sum 返回字节串的 sha256 hex（定长安全名）。
func sha256sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// windowReady 判定窗口缓存文件齐备（存在且大小 == 窗口密文长）。
func (d *stageDL) windowReady(key string, want int64) bool {
	if d == nil {
		return false
	}
	fi, err := os.Stat(d.stageWinPath(key))
	if err != nil || fi.IsDir() {
		return false
	}
	return want <= 0 || fi.Size() == want
}

// planFor 返回块的窗口规划：**优先用写侧确定性窗口（ci.Windows）**（2026-10-25 用户设计：
// 首位相连 + salt 入首窗 + 每窗 checksum）；旧 meta 无 Windows ⇒ 动态规划兜底（零回归）。
func (d *stageDL) planFor(e *metaEntry, ci shardseal.ChunkInfo) []cacheWindow {
	if d == nil {
		return nil
	}
	planKey := e.dirSeg + "|" + e.metaName + "|" + fmt.Sprintf("%d", ci.Index)
	d.mu.Lock()
	if w, ok := d.plans[planKey]; ok {
		d.mu.Unlock()
		return w
	}
	var w []cacheWindow
	if len(ci.Windows) > 0 {
		w = cacheWindowsFromMeta(ci)
	} else {
		w = planCacheWindows(ci, stageWindowTargetBytes)
	}
	if w == nil {
		w = []cacheWindow{}
	}
	d.plans[planKey] = w
	d.mu.Unlock()
	return w
}

// winBlob 是一个就绪窗口的密文与其需解密的 blocklet 下标。
type winBlob struct {
	Win   *cacheWindow
	Data  []byte
	Blets []int
}

// readWindows 读取 [offset,end) 相交 blocklet 的**窗口并集**的密文（独立窗口缓存读取）。
// 返回 (就绪窗口列表, ok)：
//   - 全部窗口就绪 ⇒ ok=true（每窗独立读缓存文件）；
//   - 任一窗口未就绪 ⇒ ok=false（调用方回源），并触发下载（含前瞻调度）。
func (d *stageDL) readWindows(ctx context.Context, o chunkRangeOpts, offset, end int64) ([]winBlob, bool) {
	if d == nil {
		return nil, false
	}
	wins := d.planFor(o.e, o.ci)
	if len(wins) == 0 {
		return nil, false
	}
	need, blets := windowsForRange(wins, o.ci.Blocklets, offset, end)
	if len(need) == 0 {
		return nil, true
	}
	// 记录读进度（推进前瞻水位）——已读窗口 = 相交窗口的全局序号（本块内首窗）。
	// 前瞻调度：读水位 + lookAhead 范围内的未下载窗口入队（维持缓冲）。
	d.advanceAhead(o, need)
	// 触发未就绪窗下载（含前瞻调度）。
	for _, w := range need {
		key := d.stageWinKey(o.e, o.rel, o.ci, w)
		if !d.windowReady(key, w.EncEnd-w.EncStart) {
			d.ensureWindowDownload(o.e, o.rel, o.ci, w)
		}
	}
	// 有界等待目标窗全部就绪（超时 ⇒ 回源）。
	if !d.waitWindowsReady(o.e, o.rel, o.ci, need) {
		return nil, false
	}
	out := make([]winBlob, 0, len(need))
	for i, w := range need {
		key := d.stageWinKey(o.e, o.rel, o.ci, w)
		seg, ok := d.readWindowData(key, w)
		if !ok {
			return nil, false
		}
		out = append(out, winBlob{Win: w, Data: seg, Blets: blets[i]})
	}
	return out, true
}

// readWindowData 读取窗口密文：优先内存缓存（命中零磁盘零 sha256），未命中读磁盘文件
// + checksum 校验后回填内存（2026-10-25 性能修复）。校验失败删除缓存文件回源。
func (d *stageDL) readWindowData(key string, w *cacheWindow) ([]byte, bool) {
	if d.blobCache != nil {
		if seg, ok := d.blobCache.get(key); ok {
			return seg, true
		}
	}
	seg, rerr := os.ReadFile(d.stageWinPath(key))
	if rerr != nil || int64(len(seg)) != w.EncEnd-w.EncStart {
		return nil, false
	}
	// 写侧 checksum 存在时校验（fail-closed：缓存被篡改 ⇒ 删除回源重新拉取）。
	if w.Checksum != "" && shardseal.VerifyWindowChecksum(
		shardseal.WindowInfo{Index: w.Index, EncOffset: w.EncStart, EncSize: w.EncEnd - w.EncStart, Checksum: w.Checksum},
		seg) != nil {
		_ = os.Remove(d.stageWinPath(key))
		return nil, false
	}
	if d.blobCache != nil {
		d.blobCache.put(key, seg)
	}
	return seg, true
}

// readWindowsReadyOnly 读取 [offset,end) 相交 blocklet 的窗口密文——**只查已就绪缓存**
// （不触发下载、不等待；供整流路径热命中用）。全部窗口已就绪 ⇒ 返回；否则 false。
func (d *stageDL) readWindowsReadyOnly(o chunkRangeOpts, offset, end int64) ([]winBlob, bool) {
	if d == nil {
		return nil, false
	}
	wins := d.planFor(o.e, o.ci)
	if len(wins) == 0 {
		return nil, false
	}
	need, blets := windowsForRange(wins, o.ci.Blocklets, offset, end)
	if len(need) == 0 {
		return nil, true
	}
	out := make([]winBlob, 0, len(need))
	for i, w := range need {
		key := d.stageWinKey(o.e, o.rel, o.ci, w)
		if d.blobCache == nil || !d.windowReady(key, w.EncEnd-w.EncStart) {
			if d.blobCache == nil {
				return nil, false
			}
		}
		seg, ok := d.readWindowData(key, w)
		if !ok {
			return nil, false
		}
		out = append(out, winBlob{Win: w, Data: seg, Blets: blets[i]})
	}
	return out, true
}

// advanceAhead 记录读进度并触发前瞻入队（读水位 + lookAhead 范围内未下载窗口）。
func (d *stageDL) advanceAhead(o chunkRangeOpts, need []*cacheWindow) {
	if d.ahead == nil || len(need) == 0 {
		return
	}
	maxIdx := 0
	for _, w := range need {
		if w.Index > maxIdx {
			maxIdx = w.Index
		}
	}
	d.ahead.setTotalWindows(len(d.planFor(o.e, o.ci)))
	for _, idx := range d.ahead.noteRead(maxIdx) {
		wins := d.planFor(o.e, o.ci)
		if idx < 0 || idx >= len(wins) {
			continue
		}
		w := &wins[idx]
		key := d.stageWinKey(o.e, o.rel, o.ci, w)
		if !d.windowReady(key, w.EncEnd-w.EncStart) {
			d.ensureWindowDownload(o.e, o.rel, o.ci, w)
		}
	}
}

// waitWindowsReady 有界等待目标窗口全部就绪。
// **2026-10-25 修复**：等待窗口由 stageWinReadyWait(2s) 提高到下载一窗的实际耗时上界
// （网盘账号限速 ~8MB/s，4MiB 窗 ≈ 0.5s；8 并发摊薄最坏 ~4s；取 30s 保证窗口下载
// 完成后**不因短超时回源整流**——整流与窗口缓存双轨会重复拉 + 排队（实测 2min 卡死）。
// 读仍不无限阻塞：极端失败（下载失败/无带宽）超时后回源。
func (d *stageDL) waitWindowsReady(e *metaEntry, rel string, ci shardseal.ChunkInfo, need []*cacheWindow) bool {
	// **30s → 3s（2026-10-25 性能修复）**：窗口下载 4MiB @7.8MB/s ≈ 0.5s；30s 硬超时
	// 在下载卡死（inflight 未清）时让读等满 30s 才回源，是热路径 outlier 主因。3s 内
	// 正常下载必完成；仍失败 ⇒ 快速回源整流（读不无限阻塞）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		allReady := true
		for _, w := range need {
			if !d.windowReady(d.stageWinKey(e, rel, ci, w), w.EncEnd-w.EncStart) {
				allReady = false
				break
			}
		}
		if allReady {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ensureWindowDownload 触发窗口下载（未 inflight 才起；已就绪忽略）。
//
// **2026-10-25 实测回归修正**：一度试过「首次触达整文件全部窗口入队」，实测更慢
// （2 分钟只 18 chunk、35s/chunk）：512 个窗口全入队 ⇒ 队尾的「当前读窗口」要等
// 前面几百窗排完。正确做法是**读驱动滑窗**：当前读窗口立即下 + 前瞻只补 lookAhead 个。
func (d *stageDL) ensureWindowDownload(e *metaEntry, rel string, ci shardseal.ChunkInfo, w *cacheWindow) {
	d.startWindowDownload(e, rel, ci, w)
}

// startWindowDownload 触发单窗口下载（未 inflight 才起；已就绪忽略）。
func (d *stageDL) startWindowDownload(e *metaEntry, rel string, ci shardseal.ChunkInfo, w *cacheWindow) {
	key := d.stageWinKey(e, rel, ci, w)
	d.mu.Lock()
	if _, running := d.inflight[key]; running {
		d.mu.Unlock()
		return
	}
	if d.windowReady(key, w.EncEnd-w.EncStart) {
		d.mu.Unlock()
		return
	}
	d.inflight[key] = struct{}{}
	d.mu.Unlock()
	go d.runWindowDownload(e, rel, ci, w, key)
}

// runWindowDownload 窗口下载执行体：sem 排队（阻塞不丢弃）→ 单窗 Range 下载。
func (d *stageDL) runWindowDownload(e *metaEntry, rel string, ci shardseal.ChunkInfo, w *cacheWindow, key string) {
	defer func() {
		d.mu.Lock()
		delete(d.inflight, key)
		d.mu.Unlock()
	}()
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-time.After(5 * time.Minute):
		return
	}
	// **ctx 超时（2026-10-25 性能修复）**：旧 context.Background() 无超时 ⇒ GetRange
	// 遇 dlink 失效/网络静默阻塞时永不返回 ⇒ inflight 永不清 ⇒ 后续读到该窗口一直等
	// waitWindowsReady 30s 硬超时（实测热路径 2 个 31-32s outlier，占总耗时 74%）。
	// 加 60s 上界：卡死自愈（inflight 释放），配合 waitWindowsReady 3s 快速回源。
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = d.downloadWindow(ctx, e, ci, w, key)
}

// downloadWindow 单窗独立下载：底层 OpenRangeRead 拉 [EncStart, EncEnd) 密文 → 原子写
// 缓存文件（tmp+rename）。
func (d *stageDL) downloadWindow(ctx context.Context, e *metaEntry, ci shardseal.ChunkInfo, w *cacheWindow, key string) error {
	rr := syncpkg.AssertRangeReader(d.fs.inner)
	if rr == nil {
		// 底层无 RangeReader：回退旧整块 OpenRead（本地/无直链场景零回归）。
		return d.downloadWholeFallback(ctx, e, ci, key)
	}
	segRel := path.Join(d.fs.dataSeg(e), ci.FileName)
	seg, ok, rerr := syncpkg.RangeReadAll(rr, ctx, segRel, w.EncStart, w.EncEnd-w.EncStart)
	if rerr != nil {
		return rerr
	}
	if !ok {
		return syncpkg.ErrUnsupported
	}
	if int64(len(seg)) != w.EncEnd-w.EncStart {
		return fmt.Errorf("secretdata: 窗口 %s 长度 %d 与规划 %d 不符", key, len(seg), w.EncEnd-w.EncStart)
	}
	dest := d.stageWinPath(key)
	tmp := dest + ".tmp"
	// **2026-10-25 修复**：窗口缓存按逻辑路径组织 ⇒ 子目录（<dir>/<rel 目录>/）需先建
	// （此前未 MkdirAll ⇒ 写 tmp 失败被 runWindowDownload 静默吞掉 ⇒ 窗口缓存 0 文件）。
	if merr := os.MkdirAll(filepath.Dir(dest), 0o700); merr != nil {
		return merr
	}
	if werr := os.WriteFile(tmp, seg, 0o600); werr != nil {
		_ = os.Remove(tmp)
		return werr
	}
	if rerr := os.Rename(tmp, dest); rerr != nil {
		_ = os.Remove(tmp)
		return rerr
	}
	return nil
}

// downloadWholeFallback 底层无 RangeReader 时的整块回退（旧行为，零回归）。
func (d *stageDL) downloadWholeFallback(ctx context.Context, e *metaEntry, ci shardseal.ChunkInfo, key string) error {
	blobRel := path.Join(d.fs.dataSeg(e), ci.FileName)
	rc, err := d.fs.inner.OpenRead(ctx, blobRel)
	if err != nil {
		return err
	}
	defer rc.Close()
	dest := d.stageWinPath(key)
	tmp := dest + ".tmp"
	if merr := os.MkdirAll(filepath.Dir(dest), 0o700); merr != nil {
		return merr
	}
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(f, rc)
	if cerr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return cerr
	}
	if cerr := f.Sync(); cerr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return cerr
	}
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(tmp)
		return cerr
	}
	if rerr := os.Rename(tmp, dest); rerr != nil {
		_ = os.Remove(tmp)
		return rerr
	}
	return nil
}

// invalidate 失效该文件全部块/窗口缓存（写/删后调用）。
func (d *stageDL) invalidate(e *metaEntry) {
	if d == nil || e == nil {
		return
	}
	prefix := d.dirSegPrefix(e)
	d.mu.Lock()
	victims := make([]string, 0, 8)
	for k := range d.inflight {
		if strings.HasPrefix(k, prefix) {
			victims = append(victims, k)
		}
	}
	d.mu.Unlock()
	for _, k := range victims {
		_ = os.Remove(d.stageWinPath(k))
	}
	// 内存窗口密文缓存按 key 前缀失效（与磁盘同纪律）。
	if d.blobCache != nil {
		d.blobCache.invalidatePrefix(prefix)
	}
}

// dirSegPrefix 返回该文件缓存 key 前缀（dirSeg|metaName|）。
func (d *stageDL) dirSegPrefix(e *metaEntry) string {
	return e.dirSeg + "|" + e.metaName + "|"
}

// close 关闭（保留缓存文件，由外层清理纪律管理）。
func (d *stageDL) close() error { return nil }

// _ 编译期断言：stageDL 使用 syncpkg。
var _ = syncpkg.ErrUnsupported
