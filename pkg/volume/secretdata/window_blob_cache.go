// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// window_blob_cache.go —— **窗口密文内存 LRU 缓存**（2026-10-25 实测性能修复）。
//
// 背景（真机实测 2.1GB 下载）：RangeSeeker 读窗口 4MiB ⇒ 8MiB chunk = 2 个读窗口；
// 每个读窗口 offset 与写侧窗口边界不对齐 ⇒ need=2 ⇒ **每窗口读 2 个 4MiB 窗口文件 +
// 2 次 sha256 校验**。实测 40s 内 readWindows 被调用 6083 次 ⇒ 读 48GB + sha256 48GB
// ⇒ 每 chunk 稳定 2.1s（全热下载仅 21MB/s，远低于磁盘/CPU 上限）。
//
// 本缓存：窗口密文首次从磁盘读入时校验 checksum（fail-closed），之后同窗口的重复读
// 全内存命中（零磁盘、零 sha256）。预算按字节 LRU 淘汰（默认 256MiB，可容纳 ~64 个
// 4MiB 窗口 ≈ 256MiB 文件顺序读的工作集）。

import (
	"container/list"
	"strings"
	"sync"
)

// defaultWindowBlobCacheBytes 是窗口密文内存缓存默认预算（256MiB）。
const defaultWindowBlobCacheBytes = 256 << 20

// windowBlobCache 是窗口密文的字节预算 LRU 缓存（并发安全，nil 安全）。
type windowBlobCache struct {
	mu     sync.Mutex
	budget int64
	bytes  int64
	items  map[string]*list.Element
	order  *list.List // 头部最近使用
}

// windowBlobEntry 是 LRU 元素（key + 密文）。
type windowBlobEntry struct {
	key  string
	data []byte
}

// newWindowBlobCache 构造缓存；budget ≤ 0 返回 nil（关闭）。budget == 0 视为默认。
func newWindowBlobCache(budget int64) *windowBlobCache {
	if budget < 0 {
		return nil
	}
	if budget == 0 {
		budget = defaultWindowBlobCacheBytes
	}
	return &windowBlobCache{
		budget: budget,
		items:  map[string]*list.Element{},
		order:  list.New(),
	}
}

// get 命中即返回密文（并刷新 LRU）；未命中 nil,false。
func (c *windowBlobCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el) //nolint:errcheck // list 方法无 error 返回
	e, _ := el.Value.(*windowBlobEntry)
	if e == nil {
		return nil, false
	}
	return e.data, true
}

// put 写入密文（超预算按 LRU 淘汰）。单条目超预算 ⇒ 不缓存（避免抖动）。
func (c *windowBlobCache) put(key string, data []byte) {
	if c == nil || int64(len(data)) > c.budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		old, _ := el.Value.(*windowBlobEntry)
		if old == nil {
			return
		}
		c.bytes -= int64(len(old.data))
		old.data = data
		c.bytes += int64(len(data))
		c.order.MoveToFront(el) //nolint:errcheck // list 方法无 error 返回
	} else {
		el := c.order.PushFront(&windowBlobEntry{key: key, data: data}) //nolint:errcheck // list 方法无 error 返回
		c.items[key] = el
		c.bytes += int64(len(data))
	}
	for c.bytes > c.budget && c.order.Len() > 0 {
		back := c.order.Back()
		if back == nil {
			break
		}
		_ = c.order.Remove(back)
		e, _ := back.Value.(*windowBlobEntry)
		if e == nil {
			continue
		}
		delete(c.items, e.key)
		c.bytes -= int64(len(e.data))
	}
}

// invalidatePrefix 删除前缀匹配的条目（写/删文件后失效）。
func (c *windowBlobCache) invalidatePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	var victims []string
	for el := c.order.Front(); el != nil; el = el.Next() {
		e, _ := el.Value.(*windowBlobEntry)
		if e == nil || !strings.HasPrefix(e.key, prefix) {
			continue
		}
		victims = append(victims, e.key)
	}
	for _, k := range victims {
		el, ok := c.items[k]
		if !ok {
			continue
		}
		e, _ := el.Value.(*windowBlobEntry)
		c.order.Remove(el)
		delete(c.items, k)
		if e != nil {
			c.bytes -= int64(len(e.data))
		}
	}
	c.mu.Unlock()
}

// clear 清空全部条目（RefreshIndex 前调用——索引重建使旧 key 失效/复用，窗口密文必须
// 同批清空）。
func (c *windowBlobCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.items = map[string]*list.Element{}
	c.order.Init()
	c.bytes = 0
	c.mu.Unlock()
}
