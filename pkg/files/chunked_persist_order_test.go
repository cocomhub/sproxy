// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestUploadStore_PersistOrdering_ConcurrentPersistsConvergeToNewest 钉住「并发持久化最终必须收敛到
// 内存最新状态」：旧实现「RLock 拍快照 → 解锁 → writeSessionJSON」是两拍，两个并发持久化若交错
// （旧快照晚落盘）磁盘会停在旧状态，重启丢分块（RV10-SLICE3 审计的既有缺陷）。
//
// 修复是会话内嵌串行锁（ChunkedUploadSession.persistMu）：同一会话的「拍快照 → 落盘」整体互斥
// ⇒ 快照拍序 == 落盘序 ⇒ 后写者赢 = 更新的快照 ⇒ 磁盘最终收敛到最新。
//
// 为什么不是确定性 probe 复现：per-id 锁覆盖「拍快照 → 落盘」两阶段后，在同一 id 上制造
// 「旧快照晚落盘」的交错需要持有 per-id 锁期间阻塞（否则 B 拿不到锁）⇒ 与「B 先落盘」互斥，
// 结构性不可能（试过 persistProbe 版：修复后必然死锁，已弃）。因此本测试退化为**最终态断言**：
// 大量并发 MarkChunkReceived + 并发持久化，断言磁盘最终收敛到内存最新。
//
// **变异盲区（如实）**：去掉 persistMu.Lock 后本测试**不必然红**（交错是概率性的，-count=20
// 也未命中）⇒ 结构性锁的「存在性」由 `internal/archcheck/chunked_persist_serialization_test.go`
// 门禁钉住（断言三入口函数体必须出现 .persistMu.Lock()，变异会红）。本测试作为并发回归哨兵保留
// （修复后必然绿；将来若有真回归且概率撞上，它给出可诊断的最终态失败）。
func TestUploadStore_PersistOrdering_ConcurrentPersistsConvergeToNewest(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	us := MustNewUploadStore(tmp, time.Hour, nil)
	defer us.Stop()

	const totalChunks = 1024
	if _, err := us.CreateSession("conc-1", "f.txt", int64(totalChunks)*1024, 1024, totalChunks, strings.Repeat("a", 64), 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// 并发推进：每个分块都触发一次异步持久化；同时穿插同步 PersistNow，最大化「两拍交错」窗口。
	var wg sync.WaitGroup
	markCh := make(chan int, totalChunks)
	for i := range totalChunks {
		markCh <- i
	}
	close(markCh)

	const workers = 32
	for range workers {
		wg.Go(func() { markChunkWorker(t, us, markCh) })
	}
	// 同步持久化穿插（与异步 persistCh 并发）。
	for range 32 {
		wg.Go(func() { persistNowWorker(t, us) })
	}
	wg.Wait()

	// 等所有异步持久化落盘完成（per-id 锁已保证拍序==写序；此等待只是确保 I/O 结束）。
	ok := testutil.WaitForBool(5*time.Second, func() bool {
		return persistConverged(us, tmp, totalChunks)
	})
	if !ok {
		data, _ := os.ReadFile(filepath.Join(tmp, "conc-1", "session.json"))
		t.Fatalf("磁盘最终未收敛到最新态（旧快照覆盖了新快照）——per-id 串行锁失效？session.json:\n%s", truncateStr(string(data), 1200))
	}
}

// markChunkWorker 并发标记收到的分块（抽取自 PersistOrdering 的 worker goroutine 体）。
func markChunkWorker(t *testing.T, us *UploadStore, markCh <-chan int) {
	t.Helper()
	for i := range markCh {
		cs := strings.Repeat(string(rune('a'+i%26)), 32)
		if err := us.MarkChunkReceived("conc-1", i, cs); err != nil {
			t.Errorf("MarkChunkReceived(%d): %v", i, err)
			return
		}
	}
}

// persistNowWorker 并发穿插同步持久化（抽取自 PersistOrdering 的 persist goroutine 体）。
func persistNowWorker(t *testing.T, us *UploadStore) {
	t.Helper()
	if err := us.PersistNow("conc-1"); err != nil {
		t.Errorf("PersistNow: %v", err)
	}
}

// persistConverged 检查内存与磁盘是否都已收敛到「全部分块收到」（抽取自 PersistOrdering 的
// WaitFor 断言闭包）。返回 false 即尚未收敛，供轮询继续等待。
func persistConverged(us *UploadStore, tmp string, totalChunks int) bool {
	us.mu.RLock()
	s := us.sessions["conc-1"]
	us.mu.RUnlock()
	if s == nil {
		return false
	}
	for _, r := range s.ReceivedChunks {
		if !r {
			return false
		}
	}
	data, err := os.ReadFile(filepath.Join(tmp, "conc-1", "session.json"))
	if err != nil {
		return false
	}
	var onDisk struct {
		ReceivedChunks []bool `json:"received_chunks"`
	}
	if json.Unmarshal(data, &onDisk) != nil {
		return false
	}
	if len(onDisk.ReceivedChunks) != totalChunks {
		return false
	}
	for _, r := range onDisk.ReceivedChunks {
		if !r {
			return false
		}
	}
	return true
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
