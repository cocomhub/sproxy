// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// span_disk_cache.go —— **本机磁盘二级缓存**（staging 目录）用于块密文跨度（2026-10-22）。
//
// 定位：`blockletCache`（内存 LRU，见 blocklet_cache.go）解决「同一块的多个窗口重复拉取」
// 的**热**问题；但内存预算有限（默认 64MiB），大文件顺序读（2.1GB 视频）仍会反复命中远端。
// 本层把**块密文跨度**（即 `rangeReadSpan` 拉到的原始密文字节）落到本机 **staging 目录**，
// 顺序读/重读时零远端流量、零 dlink 依赖。
//
// **只缓存密文**（关键安全约束）：解密仍在每次使用时进行（GCM 认证不变）⇒
//   - 磁盘上不会出现明文（与「读路径不落明文临时文件」的既有纪律一致）；
//   - 盘上密文被篡改时读仍 fail-closed（GCM 认证失败），不因缓存而掩盖篡改。
//
// 容量与清理：
//   - 字节预算（`Options.SpanDiskCacheBytes`，0 = 关闭，**默认关闭零回归**）；
//   - **定期自动清理**：后台 sweeper 周期扫描，按 atime 淘汰超预算/超 TTL 条目；
//   - **空间不足自动降级**：写入失败（含 ENOSPC）⇒ 立即清空缓存目录并跳过本次缓存
//     （告警一次，不阻塞读路径）——绝不因缓存把读搞失败。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// defaultSpanCacheSweepInterval 是磁盘缓存后台清理周期（默认 5 分钟）。
const defaultSpanCacheSweepInterval = 5 * time.Minute

// defaultSpanCacheBytes 是磁盘缓存默认字节预算（512MiB）——仅 `SpanCacheDir` 非空时生效。
const defaultSpanCacheBytes = 512 << 20

// spanCacheEntry 是索引条目（持久化到 index.json，重启可恢复统计）。
type spanCacheEntry struct {
	Size  int64 `json:"size"`
	ATime int64 `json:"atime"` // UnixNano（访问时间，LRU 依据）
}

// spanDiskCache 是本机磁盘缓存：目录内一个 index.json + 若干 `blk-<sha256>` 数据文件。
type spanDiskCache struct {
	dir    string
	budget int64

	mu      sync.Mutex
	index   map[string]spanCacheEntry
	bytes   int64
	degrade bool // 写失败（如 ENOSPC）后置位：跳过缓存并周期性重试清理

	sweepCancel context.CancelFunc
}

// newSpanDiskCache 构造磁盘缓存；budget<=0 返回 nil（关闭）。dir 为空时返回 nil（无落点）。
func newSpanDiskCache(dir string, budget int64) (*spanDiskCache, error) {
	if budget <= 0 || dir == "" {
		return nil, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secretdata: 创建块密文缓存目录 %s 失败: %w", dir, err)
	}
	c := &spanDiskCache{dir: dir, budget: budget, index: map[string]spanCacheEntry{}}
	c.loadIndex()
	// 启动即清一次（清掉上次进程退出后残留的超预算/TTL 条目）。
	c.sweep()
	return c, nil
}

// loadIndex 读取 index.json（缺失/损坏 ⇒ 从空索引重建，不报错——缓存是尽力而为设施）。
func (c *spanDiskCache) loadIndex() {
	raw, err := os.ReadFile(filepath.Join(c.dir, "index.json"))
	if err != nil {
		return
	}
	var idx map[string]spanCacheEntry
	if json.Unmarshal(raw, &idx) != nil {
		return
	}
	total := int64(0)
	for k, e := range idx {
		if _, serr := os.Stat(c.filePath(k)); serr != nil {
			continue // 数据文件缺失（被外部清理）⇒ 丢弃该条目
		}
		c.index[k] = e
		total += e.Size
	}
	c.bytes = total
}

