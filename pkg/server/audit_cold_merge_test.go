// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_cold_merge_test.go 锁定 M4 收尾：**读面合并热段 + 冷段**。
//
// 语义：`/api/audit`、`/api/audit/export` 共用的 `readAuditEvents` 必须同时覆盖
// 「在线热段（Journal）」与「已归档冷段（audit.archive_dir，经 pkg/sync.FS 读取）」——
// 否则归档后历史就「消失」了 ✗。两段都升序 ⇒ 归并后整体升序；倒序/limit 由调用参数决定。

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestReadAuditEvents_MergesHotAndCold 归档后，读面仍能看到**全部**历史（冷+热、按 TS 升序）。
func TestReadAuditEvents_MergesHotAndCold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	hotDir := filepath.Join(t.TempDir(), "hot")
	j, err := audit.NewFileJournal(audit.FileJournalConfig{
		Dir: hotDir, Layout: audit.LayoutGlobal, GlobalName: "audit.log",
		SyncMode: audit.SyncNone, MaxSize: 200, MaxArchives: 16,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	const total = 12
	for i := range total {
		if aerr := j.Append(audit.Entry{Scope: "system", Kind: audit.KindEvent, Action: "t",
			TS: time.Unix(int64(1700000000+i), 0).UTC(), Detail: strings.Repeat("m", 80)}); aerr != nil {
			t.Fatalf("Append #%d: %v", i, aerr)
		}
	}

	// 把封段归档到冷目录（经 pkg/sync.FS：与生产装配同一条能力路径 ✓）。
	coldDir := filepath.Join(t.TempDir(), "cold")
	rep, aerr := audit.ArchiveSealed(ctx, j, audit.FSArchiver{FS: syncpkg.NewLocalFS(coldDir, nil)}, 0)
	if aerr != nil {
		t.Fatalf("ArchiveSealed: %v", aerr)
	}
	if rep.Archived == 0 {
		t.Fatal("测试前提不成立：没有封段被归档（把 MaxSize 调小或增加条目）")
	}

	// Handlers：热段 Journal + 配置里的冷段目录。
	cfg := Default()
	cfg.Audit.ArchiveDir = coldDir
	var cfgp atomic.Pointer[Config]
	cfgp.Store(cfg)
	h := &Handlers{auditJournal: j, cfgPtr: &cfgp}

	// ① 升序全量：冷 + 热 合并后应覆盖全部写入条目。
	events := h.readAuditEvents(ctx, audit.EntryFilter{}, 0, false)
	if len(events) != total {
		t.Fatalf("冷+热应合计 %d 条, got %d（冷段可能未被读到 ✗）", total, len(events))
	}
	for i := 1; i < len(events); i++ {
		if events[i].TS.Before(events[i-1].TS) {
			t.Fatalf("合并结果必须按 TS 升序: idx=%d %v < %v", i, events[i].TS, events[i-1].TS)
		}
	}

	// ② 最新优先 + limit：应取到**最新**的 N 条（来自热段）。
	const n = 3
	newest := h.readAuditEvents(ctx, audit.EntryFilter{}, n, true)
	if len(newest) != n {
		t.Fatalf("limit=%d ⇒ %d 条, got %d", n, n, len(newest))
	}
	if newest[0].TS.Before(newest[n-1].TS) {
		t.Fatalf("最新优先应倒序: first=%v last=%v", newest[0].TS, newest[n-1].TS)
	}
	if want := time.Unix(int64(1700000000+total-1), 0).UTC(); !newest[0].TS.Equal(want) {
		t.Fatalf("最新一条应是最后一次写入: got %v want %v", newest[0].TS, want)
	}

	// ③ 时间过滤（Since）也要作用到冷段：只保留晚于阈值的条目。
	since := time.Unix(int64(1700000000+total-2), 0).UTC()
	filtered := h.readAuditEvents(ctx, audit.EntryFilter{Since: since}, 0, false)
	if len(filtered) == 0 || len(filtered) >= total {
		t.Fatalf("Since 过滤应显著减少条数: got %d (total %d)", len(filtered), total)
	}
	for _, e := range filtered {
		if !e.TS.After(since) {
			t.Fatalf("Since 过滤不生效: %v <= %v", e.TS, since)
		}
	}
}
