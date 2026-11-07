// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// dlink_cache.go —— **有界 LRU 直链缓存**（2026-10-25）。
//
// 背景：百度 dlink 自签有效 8h，旧实现本地缓存 TTL 60s + 裸 map（无淘汰）——
// ① 60s 太短：同一文件的多次 Range GET 在 >60s 的窗口（如大块持续下载）会**使用中
// 过期** ⇒ 已签发直链作废重签（每次 ~0.3s 且重建连接）；② 裸 map 无界：TTL 拉长到
// 小时级后条目会无限增长（数万 blob ⇒ 内存与陈旧条目堆积）。
//
// 本实现：TTL 提到 **1h**（8h 签名的 1/8，失效先于签名到期，安全）+ **有界 LRU**
// （容量上限淘汰最久未用）。CDN 非 206（签名失效/被踢）仍自愈重签。

import (
	"container/list"
	"sync"
	"time"
)

// dlinkLRUCapacity 是直链缓存容量上限（数千 blob 文件的工作集；超出淘汰最久未用，
// 下次用重新签发）。
const dlinkLRUCapacity = 4096

// dlinkEntry 是直链缓存值（key + url + 过期时间）。
type dlinkEntry struct {
	key string
	url string
	exp time.Time
}

// dlinkLRU 是有界 LRU 直链缓存（并发安全）。
type dlinkLRU struct {
	mu  sync.Mutex
	cap int
	idx map[string]*list.Element
	ord *list.List // 头部最近使用
}

// newDlinkLRU 构造 LRU；cap ≤ 0 用默认。
func newDlinkLRU(capacity int) *dlinkLRU {
	if capacity <= 0 {
		capacity = dlinkLRUCapacity
	}
	return &dlinkLRU{cap: capacity, idx: map[string]*list.Element{}, ord: list.New()}
}

// get 取直链（命中刷新 LRU 位次）；未命中/已过期返回 false（调用方重签）。
func (c *dlinkLRU) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.idx[key]
	if !ok {
		return "", false
	}
	e, _ := el.Value.(*dlinkEntry)
	if e == nil || time.Now().After(e.exp) {
		_ = c.ord.Remove(el)
		delete(c.idx, key)
		return "", false
	}
	c.ord.MoveToFront(el)
	return e.url, true
}

// put 写直链（超容量淘汰最久未用；同 key 更新并刷新位次）。
func (c *dlinkLRU) put(key, url string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		if e, _ := el.Value.(*dlinkEntry); e != nil {
			e.url = url
			e.exp = time.Now().Add(ttl)
		}
		c.ord.MoveToFront(el)
		return
	}
	c.idx[key] = c.ord.PushFront(&dlinkEntry{key: key, url: url, exp: time.Now().Add(ttl)})
	for len(c.idx) > c.cap && c.ord.Len() > 0 {
		back := c.ord.Back()
		if back == nil {
			break
		}
		_ = c.ord.Remove(back)
		if e, _ := back.Value.(*dlinkEntry); e != nil {
			delete(c.idx, e.key)
		}
	}
}

// delete 删除 key（直链失效/重签前清旧）。
func (c *dlinkLRU) delete(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		_ = c.ord.Remove(el)
		delete(c.idx, key)
	}
}

// len 返回当前条目数（测试/观测用）。
func (c *dlinkLRU) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.idx)
}
