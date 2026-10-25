// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// blocklet_cache.go —— **块级明文 LRU 缓存** + 整块读取（2026-10-22 缺陷修复）。
//
// 背景（真机实测）：`RangeSeeker` 的**外部/网盘卷窗口**是 256KiB（`externalRangeWindow`，
// 为 seek/首帧优化而设），而 secretdata 的最小可解密单元是 **blocklet**——本地卷 4MiB、
// 外部卷 256KiB，**视频关键帧模式则为整个 GOP（实测 ~10MB；`shardseal` 对超 blocklet
// 上限的 GOP 采用「单段容纳」）**。`readChunkRangeBySegments` 按目标 blocklet 的
// `[salt, 末段末) ` 合并区间取密文 ⇒ **每个 256KiB 窗口都重新拉取覆盖该 blocklet 的整段
// 密文**（GOP ≈ 10MB，再被 `maxRangeSpan` 拆成 3 次 4MiB Range GET）。结果：顺序读 10MB
// 要发 **120 次** Range GET；每次窗口耗时远超客户端超时（30s）⇒ 连接中断 ⇒ 服务端
// `ctx canceled` ⇒ 下载内容缺失/错位（sha256 稳定地错）。
//
// 本缓存把「窗口读」与「网络读」解耦：**同一块只取一次密文、只解一次**，后续窗口（乃至
// seek 回同一块）直接按**明文偏移**切缓存。
//
// 容量：按**字节预算** LRU 淘汰（默认 64MiB，见 `defaultBlockletCacheBytes`）——单块
// 可达 10MB+，按条数限容会让内存不可控。显式负预算 = 关闭（零回归）。
//
// 并发：内部 mutex 串行化；临界区只做 map/LRU 操作，**不持锁做网络 I/O**（miss 路径在
// 查表后释放锁再取密文，回填时再锁）。

import "sync"

// defaultBlockletCacheBytes 是块级明文缓存的默认字节预算（64MiB）。
//
// 取值依据：外部卷视频 GOP 常见 1–10MB，64MiB 约容纳 6–60 个相邻块 ⇒ 覆盖顺序播放的
// 连续窗口与常见 seek 回退；同时保持单卷内存上界可控（多卷并存时每卷一份，装配层可经
// `Options.BlockletCacheBytes` 调低）。
const defaultBlockletCacheBytes = 64 << 20

// blockletCache 是**字节预算**驱动的 LRU 缓存（key → 明文 []byte）。
// LRU 用「每次 get 前置到前端」的切片实现（条目数少：64MiB / 块均 ≥256KiB ⇒ 数十条），
// 与既有 deriveCache 同范式（不引入第三方 LRU 依赖）。
type blockletCache struct {
	mu     sync.Mutex
	items  map[string][]byte
	order  []string // 前端最近使用
	bytes  int64    // 当前占用字节
	budget int64    // 字节预算
}

// newBlockletCache 构造缓存；预算 0 = 默认 64MiB；**显式负值 = 关闭**（返回 nil）。
func newBlockletCache(budget int64) *blockletCache {
	if budget < 0 {
		return nil
	}
	if budget == 0 {
		budget = defaultBlockletCacheBytes
	}
	return &blockletCache{items: map[string][]byte{}, budget: budget}
}

// get 取缓存并触达 LRU 前端。nil 接收者（缓存关闭）恒 miss。
func (c *blockletCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.touchLocked(key)
	return v, true
}

// put 写缓存（超预算时从 LRU 尾端淘汰；**单条超预算直接跳过**——否则会把预算内其它块
// 全部挤掉且自身仍超限）。
func (c *blockletCache) put(key string, plain []byte) {
	if c == nil || int64(len(plain)) > c.budget {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.items[key]; ok {
		c.bytes -= int64(len(old))
		delete(c.items, key)
		c.removeOrderLocked(key)
	}
	for c.bytes+int64(len(plain)) > c.budget && len(c.order) > 0 {
		victim := c.order[len(c.order)-1]
		c.order = c.order[:len(c.order)-1]
		if old, ok := c.items[victim]; ok {
			c.bytes -= int64(len(old))
			delete(c.items, victim)
		}
	}
	c.items[key] = plain
	c.bytes += int64(len(plain))
	c.order = append([]string{key}, c.order...)
}

// touchLocked 把 key 提到 LRU 前端（调用方持锁）。
func (c *blockletCache) touchLocked(key string) {
	for i := range c.order {
		if c.order[i] == key {
			copy(c.order[1:i+1], c.order[:i])
			c.order[0] = key
			return
		}
	}
}

// removeOrderLocked 从 LRU 序列中移除 key（调用方持锁）。
func (c *blockletCache) removeOrderLocked(key string) {
	for i := range c.order {
		if c.order[i] == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// invalidatePrefix 删除所有以 prefix 开头的条目（文件写/删后失效其全部块缓存，防陈旧
// 明文被后续读命中——与 trusted 卷 sidecar 失效同纪律）。
func (c *blockletCache) invalidatePrefix(prefix string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := c.order[:0]
	for _, k := range c.order {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			if old, ok := c.items[k]; ok {
				c.bytes -= int64(len(old))
				delete(c.items, k)
			}
			continue
		}
		kept = append(kept, k)
	}
	c.order = kept
}
