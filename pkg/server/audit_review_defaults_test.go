// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_review_defaults_test.go 锁定评审 R13 修掉的两个**配置默认值/语义**缺陷（P1）：
//
//  1. `audit.max_total_bytes` 的「非零安全默认 1 GiB」此前只在注释/文档里承诺，`Default()`
//     未赋值 ⇒ 实际 0（不限）⇒ 审计目录可被写满且无有界丢弃；
//  2. `audit.buffer_size: 0` 此前直接 `return nil, nil` ⇒ 连**落盘**一起关掉（整条审计链静默关闭），
//     与 `config.go`/`docs/config.md` 的「只关内存环形缓冲，落盘仍在」相反。

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestAuditDefaults_MaxTotalBytesIsBounded 默认配置必须给出有界总容量（1 GiB）。
func TestAuditDefaults_MaxTotalBytesIsBounded(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if got, want := int64(cfg.Audit.MaxTotalBytes), int64(1)<<30; got != want {
		t.Fatalf("audit.max_total_bytes 默认 = %d, want %d（文档承诺的非零安全默认）", got, want)
	}
}

// TestAuditServices_BufferSizeZeroKeepsJournal `buffer_size: 0` ⇒ 只关内存索引，**落盘照常**。
func TestAuditServices_BufferSizeZeroKeepsJournal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Audit.BufferSize = 0 // 运维按文档「只省内存」调优

	svc, journal := setupAuditServices(cfg, func() syncpkg.FS { return nil }, testLogger())
	if svc == nil || journal == nil {
		t.Fatalf("buffer_size=0 不应关闭落盘：svc=%v journal=%v", svc != nil, journal != nil)
	}
	t.Cleanup(func() { _ = journal.Close() })

	if err := svc.Emit(ctx, audit.Entry{
		Scope: "system", Kind: audit.KindEvent, Action: "buffer_zero_probe",
		Type: "sensitive", Result: "success", TS: time.Now(),
	}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	entries, err := journal.Read(audit.EntryFilter{Action: "buffer_zero_probe"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("落盘应保留 1 条, got %d（buffer_size=0 把落盘也关掉了 ✗）", len(entries))
	}
	if _, statErr := journal.Segments(); statErr != nil {
		t.Fatalf("Segments: %v", statErr)
	}
	_ = filepath.Join // 保持 import 稳定
}
