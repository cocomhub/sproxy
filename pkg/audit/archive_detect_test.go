// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// archive_detect_test.go 锁定评审 R13 P2：**用生产段名 `audit.log`** 时，归档段必须被
// `Rotate`/`Rewrite` 跳过。
//
// 旧实现用 `strings.Contains(base, ".audit.log.")` 判「是否归档段」——它只对默认段名
// `compliance.audit.log`（含前置点）成立；生产段名是 `audit.log`，归档名是 `audit.log.1`，
// 不含前置点 ⇒ 判据恒 false ⇒ `Rewrite`（audit_ttl 开启时生产可达）会把归档段也读入重写，
// `Rotate` 会把 `audit.log.1` 再轮转成 `audit.log.1.1`（该名字的 segmentBase 不再是 globalName
// ⇒ 从 Segments()/Read() 消失，磁盘仍占）。

import (
	"strings"
	"testing"
	"time"
)

// TestRotateRewrite_SkipArchivedWithProductionName 生产段名下归档段不得被再轮转/重写。
func TestRotateRewrite_SkipArchivedWithProductionName(t *testing.T) {
	t.Parallel()
	j, err := NewFileJournal(FileJournalConfig{
		Dir:         t.TempDir(),
		Layout:      LayoutGlobal,
		GlobalName:  "audit.log", // ← 生产装配使用的名字（不含前置点）
		SyncMode:    SyncNone,
		MaxSize:     1, // 每条都触发轮转
		MaxArchives: 5,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	for i := range 3 {
		if aerr := j.Append(Entry{
			Scope: "system", Kind: KindEvent, Action: "probe",
			Type: "sensitive", Result: "success", TS: time.Unix(int64(1700000000+i), 0).UTC(),
		}); aerr != nil {
			t.Fatalf("Append #%d: %v", i, aerr)
		}
	}
	before, err := j.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	var archived []string
	for _, s := range before {
		if strings.HasSuffix(s.Path, ".audit.log.1") || strings.Contains(s.Path, "audit.log.1") {
			archived = append(archived, s.Path)
		}
	}
	if len(archived) == 0 {
		t.Fatalf("测试前提不成立：MaxSize=1 应已产生归档段, got %+v", before)
	}

	// ① Rewrite 只处理在线段：归档段必须原样存活（旧实现会把它一起重写）。
	if rerr := j.Rewrite(func(Entry) bool { return true }); rerr != nil {
		t.Fatalf("Rewrite: %v", rerr)
	}
	afterRewrite, err := j.Segments()
	if err != nil {
		t.Fatalf("Segments(after rewrite): %v", err)
	}
	if !hasSegment(afterRewrite, "audit.log.1") {
		t.Fatalf("Rewrite 后归档段消失/被改名: %+v", afterRewrite)
	}

	// ② Rotate 不重复轮转归档段（不得出现 audit.log.1.1 这种不可读名字）。
	if rerr := j.Rotate("manual"); rerr != nil {
		t.Fatalf("Rotate: %v", rerr)
	}
	final, err := j.Segments()
	if err != nil {
		t.Fatalf("Segments(final): %v", err)
	}
	for _, s := range final {
		if strings.Contains(s.Path, "audit.log.1.1") || strings.HasSuffix(s.Path, ".audit.log.1.1") {
			t.Fatalf("归档段被二次轮转（会成为不可读孤儿）: %s", s.Path)
		}
	}
	if !hasSegment(final, "audit.log.1") {
		t.Fatalf("Rotate 后归档段消失: %+v", final)
	}
}

// hasSegment 报告 segs 中是否存在以 want 结尾的段路径。
func hasSegment(segs []SegmentInfo, want string) bool {
	for _, s := range segs {
		if strings.HasSuffix(s.Path, want) {
			return true
		}
	}
	return false
}
