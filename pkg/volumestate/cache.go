// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

import (
	"container/list"
	"context"
	"log/slog"
	"sync"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// DefaultTTL 是外部卷的默认缓存有效期（C 类结构视图：有界过期即可）。
const DefaultTTL = 30 * time.Second

// DefaultMaxEntries 是内存 LRU 的默认容量（目录快照数）。
const DefaultMaxEntries = 4096

// Options 是 Cache 的构造参数（全部有安全默认：零值可用）。
type Options struct {
	// TTL 按卷名返回缓存有效期：>0 按时间过期；**0 = 不按时间过期**（本地卷：写穿即准）；
	// <0 = 该卷禁用缓存（每次回源，调试/强一致场景）。
	TTL func(volume string) time.Duration
	// MaxEntries 内存 LRU 容量（<=0 ⇒ DefaultMaxEntries）。
	MaxEntries int
	// Store 落盘区（nil 或未启用 ⇒ 仅内存）。
	Store *Store
	// Persist 按卷名返回**是否允许落盘**（保密卷默认 false：只落内存，避免元数据明文落节点磁盘）。
	// nil ⇒ 全部允许（调用方已按卷类型决定是否启用本层）。
	Persist func(volume string) bool
	// Now 时钟（测试注入；nil ⇒ time.Now）。
	Now func() time.Time
	// Log 日志（nil ⇒ slog.Default）。
	Log *slog.Logger
}

// Cache 是卷状态的**唯一缓存实现**（P1 卷维度：键 = 卷名 + FS 相对路径）。
//
// 并发：整体单锁（含 LRU 与落盘路径计算）。命中路径只有一次 map 查找 + 新鲜度判定，
// 无 I/O ⇒ 锁内耗时可控；回源在锁外执行（避免慢远端阻塞其它卷）。
type Cache struct {
	mu       sync.Mutex
	lru      *list.List               // 元素为 *cacheEntry；Front = 最近使用
	items    map[string]*list.Element // key = volume + "\x00" + fsPath
	maxItems int
	opts     Options
	log      *slog.Logger
}

// cacheEntry 是 LRU 元素。
type cacheEntry struct {
	key  string
	vol  string
	path string
	snap *DirSnapshot
}

// New 构造缓存。
func New(opts Options) *Cache {
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = DefaultMaxEntries
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Cache{
		lru:      list.New(),
		items:    make(map[string]*list.Element, opts.MaxEntries),
		maxItems: opts.MaxEntries,
		opts:     opts,
		log:      log,
	}
}

// cacheKey 拼接缓存键（分隔符用 NUL：卷名与路径都不含 NUL ⇒ 无歧义）。
func cacheKey(vol, fsPath string) string { return vol + "\x00" + fsPath }

// ttlFor 返回该卷的 TTL 策略（未注入 ⇒ DefaultTTL）。
func (c *Cache) ttlFor(vol string) time.Duration {
	if c.opts.TTL == nil {
		return DefaultTTL
	}
	return c.opts.TTL(vol)
}

// Get 返回目录的直接子项：命中且新鲜 ⇒ 缓存；否则回源并写回。
//
// probe 是**廉价代际探测**（卷实现 StateGeneration 时由装饰器提供；未实现 ⇒ nil）：
// 命中前比对代际，变化即回源——这是「跨进程/外部改动」的收敛路径。
// fetch 是回源函数（由装饰器给出，通常为 inner.ListDir）。
//
// A 类安全口径：本方法只服务**结构视图（list）**；需要 checksum 做安全判定的调用方
// 必须走「现取 Stat / FileMeta」而不是信任缓存条目（见包文档）。
func (c *Cache) Get(
	ctx context.Context,
	vol, fsPath string,
	probe func(context.Context, string) (uint64, bool),
	fetch func(context.Context) ([]syncpkg.Entry, error),
) ([]syncpkg.Entry, error) {
	fsPath = normalizePath(fsPath)
	if c.ttlFor(vol) < 0 { // 禁用缓存
		return fetch(ctx)
	}
	key := cacheKey(vol, fsPath)
	if snap, ok := c.lookupFresh(ctx, key, vol, fsPath, probe); ok {
		return snap.Entries, nil
	}
	entries, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	var gen uint64
	if probe != nil {
		if g, ok := probe(ctx, fsPath); ok {
			gen = g
		}
	}
	c.store(vol, fsPath, &DirSnapshot{
		Entries:   entries,
		Gen:       gen,
		FetchedAt: c.opts.Now().UnixNano(),
		Volume:    vol,
	})
	return entries, nil
}

// lookupFresh 命中判定：内存 → 磁盘 → TTL → 代际。
func (c *Cache) lookupFresh(
	ctx context.Context, key, vol, fsPath string,
	probe func(context.Context, string) (uint64, bool),
) (*DirSnapshot, bool) {
	snap, elem := c.lookupOrLoadDisk(key, vol, fsPath)
	if snap == nil {
		return nil, false
	}
	if !c.freshByTTL(vol, snap) || !c.freshByGen(ctx, fsPath, snap, probe) {
		c.drop(key, elem)
		return nil, false
	}
	c.moveFront(elem)
	return snap, true
}

// lookupOrLoadDisk 内存命中；否则尝试磁盘快照（重启后免全量远端往返）。
func (c *Cache) lookupOrLoadDisk(key, vol, fsPath string) (*DirSnapshot, *list.Element) {
	if snap, elem := c.lookup(key); snap != nil {
		return snap, elem
	}
	if c.opts.Store == nil || !c.persistAllowed(vol) {
		return nil, nil
	}
	d, ok := c.opts.Store.Load(vol, fsPath)
	if !ok || d.Volume != vol {
		return nil, nil
	}
	return d, c.pushFront(key, vol, fsPath, d)
}

// freshByTTL 判定时间新鲜度（TTL<=0 ⇒ 不按时间过期）。
func (c *Cache) freshByTTL(vol string, snap *DirSnapshot) bool {
	ttl := c.ttlFor(vol)
	if ttl <= 0 {
		return true
	}
	return c.opts.Now().UnixNano()-snap.FetchedAt <= int64(ttl)
}

// freshByGen 判定代际新鲜度：探测失败或不一致 ⇒ 不新鲜（回源取真值，绝不乐观复用）。
func (c *Cache) freshByGen(ctx context.Context, fsPath string, snap *DirSnapshot, probe func(context.Context, string) (uint64, bool)) bool {
	if probe == nil {
		return true
	}
	gen, ok := probe(ctx, fsPath)
	return ok && gen == snap.Gen
}

// lookup 取内存快照（不判定新鲜度）。
func (c *Cache) lookup(key string) (*DirSnapshot, *list.Element) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return nil, nil
	}
	ce, _ := elem.Value.(*cacheEntry)
	if ce == nil {
		return nil, nil
	}
	return ce.snap, elem
}

