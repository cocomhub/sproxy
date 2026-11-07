// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// purge_cold_test.go 锁定评审 R13 P2：审计保留期（TTL）必须**同时作用于冷段**。
//
// 背景：`FileJournal.Rewrite` 只重写在线段；开启归档后（`archive_keep=0` 时封段随即搬走），
// 热段里根本没有超龄数据 ⇒ TTL 实际不生效，超期证据永久留在冷存储。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// writeColdSegment 在 dir 下写一个只含一条指定 TS 条目的冷段（内容寻址命名无关，仅测按时间清理）。
func writeColdSegment(t *testing.T, dir, name string, ts time.Time) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	line, err := json.Marshal(Entry{
		Scope: "system", Kind: KindEvent, Action: "purge_probe",
		Type: "sensitive", Result: "success", TS: ts,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	p := filepath.Join(dir, name)
	if werr := os.WriteFile(p, append(line, '\n'), 0o640); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	return p
}

// TestPurgeCold_RemovesOldKeepsNew 过期段被删、未过期段保留。
func TestPurgeCold_RemovesOldKeepsNew(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)

	now := time.Unix(1_800_000_000, 0).UTC()
	old := writeColdSegment(t, dir, "audit.log.1.aaaaaaaaaaaaaaaa", now.Add(-30*24*time.Hour))
	fresh := writeColdSegment(t, dir, "audit.log.2.bbbbbbbbbbbbbbbb", now.Add(-1*time.Hour))

	n, err := PurgeCold(ctx, fsys, "", now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("PurgeCold: %v", err)
	}
	if n != 1 {
		t.Fatalf("应删除 1 个过期段, got %d", n)
	}
	if _, serr := os.Stat(old); !os.IsNotExist(serr) {
		t.Fatalf("过期冷段应被删除: %v", serr)
	}
	if _, serr := os.Stat(fresh); serr != nil {
		t.Fatalf("未过期冷段必须保留: %v", serr)
	}
}

// TestPurgeCold_KeepsUnknownAge 时间未知（无法解析）的段保守保留（宁多留不误删）。
func TestPurgeCold_KeepsUnknownAge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)
	// 非法 JSON ⇒ 首/尾探测失败 ⇒ First/Last 为零值。
	p := filepath.Join(dir, "audit.log.3.cccccccccccccccc")
	if err := os.WriteFile(p, []byte("not-json\n"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}
	n, err := PurgeCold(ctx, fsys, "", time.Now())
	if err != nil {
		t.Fatalf("PurgeCold: %v", err)
	}
	if n != 0 {
		t.Fatalf("时间未知的段不应删除, got %d", n)
	}
	if _, serr := os.Stat(p); serr != nil {
		t.Fatalf("时间未知的段必须保留: %v", serr)
	}
}
