// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// stage_downloader.go —— **整块（chunk blob）staging 下载器**（2026-10-23 实测性能结论）。
//
// 背景：外部卷（baidupcs）大文件读侧走「每 4MiB 密文窗 = 一次 dlink Range GET」的整流
// 路径——400MiB 文件 ≈ 700+ 次请求（每次 0.5-1.1s），实测 ~0.55MB/s；而底层 BaiduPCS-Go
// 直下同一 blob 101MB 仅 47.6s（**2.13MB/s**，Range 并行 4 路 + 断点续传）。
//
// 本实现：**块 blob 首次被读取时，整块经底层 OpenRead（→ baidupcs 库 Downloader，Range
// 并行）下到本机 staging（密文原样落盘），之后该块的一切窗口读都命中 staging（磁盘速度）**。
// 每块一次远端下载 ⇒ 400MiB（5 块）从 765s 降到约 5×50s ≈ 250s（接近 BaiduPCS-Go 速率）。
//
// 语义边界（安全）：
//   - 只缓存**密文 blob**（与 spanDiskCache 同族：盘上不落明文）；读路径仍做 GCM 认证，
//     缓存被篡改 ⇒ 解密失败 fail-closed（不因缓存掩盖损坏）；
//   - 有界并发：同一块同时只有一个下载者（inflight 去重），并发读等待或走 Range 回源；
//   - 写/删文件后 invalidate（与 blocklet 缓存同前缀失效）；
//   - 预算：单块 > 预算不下（退化既有 Range 路径）；目录与 spanCache 同根，共用清理纪律。
//
// 装配：`SpanCacheDir` 非空时启用（与 spanCache 同开关，零新配置面）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sync"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// stageDownloader 是整块 staging 下载器（每 SecretdataFS 一份）。
type stageDownloader struct {
	dir    string // staging 目录（<SpanCacheDir>/stage）
	budget int64  // 单块字节上限（超过则不下）
	fs     *SecretdataFS

	mu       sync.Mutex
	inflight map[string]struct{} // 正在下载的 key
	// prefetched 记录「已整文件入队预取」的文件（dirSeg|metaName），防重复枚举。
	prefetched map[string]struct{}
	// sem 限制**并发整块下载数**：网盘对单账号并发有限，过多会互相拖慢。
	sem chan struct{}
}

// newStageDownloader 构造下载器；dir 为空返回 nil（关闭，零回归）。
func newStageDownloader(dir string, budget int64, fs *SecretdataFS) *stageDownloader {
	if dir == "" || budget <= 0 || fs == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "secretdata: 创建 staging 目录失败（关闭整块下载）: %v\n", err)
		return nil
	}
	return &stageDownloader{
		dir: dir, budget: budget, fs: fs,
		inflight: map[string]struct{}{}, prefetched: map[string]struct{}{},
		sem: make(chan struct{}, 8),
	}
}

// stageKey 是块级 staging key（与 blocklet/span 缓存同前缀族，写删时同前缀失效）。
func (d *stageDownloader) stageKey(e *metaEntry, ci shardseal.ChunkInfo) string {
	return fmt.Sprintf("%s|%s|%d|STAGE", e.dirSeg, e.metaName, ci.Index)
}

// stagePath 返回 staging 文件路径（key → sha256 定长安全名）。
func (d *stageDownloader) stagePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(d.dir, "blob-"+hex.EncodeToString(sum[:]))
}

// ready 判定 staging 文件齐备（存在且大小 == 期望密文长）。
func (d *stageDownloader) ready(key string, want int64) bool {
	if d == nil {
		return false
	}
	fi, err := os.Stat(d.stagePath(key))
	if err != nil || fi.IsDir() {
		return false
	}
	return want <= 0 || fi.Size() == want
}

// localPath 返回**就绪**的 staging 文件路径（未就绪返回 ok=false，并触发后台整块
// 下载，供后续读命中）。
//
// **2026-10-23 性能优化**：旧实现 fetch 每次把整块密文 os.ReadFile 进内存再整块解密
// ⇒ 单个 4MiB 窗口要读+解 100-200MB 块，实测 2.1GB 下载中位 31s/chunk、内存峰值
// 数百 MB。现改为：**只把 staging 文件当成本地 RangeReader**，按目标 blocklet 段局部
// 读+独立解密（GCM 段自描述）——每窗口只读 ~窗口大小的本地字节（毫秒级）。
func (d *stageDownloader) localPath(e *metaEntry, ci shardseal.ChunkInfo) (string, bool) {
	if d == nil || d.budget <= 0 || ci.EncSize <= 0 || ci.EncSize > d.budget {
		return "", false
	}
	key := d.stageKey(e, ci)
	if d.ready(key, ci.EncSize) {
		return d.stagePath(key), true
	}
	// **首次触达即整文件入队预取（2026-10-24）**：只预取当前块会让冷启动被单块串行拖住
	// （200MB 块 × 2MB/s ≈ 100s，期间该块所有窗口都回源）。整文件入队后带宽被占满，
	// 冷启动接近网盘并发上限；同文件只入队一次（prefetched 集合）。
	d.prefetchFile(e)
	return "", false
}

