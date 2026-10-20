// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// journal_seal_test.go 锁定 M4「轮转 → 封段回调」的三条契约：
//  ① 回调在**Journal 锁之外**触发（回调内可安全读 Segments/Read ⇒ 不自死锁）；
//  ② 空段轮转（无可归档内容）**不触发**；
//  ③ 回调 panic 被隔离，绝不影响写入（审计不得因归档把业务打崩）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFileJournal_OnSealFiresOutsideLock 回调内读段清单/条目不得死锁（证明在锁外）。
func TestFileJournal_OnSealFiresOutsideLock(t *testing.T) {
	t.Parallel()
	var self *FileJournal
	var sealedAtCallback int
	var fired int
	j, err := NewFileJournal(FileJournalConfig{
		Dir: filepath.Join(t.TempDir(), "hot"), Layout: LayoutGlobal, GlobalName: "audit.log",
		SyncMode: SyncNone, MaxSize: 200, MaxArchives: 8,
		OnSeal: func() {
			fired++
			// 两个都会拿 j.mu：若回调在锁内触发 ⇒ 这里会永久阻塞（测试超时）✓
			segs, serr := self.Segments()
			if serr == nil {
				for _, s := range segs {
					if s.Sealed {
						sealedAtCallback++
					}
				}
			}
			if _, rerr := self.Read(EntryFilter{}); rerr != nil {
				t.Logf("Read in callback: %v", rerr)
			}
		},
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	self = j
	t.Cleanup(func() { _ = j.Close() })

	for range 12 {
		if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "t",
			TS: time.Now(), Detail: strings.Repeat("z", 80)}); aerr != nil {
			t.Fatalf("Append: %v", aerr)
		}
	}
	if fired == 0 {
		t.Fatal("轮转产生封段后必须触发 OnSeal")
	}
	if sealedAtCallback == 0 {
		t.Fatal("回调触发时封段应已可见（rename 已完成）")
	}
}

// TestFileJournal_OnSealSkippedForEmptyRotate 空 Journal 轮转不触发回调（不制造空归档）。
func TestFileJournal_OnSealSkippedForEmptyRotate(t *testing.T) {
	t.Parallel()
	var fired int
	j, err := NewFileJournal(FileJournalConfig{
		Dir: filepath.Join(t.TempDir(), "hot"), Layout: LayoutGlobal, GlobalName: "audit.log",
		SyncMode: SyncNone, OnSeal: func() { fired++ },
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if rerr := j.Rotate("empty"); rerr != nil {
		t.Fatalf("Rotate: %v", rerr)
	}
	if fired != 0 {
		t.Fatalf("空段轮转不应触发 OnSeal, fired=%d", fired)
	}
}

// TestFileJournal_OnSealPanicIsolated 回调 panic 被隔离 ⇒ 写入照常成功、后续写入不受影响。
func TestFileJournal_OnSealPanicIsolated(t *testing.T) {
	t.Parallel()
	j, err := NewFileJournal(FileJournalConfig{
		Dir: filepath.Join(t.TempDir(), "hot"), Layout: LayoutGlobal, GlobalName: "audit.log",
		SyncMode: SyncNone, MaxSize: 200, MaxArchives: 8,
		OnSeal: func() { panic("归档实现 panic（测试）") },
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	for range 12 {
		if aerr := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "t",
			TS: time.Now(), Detail: strings.Repeat("p", 80)}); aerr != nil {
			t.Fatalf("回调 panic 不得让 Append 返回错误: %v", aerr)
		}
	}
	// 段文件确实写入了（panic 不阻断落盘）。
	segs, serr := j.Segments()
	if serr != nil || len(segs) == 0 {
		t.Fatalf("应存在段文件: err=%v count=%d", serr, len(segs))
	}
	if _, statErr := os.Stat(segs[0].Path); statErr != nil {
		t.Fatalf("段文件应存在: %v", statErr)
	}
}