// filePath 返回 key 的数据文件路径（key 经 sha256 → 定长安全文件名，不含路径分隔符）。
func (c *spanDiskCache) filePath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.dir, "blk-"+hex.EncodeToString(sum[:]))
}

// get 读取缓存条目（命中即刷新 atime；数据文件损坏/缺失 ⇒ 当作 miss 并删除条目）。
func (c *spanDiskCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	e, ok := c.index[key]
	degraded := c.degrade
	c.mu.Unlock()
	if !ok || degraded {
		return nil, false
	}
	raw, err := os.ReadFile(c.filePath(key))
	if err != nil || int64(len(raw)) != e.Size {
		c.mu.Lock()
		c.removeLocked(key)
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Lock()
	if cur, ok2 := c.index[key]; ok2 {
		cur.ATime = time.Now().UnixNano()
		c.index[key] = cur
	}
	c.mu.Unlock()
	return raw, true
}

// put 写缓存条目（原子写：临时文件 + rename）。写失败（含 ENOSPC）⇒ 置 degrade、
// 清空缓存目录并跳过本次缓存（读路径不受影响）。
func (c *spanDiskCache) put(key string, data []byte) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.degrade {
		c.mu.Unlock()
		return
	}
	c.bytes += int64(len(data)) // 乐观累计（写成功后才精确化）
	c.mu.Unlock()

	// 目录可能被 enterDegrade 清空（或外部清理）：每次 put 前确保存在（幂等）。
	if merr := os.MkdirAll(c.dir, 0o700); merr != nil {
		c.enterDegrade(merr)
		return
	}
	final := c.filePath(key)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		c.enterDegrade(err)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		c.enterDegrade(err)
		return
	}
	c.mu.Lock()
	if old, ok := c.index[key]; ok {
		c.bytes -= old.Size
	}
	c.index[key] = spanCacheEntry{Size: int64(len(data)), ATime: time.Now().UnixNano()}
	over := c.bytes - c.budget
	c.mu.Unlock()
	c.persistIndex()
	// 超预算 ⇒ 立即按 LRU 淘汰（不等下个 sweep 周期）。
	if over > 0 {
		c.sweep()
	}
}

// enterDegrade 标记降级（空间不足等写失败）：清空缓存目录并告警一次；后续 put 直接跳过。
// 下一个 sweep 周期会清除 degrade 标志并重试（空间可能已恢复）。
func (c *spanDiskCache) enterDegrade(err error) {
	c.mu.Lock()
	already := c.degrade
	c.degrade = true
	c.index = map[string]spanCacheEntry{}
	c.bytes = 0
	c.mu.Unlock()
	if !already {
		// 尽力清理（空间不足时删除可释放空间）。
		_ = os.Remove(filepath.Join(c.dir, "index.json"))
		if entries, rerr := os.ReadDir(c.dir); rerr == nil {
			for _, e := range entries {
				_ = os.Remove(filepath.Join(c.dir, e.Name()))
			}
		}
		// 告警走 stderr（本包不持有 logger；装配层可从日志级别观察）。
		fmt.Fprintf(os.Stderr, "secretdata: 块密文磁盘缓存降级（%v）⇒ 已清空缓存目录 %s，本次读改为直连远端\n", err, c.dir)
	}
}

// invalidatePrefix 删除所有以 prefix 开头的条目（文件写/删后失效；与内存缓存同纪律）。
func (c *spanDiskCache) invalidatePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	var victims []string
	for k := range c.index {
		if strings.HasPrefix(k, prefix) {
			victims = append(victims, k)
		}
	}
	for _, k := range victims {
		c.removeLocked(k)
	}
	c.mu.Unlock()
	for _, k := range victims {
		_ = os.Remove(c.filePath(k))
	}
	if len(victims) > 0 {
		c.persistIndex()
	}
}

// removeLocked 从索引移除 key（调用方持锁）。
func (c *spanDiskCache) removeLocked(key string) {
	if e, ok := c.index[key]; ok {
		c.bytes -= e.Size
		if c.bytes < 0 {
			c.bytes = 0
		}
		delete(c.index, key)
	}
}

