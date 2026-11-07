// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// archive_unavailable_retain_test.go 锁定评审 R13 P2：**归档已配置但不可用**时，轮转不得按
// `max_archives` 搬移/删除封段（否则「以为已归档」时唯一副本会被覆盖或删除 ⇒ 证据静默丢失）。
//
// 注意：只跳过「删除」这一步是不够的 —— 后续 `.1 → .2` 的 rename 在 Unix 上会**覆盖**最旧的 `.2`，
// 证据照样丢 ✗。因此实现是「整轮跳过」。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestShiftArchives_RetainsWhenArchiveUnavailable 归档不可用 ⇒ 段名与内容都不变（不做任何搬移）。
func TestShiftArchives_RetainsWhenArchiveUnavailable(t *testing.T) {
	t.Parallel()
	j, err := NewFileJournal(FileJournalConfig{
		Dir: t.TempDir(), Layout: LayoutGlobal, GlobalName: "audit.log",
		SyncMode: SyncNone, MaxSize: 0, MaxArchives: 2,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })

	base := filepath.Join(j.Dir(), "audit.log")
	write := func(n, body string) {
		t.Helper()
		if werr := os.WriteFile(base+"."+n, []byte(body), 0o640); werr != nil {
			t.Fatalf("write .%s: %v", n, werr)
		}
	}

	// ① 基线语义（归档可用/未配置归档）：删最旧 `.2`，再把 `.1` 搬成 `.2`。
	write("1", "old-1\n")
	write("2", "old-2\n")
	j.shiftArchives(base)
	got, rerr := os.ReadFile(base + ".2")
	if rerr != nil {
		t.Fatalf("搬移后 .2 应存在: %v", rerr)
	}
	if string(got) != "old-1\n" {
		t.Fatalf("搬移后 .2 应是旧 .1 的内容, got %q", string(got))
	}
	if _, serr := os.Stat(base + ".1"); !os.IsNotExist(serr) {
		t.Fatalf("搬移后 .1 不应存在: %v", serr)
	}

	// ② 归档不可用：**整轮跳过** ⇒ `.1`/`.2` 名字与内容都保持原样（不覆盖、不删除）。
	write("1", "keep-1\n")
	write("2", "keep-2\n")
	j.SetArchiveUnavailable(true)
	j.shiftArchives(base)
	for n, want := range map[string]string{"1": "keep-1\n", "2": "keep-2\n"} {
		b, err := os.ReadFile(base + "." + n)
		if err != nil {
			t.Fatalf("归档不可用时 .%s 必须保留: %v", n, err)
		}
		if string(b) != want {
			t.Fatalf("归档不可用时 .%s 不得被覆盖: got %q want %q", n, string(b), want)
		}
	}
}