// store 写回内存 LRU + 落盘（best-effort）。
func (c *Cache) store(vol, fsPath string, snap *DirSnapshot) {
	key := cacheKey(vol, fsPath)
	c.mu.Lock()
	if elem, ok := c.items[key]; ok {
		if ce, _ := elem.Value.(*cacheEntry); ce != nil {
			ce.snap = snap
		}
		c.lru.MoveToFront(elem)
	} else {
		c.items[key] = c.lru.PushFront(&cacheEntry{key: key, vol: vol, path: fsPath, snap: snap})
	}
	c.evictLocked()
	c.mu.Unlock()
	if c.opts.Store != nil && c.persistAllowed(vol) {
		c.opts.Store.Save(vol, fsPath, snap)
	}
}

// persistAllowed 报告该卷是否允许落盘（保密卷默认不允许）。
func (c *Cache) persistAllowed(vol string) bool {
	if c.opts.Persist == nil {
		return true
	}
	return c.opts.Persist(vol)
}

// pushFront 插入并返回元素（lookupFresh 从磁盘载入时用）。
func (c *Cache) pushFront(key, vol, fsPath string, snap *DirSnapshot) *list.Element {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		if ce, _ := elem.Value.(*cacheEntry); ce != nil {
			ce.snap = snap
		}
		c.lru.MoveToFront(elem)
		return elem
	}
	elem := c.lru.PushFront(&cacheEntry{key: key, vol: vol, path: fsPath, snap: snap})
	c.items[key] = elem
	c.evictLocked()
	return elem
}

// evictLocked 超容淘汰（调用方持锁）。
func (c *Cache) evictLocked() {
	for c.lru.Len() > c.maxItems {
		back := c.lru.Back()
		if back == nil {
			return
		}
		c.lru.Remove(back)
		if ce, _ := back.Value.(*cacheEntry); ce != nil {
			delete(c.items, ce.key)
		}
	}
}

// moveFront 提升热度（可能为 nil：磁盘载入路径已 pushFront）。
func (c *Cache) moveFront(elem *list.Element) {
	if elem == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lru.MoveToFront(elem)
}

// drop 删除某键（内存 + 落盘）。
func (c *Cache) drop(key string, elem *list.Element) {
	var vol, p string
	c.mu.Lock()
	if elem == nil {
		elem = c.items[key]
	}
	if elem != nil {
		if ce, _ := elem.Value.(*cacheEntry); ce != nil {
			vol, p = ce.vol, ce.path
		}
		c.lru.Remove(elem)
		delete(c.items, key)
	}
	c.mu.Unlock()
	if c.opts.Store != nil && vol != "" {
		c.opts.Store.Remove(vol, p)
	}
}

// Invalidate 是**唯一失效入口**（P3：由装饰器在写成功后自动调用，调用方无需登记）。
//
// 语义：删除该 ref 的目录快照 + 把全部祖先目录标记 stale（删除快照 ⇒ 下次读回源）。
// 内存失效不可能失败（同进程）；磁盘文件删除为 best-effort（陈旧快照另有 TTL/代际兜底）。
func (c *Cache) Invalidate(ref ObjectRef) {
	if ref.Volume == "" {
		return
	}
	p := normalizePath(ref.Path)
	for _, a := range ancestorPaths(p) {
		c.drop(cacheKey(ref.Volume, a), nil)
	}
}

// Refresh 丢弃该卷的全部缓存（显式重建入口，对应 POST /api/volumes/refresh）。
func (c *Cache) Refresh(_ context.Context, vol string) error {
	c.mu.Lock()
	var dropped []string
	for key, elem := range c.items {
		ce, _ := elem.Value.(*cacheEntry)
		if ce != nil && ce.vol == vol {
			dropped = append(dropped, ce.path)
			c.lru.Remove(elem)
			delete(c.items, key)
		}
	}
	c.mu.Unlock()
	if c.opts.Store != nil {
		c.opts.Store.RemoveVolume(vol)
	}
	if len(dropped) > 0 {
		c.log.Debug("卷状态缓存已清空", "volume", vol, "dirs", len(dropped))
	}
	return nil
}

// Len 返回当前内存快照数（测试/指标用）。
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lru.Len()
}
