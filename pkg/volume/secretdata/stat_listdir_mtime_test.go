// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestStat_ListDir_ExposeLogicalMTime 钉住「加密卷的 Stat/ListDir 暴露逻辑层原始 mtime」。
//
// 背景（独立对抗审核发现并复核，2026-10-18）：`SecretdataFS.Stat`/`ListDir` 构造
// `syncpkg.Entry` 时**不填 MTime**，而上层 `pkg/server/files_service.go` 直接取
// `Entry.MTime` 作为 `ModTime` ⇒ 加密卷的 `list --json` / `meta` 的 `mod_time` **恒为 0** ✗，
// 与 README「逻辑 Stat/ListDir 用 meta 内原始 mtime」及 `docs/secret-volume.md §3.5` 的承诺
// 矛盾（mtime 明明已存在 `metaEntry.mtime` / `Meta.Original.MTime` 里，只是没暴露）。
// 该字段还参与同步比对（`pkg/syncexec` 按 MTime/Checksum 判差异）⇒ 恒 0 会削弱增量判定。
//
// 判据：写入时声明的 mtime 必须在 Stat / ListDir / **重启后重新加载**三条路径上逐位一致
// （meta 内以 RFC3339Nano 存储 ⇒ UnixNano 往返无损）。
func TestStat_ListDir_ExposeLogicalMTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const mt = int64(1730000000123456789) // UnixNano，非整秒（验证纳秒精度不丢）

	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	fs := newFSSharedInner(t, inner)
	if err := fs.MakeDir(ctx, "d"); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin",
		bytes.NewReader(data(100)), 100, mt); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	checkStat := func(fs *SecretdataFS, stage string) {
		t.Helper()
		e, err := fs.Stat(ctx, "d/f.bin")
		if err != nil || e == nil {
			t.Fatalf("%s: Stat 失败 err=%v e=%v", stage, err, e)
		}
		if e.MTime != mt {
			t.Errorf("%s: Stat 的 Entry.MTime=%d，want %d（上层 ModTime 取此值 ⇒ 不填则 mod_time 恒 0）",
				stage, e.MTime, mt)
		}
	}
	checkList := func(fs *SecretdataFS, stage string) {
		t.Helper()
		ents, err := fs.ListDir(ctx, "d")
		if err != nil {
			t.Fatalf("%s: ListDir 失败: %v", stage, err)
		}
		found := false
		for _, e := range ents {
			if e.Name != "f.bin" {
				continue
			}
			found = true
			if e.MTime != mt {
				t.Errorf("%s: ListDir 条目 MTime=%d，want %d", stage, e.MTime, mt)
			}
		}
		if !found {
			t.Fatalf("%s: ListDir 未返回 f.bin（条目=%v）", stage, ents)
		}
	}

	checkStat(fs, "写入后")
	checkList(fs, "写入后")

	// 目录条目同样要暴露 mtime（P2 修复 2026-10-18）：此前目录条目 MTime 恒 0。
	// MakeDir 记创建时刻 ⇒ 断言「非 0 且重启后保持」（不比较具体值，避免时钟敏感）。
	de, derr := fs.Stat(ctx, "d")
	if derr != nil || de == nil {
		t.Fatalf("Stat(d) 失败 err=%v e=%v", derr, de)
	}
	if de.MTime <= 0 {
		t.Errorf("目录条目 Stat 的 MTime=%d，应为创建时刻（>0）", de.MTime)
	}
	rootEnts, rerr := fs.ListDir(ctx, "")
	if rerr != nil {
		t.Fatalf("ListDir(根) 失败: %v", rerr)
	}
	for _, e := range rootEnts {
		if e.Name == "d" && e.MTime <= 0 {
			t.Errorf("根列表中的目录条目 d 的 MTime=%d，应为创建时刻（>0）", e.MTime)
		}
	}

	// 重启（从 meta 重新加载索引）后仍必须一致——mtime 是 meta 内的持久值。
	restarted := newFSSharedInner(t, inner)
	checkStat(restarted, "重启后")
	checkList(restarted, "重启后")
	if de2, derr2 := restarted.Stat(ctx, "d"); derr2 != nil || de2 == nil || de2.MTime != de.MTime {
		t.Errorf("重启后目录 mtime 应保持（%v vs %v，err=%v）", de2, de, derr2)
	}
}