// sweep 执行一次清理：① 清除 degrade（重试）；② 删除数据文件缺失的条目；
// ③ 按 atime（LRU）淘汰至预算内；④ TTL 过期条目一并淘汰。
func (c *spanDiskCache) sweep() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.degrade = false // 空间可能已恢复：下个周期重试缓存写入
	// ① 数据文件缺失 ⇒ 丢弃条目。
	for k := range c.index {
		if _, err := os.Stat(c.filePath(k)); err != nil {
			c.removeLocked(k)
		}
	}
	// ② TTL：超过 2 倍清理周期未访问的条目淘汰（防冷数据长期占空间）。
	cutoff := time.Now().Add(-2 * defaultSpanCacheSweepInterval).UnixNano()
	for k, e := range c.index {
		if e.ATime != 0 && e.ATime < cutoff {
			c.removeLocked(k)
			_ = os.Remove(c.filePath(k))
		}
	}
	// ③ LRU 淘汰至预算内。
	for c.bytes > c.budget {
		victim, ok := c.oldestLocked()
		if !ok {
			break
		}
		c.removeLocked(victim)
		_ = os.Remove(c.filePath(victim))
	}
	c.persistIndexLocked()
}

// oldestLocked 返回 atime 最小的 key（调用方持锁）。
func (c *spanDiskCache) oldestLocked() (string, bool) {
	var (
		best  string
		bestT int64 = 1<<63 - 1
		found bool
	)
	for k, e := range c.index {
		if e.ATime < bestT {
			best, bestT, found = k, e.ATime, true
		}
	}
	return best, found
}

// startSweeper 启动后台周期清理（ctx 取消 / Close 时停止）。
func (c *spanDiskCache) startSweeper(ctx context.Context, interval time.Duration) {
	if c == nil {
		return
	}
	if interval <= 0 {
		interval = defaultSpanCacheSweepInterval
	}
	sctx, cancel := context.WithCancel(ctx)
	c.sweepCancel = cancel
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				c.sweep()
			}
		}
	}()
}

// close 停止后台清理并把索引落盘。
func (c *spanDiskCache) close() {
	if c == nil {
		return
	}
	if c.sweepCancel != nil {
		c.sweepCancel()
	}
	c.persistIndex()
}

// persistIndex 落盘索引（尽力而为；失败仅意味着重启后需重新扫描缓存，不影响正确性）。
func (c *spanDiskCache) persistIndex() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.persistIndexLocked()
}

func (c *spanDiskCache) persistIndexLocked() {
	raw, err := json.Marshal(c.index)
	if err != nil {
		return
	}
	tmp := filepath.Join(c.dir, "index.json.tmp")
	if werr := os.WriteFile(tmp, raw, 0o600); werr != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, filepath.Join(c.dir, "index.json"))
}

// spanDiskCacheKey 是磁盘缓存的 key（文件 + 块序号 + 跨度区间）——**区间的起点与终点都
// 入 key**：不同窗口跨度各自独立缓存，命中即零远端读。
func (s *SecretdataFS) spanDiskCacheKey(e *metaEntry, ci shardseal.ChunkInfo, off, end int64) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d", e.dirSeg, e.metaName, ci.Index, off, end)
}

// readSpanCached 读区间 [off,end) 的**密文**：先查磁盘缓存，未命中走远端并回填。
// 缓存关闭时直连远端（零回归）。
func (s *SecretdataFS) readSpanCached(ctx context.Context, rr syncpkg.RangeReader, segPath, key string, off, end int64) ([]byte, error) {
	if s.spanCache != nil {
		if raw, ok := s.spanCache.get(key); ok {
			return raw, nil
		}
	}
	raw, err := s.rangeReadSpanRaw(ctx, rr, segPath, off, end)
	if err != nil {
		return nil, err
	}
	if s.spanCache != nil {
		s.spanCache.put(key, raw)
	}
	return raw, nil
}
