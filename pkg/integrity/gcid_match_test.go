// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeSized(t *testing.T, size int) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f.bin")
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i*31 + 7)
	}
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestMatchAnyGCID_SameOrderAsDownloadVerify 钉住 **反查与下载校验同源**（2026-11-07 用户明示）：
// `MatchAnyGCID` 的候选顺序必须与 `RecomputeGCIDOrdered`（所有下载器共用入口）**完全一致**，
// 从而「用本地 GCID 反查分享 hash」与「下载时确认 GCID 一致」用的是**同一套逻辑**。
func TestMatchAnyGCID_SameOrderAsDownloadVerify(t *testing.T) {
	t.Parallel()
	p := writeSized(t, 3<<20)
	want := GCIDOrderFor(3 << 20)
	// 用**最后一个候选**的 GCID 作为目标 ⇒ 必须试满全部候选才命中，且 tried 顺序 == 期望顺序
	last := want[len(want)-1]
	gcid, gerr := RecomputeGCIDBlock(p, last)
	if gerr != nil {
		t.Fatalf("RecomputeGCIDBlock: %v", gerr)
	}
	hit, block, tried, err := MatchAnyGCID(p, map[string]struct{}{gcid: {}})
	if err != nil {
		t.Fatalf("MatchAnyGCID: %v", err)
	}
	if hit != gcid || block != last {
		t.Fatalf("应命中最后一个候选：hit=%s block=%d（期望 %s/%d）", hit, block, gcid, last)
	}
	if !slices.Equal(tried, want) {
		t.Fatalf("候选尝试顺序必须与 GCIDOrderFor 一致：got %v want %v", tried, want)
	}
}

// TestMatchAnyGCID_DoesNotHardcodeBlockSize 钉住 **不写死块大小**：命中任意候选都能成功，
// 且首个候选命中时只算一次（命中即返）。
func TestMatchAnyGCID_DoesNotHardcodeBlockSize(t *testing.T) {
	t.Parallel()
	// 文件必须**大于最大候选块**（4MiB）⇒ 各候选产出互不相同的 GCID
	// （否则「块 ≥ 文件大小」都退化为单块 GCID，命中会落在更早的候选上——那是预期行为）。
	p := writeSized(t, 5<<20)
	order := GCIDOrderFor(5 << 20)
	for _, bs := range order {
		g, err := RecomputeGCIDBlock(p, bs)
		if err != nil {
			t.Fatalf("RecomputeGCIDBlock(%d): %v", bs, err)
		}
		hit, block, tried, merr := MatchAnyGCID(p, map[string]struct{}{g: {}})
		if merr != nil {
			t.Fatalf("MatchAnyGCID: %v", merr)
		}
		if hit == "" || block != bs {
			t.Fatalf("块大小 %d 的 GCID 应可命中，got hit=%q block=%d", bs, hit, block)
		}
		if want := slices.Index(order, bs) + 1; len(tried) != want {
			t.Fatalf("命中即返：块 %d 应只试 %d 个候选，got %d", bs, want, len(tried))
		}
	}
}

// TestMatchAnyGCID_MissAndEmpty 未命中/空目标的边界语义。
func TestMatchAnyGCID_MissAndEmpty(t *testing.T) {
	t.Parallel()
	p := writeSized(t, 1<<20)
	hit, block, tried, err := MatchAnyGCID(p, map[string]struct{}{"deadbeef": {}})
	if err != nil {
		t.Fatalf("MatchAnyGCID: %v", err)
	}
	if hit != "" || block != 0 {
		t.Fatalf("未命中应返回空，got %q/%d", hit, block)
	}
	if len(tried) != len(GCIDOrderFor(1<<20)) {
		t.Fatalf("未命中应尝试全部候选，got %d", len(tried))
	}
	if h, _, tr, e := MatchAnyGCID(p, nil); h != "" || tr != nil || e != nil {
		t.Fatalf("空目标应直接返回空，got %q/%v/%v", h, tr, e)
	}
	empty := writeSized(t, 0)
	if h, _, _, e := MatchAnyGCID(empty, map[string]struct{}{"x": {}}); h != "" || e != nil {
		t.Fatalf("空文件应返回空且无错，got %q/%v", h, e)
	}
}

// TestMatchAnyGCID_CaseInsensitive 目标 hash 大小写不敏感（分享侧常见大写）。
func TestMatchAnyGCID_CaseInsensitive(t *testing.T) {
	t.Parallel()
	p := writeSized(t, 512<<10)
	order := GCIDOrderFor(512 << 10)
	g, err := RecomputeGCIDBlock(p, order[0])
	if err != nil {
		t.Fatalf("RecomputeGCIDBlock: %v", err)
	}
	hit, _, _, merr := MatchAnyGCID(p, map[string]struct{}{toUpperHex(g): {}})
	if merr != nil {
		t.Fatalf("MatchAnyGCID: %v", merr)
	}
	if hit == "" {
		t.Fatal("大写目标 hash 应命中")
	}
}

func toUpperHex(s string) string {
	out := make([]rune, 0, len(s))
	for _, c := range s {
		if c >= 'a' && c <= 'f' {
			c -= 'a' - 'A'
		}
		out = append(out, c)
	}
	return string(out)
}
