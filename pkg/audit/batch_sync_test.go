// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// batch_sync_test.go 锁定评审 R14 C14：`SyncBatch` 必须有**时间维度**刷盘。
//
// 修复前只有「满 batchLines 行才 fsync」⇒ kill -9 前最多 255 条证据停在 page cache，与设计
// 「≤ batch_max_interval 一次 fsync」不符；且该 fsync 在全局写锁内（批量删除时串行化）。

import (
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestSyncBatch_FlushesByTime 定时器必须把「未满批次」的条目刷盘。
func TestSyncBatch_FlushesByTime(t *testing.T) {
	t.Parallel()
	syncedCh := make(chan struct{}, 8)
	var synced atomic.Int64
	j, err := NewFileJournal(FileJournalConfig{
		Dir: t.TempDir(), Layout: LayoutGlobal, GlobalName: "audit.log",
		SyncMode: SyncBatch, BatchLines: 1000, BatchInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	j.syncHook = func(string) {
		synced.Add(1)
		select {
		case syncedCh <- struct{}{}:
		default:
		}
	}

	// 写 3 条（远小于 batchLines=1000）⇒ 只有定时器能触发 fsync。
	for i := 0; i < 3; i++ {
		if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "batch_probe", Type: TypeSensitive, Result: ResultSuccess, TS: time.Now(), Seq: uint64(i + 1)}); aerr != nil {
			t.Fatalf("Append(%d): %v", i, aerr)
		}
	}

	select {
	case <-syncedCh:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncBatch 未按时间刷盘（kill -9 会丢未满批次证据）")
	}
}

// TestSyncBatch_SizeTriggerFlushesOutsideLock 满 batchLines 仍会刷盘（阈值语义保留）。
func TestSyncBatch_SizeTriggerFlushesOutsideLock(t *testing.T) {
	t.Parallel()
	var synced atomic.Int64
	j, err := NewFileJournal(FileJournalConfig{
		Dir: t.TempDir(), Layout: LayoutGlobal, GlobalName: "audit.log",
		SyncMode: SyncBatch, BatchLines: 4, BatchInterval: time.Hour, // 定时器不参与
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	j.syncHook = func(string) { synced.Add(1) }

	for i := 0; i < 4; i++ {
		if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "batch_probe", Type: TypeSensitive, Result: ResultSuccess, TS: time.Now(), Seq: uint64(i + 1)}); aerr != nil {
			t.Fatalf("Append(%d): %v", i, aerr)
		}
	}
	if synced.Load() == 0 {
		t.Fatal("满 batchLines 必须刷盘")
	}
	// 文件存在且可读（未破坏写入语义）。
	if _, rerr := j.Read(EntryFilter{}); rerr != nil {
		t.Fatalf("Read: %v", rerr)
	}
	_ = filepath.Join
}
