// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_index_concurrency_test.go 钉住「索引 map 并发读写窗口」（审查 P1）：
// 写路径（upsert/remove）持锁**原地修改** entries，读路径（searchLocked/list）无锁遍历
// 同一 map —— Go map 并发写/读写 = runtime fatal（进程崩溃），非 panic 可恢复。
//
// 本用例并发跑 upsert（写）+ search（读），-race 下必现 map concurrent write / concurrent
// map read and map write；修复后（copy-on-write：写路径拷贝 entries → 改 → 替换指针）
// 并发安全无 fatal。
//
// 变异验证：把写路径改回「原地修改 entries 不替换指针」→ 本用例 -race 红。

import (
	"sync"
	"testing"
)

// newConcurrencyIndex 构造一个已构建的最小索引容器（entries 直接注入，跳过磁盘依赖）。
// 用新 searchIndex 的 ensureOwner 全量构建太慢（依赖真实 root）；直接用零值 Service 的
// index 字段注入测试：newSearchIndex(nil, nil, nil, nil) + 手动塞 ownerIndex。
func newConcurrencyIndex(owner string, entries map[string]*indexEntry) *searchIndex {
	ix := newSearchIndex(nil, nil, nil, nil, false)
	ix.mu.Lock()
	ix.owners[owner] = &ownerIndex{entries: entries}
	ix.built[owner] = true
	ix.mu.Unlock()
	return ix
}

// TestSearchIndex_ConcurrentUpsertSearch_NoFatal 钉住并发写读安全：
// 并发 goroutine 交替 upsert（写）+ searchLocked（读），-race 下修复前必 fatal。
func TestSearchIndex_ConcurrentUpsertSearch_NoFatal(t *testing.T) {
	// 并行化：不依赖 t.Setenv/全局可变状态。
	t.Parallel()

	const owner = "alice"
	entries := map[string]*indexEntry{
		"seed.txt": {name: "seed.txt", base: "seed.txt", size: 1},
	}
	ix := newConcurrencyIndex(owner, entries)
	// searchLocked 依赖 ensureOwner → tenant0 回调；直接调 searchLocked 会经
	// ensureOwner（built=true 直接返回）。tenant0 为 nil 时 ensureOwner 只走 built 分支。
	// 但 searchLocked 需要 csMap（空即可）。

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 写 goroutine：持续 upsert（每次不同 rel）。
	wg.Go(func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			ix.upsert(owner, "f"+string(rune('a'+i%26))+".txt", int64(i), 0, "", nil, "")
			i++
		}
	})
	// 读 goroutine：持续 searchLocked。
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			ix.searchLocked(owner, "seed", map[string]string{})
		}
	})
	// 主 goroutine 跑有限轮后停。
	for range 500 {
		ix.searchLocked(owner, "seed", map[string]string{})
	}
	close(stop)
	wg.Wait()
}
