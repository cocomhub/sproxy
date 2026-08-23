// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sproxysig

import (
	"sync"
	"time"
)

// NoncePool 是内存 nonce 去重池（请求窗口内一次性，防重放）。
// 条目按 (ak, nonce) 去重，记录该请求的过期时间 exp；过期条目在访问时惰性清理。
// 进程内实现（单实例服务端）；多实例需换共享存储（后续扩展）。
type NoncePool struct {
	mu sync.Mutex
	m  map[string]int64 // "ak\x00nonce" → exp ms
}

// NewNoncePool 创建 nonce 池。
func NewNoncePool() *NoncePool {
	return &NoncePool{m: make(map[string]int64)}
}

// Seen 报告 (ak, nonce) 是否已在未过期窗口内使用过；未使用则记录。
func (p *NoncePool) Seen(ak, nonce string, expMs int64) bool {
	if ak == "" || nonce == "" {
		return true // fail-closed：空值视为重放
	}
	key := ak + "\x00" + nonce
	now := time.Now().UnixMilli()
	p.mu.Lock()
	defer p.mu.Unlock()
	if exp, ok := p.m[key]; ok && exp > now {
		return true // 未过期即重放
	}
	// 记录本次；顺带清理过期条目（惰性，防 map 无限增长）
	p.m[key] = expMs
	for k, e := range p.m {
		if e <= now {
			delete(p.m, k)
		}
	}
	return false
}