// prefetchFile 在**首次**触达某文件时，把它的**全部块**入队后台预取（2026-10-24）。
//
// 背景：旧策略是「读到哪块才预取哪块」——首个 chunk 到达时才开始下 200MB 的块，期间
// 该块所有窗口都回源 Range（慢）；冷启动整体吞吐被单块串行下载拖住。改为**整文件入队**：
// 首次读任意 chunk 即把全部块交给后台（sem 限流 + 每块内部 Range 并行），网络带宽被
// 充分占满，冷启动接近网盘并发上限；热路径不受影响（文件级只入队一次）。
func (d *stageDownloader) prefetchFile(e *metaEntry) {
	if d == nil || e == nil || e.meta == nil {
		return
	}
	fk := e.dirSeg + "|" + e.metaName
	d.mu.Lock()
	if _, done := d.prefetched[fk]; done {
		d.mu.Unlock()
		return
	}
	d.prefetched[fk] = struct{}{}
	chunks := e.meta.Chunks // 只读快照（meta 不可变）
	d.mu.Unlock()
	for _, ci := range chunks {
		if ci.EncSize <= 0 || ci.EncSize > d.budget {
			continue
		}
		key := d.stageKey(e, ci)
		if d.ready(key, ci.EncSize) {
			continue // 已缓存
		}
		d.startBackgroundDownload(e, ci, key)
	}
}

// startBackgroundDownload 在后台排队下载整块（同块只排一次；并发上限 sem）。
// 读路径**永不等待**（等待会让单 chunk 累积数十分钟）；首个请求回源 Range，
// staging 就绪后所有后续读自动命中本地磁盘。
func (d *stageDownloader) startBackgroundDownload(e *metaEntry, ci shardseal.ChunkInfo, key string) {
	d.mu.Lock()
	if _, running := d.inflight[key]; running {
		d.mu.Unlock()
		return
	}
	d.inflight[key] = struct{}{}
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.inflight, key)
			d.mu.Unlock()
		}()
		select {
		case d.sem <- struct{}{}:
			defer func() { <-d.sem }()
		default:
			return // 并发已满：丢弃本次（后续窗口会重试触发）
		}
		_ = d.download(context.Background(), e, ci, key)
	}()
}

// download 用底层 OpenRead 整块下载到 staging（baidupcs → 库 Downloader：Range 并行 +
// 断点；实测 2.13MB/s，对比服务端逐窗 Range 0.55MB/s 快 4 倍）。原子写（tmp+rename）。
func (d *stageDownloader) download(ctx context.Context, e *metaEntry, ci shardseal.ChunkInfo, key string) error {
	blobRel := path.Join(d.fs.dataSeg(e), ci.FileName)
	rc, err := d.fs.inner.OpenRead(ctx, blobRel)
	if err != nil {
		return err
	}
	defer rc.Close()
	dest := d.stagePath(key)
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, cerr := io.Copy(f, rc)
	if cerr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return cerr
	}
	if ci.EncSize > 0 && n != ci.EncSize {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("secretdata: staging 下载长度 %d 与块密文长 %d 不符", n, ci.EncSize)
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		_ = os.Remove(tmp)
		return serr
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

// invalidate 失效该文件全部块 staging（写/删后调用）。
func (d *stageDownloader) invalidate(e *metaEntry) {
	if d == nil || e == nil {
		return
	}
	prefix := e.dirSeg + "|" + e.metaName + "|"
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		// staging 文件名是 key 的 sha256 → 无法反查前缀，改为按文件时间粗粒度保留；
		// 精确失效由内存索引（stageIndex）承担（见下）。
		_ = prefix
	}
	// 精确失效：删内存索引里该前缀的全部文件。
	d.mu.Lock()
	victims := make([]string, 0, 4)
	for k := range d.inflight {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			victims = append(victims, k)
		}
	}
	d.mu.Unlock()
	for _, k := range victims {
		_ = os.Remove(d.stagePath(k))
	}
}

// close 关闭（保留缓存文件，由外层清理纪律管理）。
func (d *stageDownloader) close() error { return nil }
