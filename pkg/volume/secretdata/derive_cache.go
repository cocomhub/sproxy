// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import "sync"

// deriveCache 是派生密钥小容量 LRU 缓存（key = meta blob 内嵌 salt 的 hex → 派生 key）。
// 目的（Imp-2）：并行 loadIndex 按容器并行扫描 + 容器内文件 meta 并行解密时，同 salt 的
// 多个 meta（去重克隆共享 meta.Salt）只派生一次 scrypt；同一 FS 实例 secret/algoVer 固定，
// 故 cache key 只需 salt 即可。容量小（默认 64）控制内存，命中失败剔除（不误用旧 key）。
type deriveCache struct {
	mu    sync.Mutex
	items map[string][]byte
	order []string // 前端最近使用
	cap   int
}

// newDeriveCache 构造派生缓存；cap<=0 时用默认 64。
func newDeriveCache(cap int) *deriveCache {
	if cap <= 0 {
		cap = 64
	}
	return &deriveCache{items: map[string][]byte{}, cap: cap}
}

// get 取缓存 key 并触达 LRU 前端。
func (c *deriveCache) get(saltHex string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key, ok := c.items[saltHex]
	if !ok {
		return nil, false
	}
	for i := range c.order {
		if c.order[i] == saltHex {
			copy(c.order[1:i+1], c.order[:i])
			c.order[0] = saltHex
			break
		}
	}
	return key, true
}

// put 写缓存（已存在则刷新值并提前）。
func (c *deriveCache) put(saltHex string, key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[saltHex]; ok {
		c.items[saltHex] = key
		return
	}
	if len(c.order) >= c.cap {
		victim := c.order[len(c.order)-1]
		delete(c.items, victim)
		c.order = c.order[:len(c.order)-1]
	}
	c.items[saltHex] = key
	c.order = append([]string{saltHex}, c.order...)
}

// delete 剔除缓存项（缓存键与 blob 不匹配时清理，防误用旧 key）。
func (c *deriveCache) delete(saltHex string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[saltHex]; !ok {
		return
	}
	delete(c.items, saltHex)
	for i := range c.order {
		if c.order[i] == saltHex {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}
