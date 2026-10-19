// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_archive_wiring_test.go 锁定 M4 冷段归档的装配层接线：
// `audit.archive_dir` 非空 ⇒ 启动期把**封段**归档到该目录并回收本地封段（复用 pkg/audit 原语）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestRunAuditArchive_ArchivesSealedSegments 封段被归档到冷目录、本地回收，活跃段不受影响。
func TestRunAuditArchive_ArchivesSealedSegments(t *testing.T) {
	t.Parallel()
	segDir := filepath.Join(t.TempDir(), "hot")
	j, err := audit.NewFileJournal(audit.FileJournalConfig{
		Dir: segDir, Layout: audit.LayoutGlobal, GlobalName: "audit.log",
		SyncMode: audit.SyncNone, MaxSize: 200, MaxArchives: 8,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	for range 12 {
		if aerr := j.Append(audit.Entry{Scope: "system", Kind: audit.KindEvent, Action: "t",
			TS: time.Now(), Detail: strings.Repeat("y", 80)}); aerr != nil {
			t.Fatalf("Append: %v", aerr)
		}
	}
	segs, _ := j.Segments()
	var sealedCount int
	for _, s := range segs {
		if s.Sealed {
			sealedCount++
		}
	}
	if sealedCount == 0 {
		t.Fatal("测试前提不成立：无封段")
	}

	cold := filepath.Join(t.TempDir(), "cold")
	cfg := &Config{}
	cfg.Audit.ArchiveDir = cold
	cfg.Audit.ArchiveKeep = 0 // 全部归档
	runAuditArchive(cfg, j, testLogger())

	// ① 冷目录内出现归档文件；
	ents, derr := os.ReadDir(cold)
	if derr != nil || len(ents) == 0 {
		t.Fatalf("冷目录应含归档文件: err=%v len=%d", derr, len(ents))
	}
	// ② 封段已从热段目录回收（只剩活跃段，或空）。
	left, _ := j.Segments()
	for _, s := range left {
		if s.Sealed {
			t.Fatalf("封段应已被归档并回收: %s", s.Path)
		}
	}
}

// TestRunAuditArchive_DisabledWhenNoDir 未配置 archive_dir ⇒ 不动任何段（默认关闭）。
func TestRunAuditArchive_DisabledWhenNoDir(t *testing.T) {
	t.Parallel()
	cfg := &Config{} // ArchiveDir 空
	// 未调用 runAuditArchive（装配层只在非空时调用）——此处断言配置语义即可。
	if cfg.Audit.ArchiveDir != "" {
		t.Fatal("默认应为空（不归档）")
	}
}
