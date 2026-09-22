// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"strings"
	"testing"
)

// TestMerge3_NoConflictOursUnchanged 一方未改 → 无冲突取 theirs。
func TestMerge3_NoConflictOursUnchanged(t *testing.T) {
	t.Parallel()
	base := []byte("line1\nline2\nline3\n")
	ours := []byte("line1\nline2\nline3\n") // ours 未改
	theirs := []byte("line1\nline2 modified\nline3\n")
	out, conflicted, hunks := Merge3(base, ours, theirs)
	if conflicted {
		t.Fatalf("ours 未改不应冲突, hunks=%+v", hunks)
	}
	if !bytes.Equal(out, theirs) {
		t.Fatalf("结果应取 theirs, got %q", out)
	}
	if len(hunks) != 0 {
		t.Fatalf("无冲突 hunk 应为空, got %+v", hunks)
	}
}

// TestMerge3_NoConflictTheirsUnchanged 一方未改 → 无冲突取 ours。
func TestMerge3_NoConflictTheirsUnchanged(t *testing.T) {
	t.Parallel()
	base := []byte("line1\nline2\nline3\n")
	theirs := []byte("line1\nline2\nline3\n")
	ours := []byte("line1\nOURS\nline3\n")
	out, conflicted, _ := Merge3(base, ours, theirs)
	if conflicted {
		t.Fatalf("theirs 未改不应冲突")
	}
	if !bytes.Equal(out, ours) {
		t.Fatalf("结果应取 ours, got %q", out)
	}
}

// TestMerge3_NonOverlappingMerge 非重叠修改自动合并。
func TestMerge3_NonOverlappingMerge(t *testing.T) {
	t.Parallel()
	base := []byte("a\nb\nc\n")
	ours := []byte("A\nb\nc\n")
	theirs := []byte("a\nB\nc\n")
	out, conflicted, hunks := Merge3(base, ours, theirs)
	if conflicted {
		t.Fatalf("非重叠修改不应冲突, hunks=%+v", hunks)
	}
	want := "A\nB\nc\n"
	if string(out) != want {
		t.Fatalf("非重叠合并 = %q, want %q", out, want)
	}
	if len(hunks) != 0 {
		t.Fatalf("无冲突 hunks 应为空")
	}
}

// TestMerge3_OverlappingConflict 重叠修改 → 冲突标记 + hunk 明细。
func TestMerge3_OverlappingConflict(t *testing.T) {
	t.Parallel()
	base := []byte("a\nb\nc\n")
	ours := []byte("a\nOURS\nc\n")
	theirs := []byte("a\nTHEIRS\nc\n")
	out, conflicted, hunks := Merge3(base, ours, theirs)
	if !conflicted {
		t.Fatalf("重叠修改应冲突")
	}
	if len(hunks) != 1 {
		t.Fatalf("应 1 个冲突 hunk, got %+v", hunks)
	}
	h := hunks[0]
	if strings.Join(h.Ours, "\n") != "OURS" {
		t.Fatalf("hunk.Ours = %v, want [OURS]", h.Ours)
	}
	if strings.Join(h.Theirs, "\n") != "THEIRS" {
		t.Fatalf("hunk.Theirs = %v, want [THEIRS]", h.Theirs)
	}
	if !strings.Contains(string(out), "<<<<<<<") || !strings.Contains(string(out), ">>>>>>>") {
		t.Fatalf("冲突结果应含标记, got %q", out)
	}
}

// TestMerge3_EmptyBase 空 base：双方都新增 → 无冲突全并。
func TestMerge3_EmptyBase(t *testing.T) {
	t.Parallel()
	base := []byte{}
	ours := []byte("a\nb\n")
	theirs := []byte("a\nc\n")
	out, conflicted, _ := Merge3(base, ours, theirs)
	if conflicted {
		t.Fatalf("空 base 双方新增不应冲突")
	}
	if !strings.Contains(string(out), "b") || !strings.Contains(string(out), "c") {
		t.Fatalf("空 base 合并应含双方新增, got %q", out)
	}
}

// TestMerge3_CRLFLineEndings 行尾差异（CRLF vs LF）按行归一处理不报错。
func TestMerge3_CRLFLineEndings(t *testing.T) {
	t.Parallel()
	base := []byte("a\r\nb\r\nc\r\n")
	ours := []byte("a\r\nOURS\r\nc\r\n")
	theirs := []byte("a\r\nb\r\nc\r\n")
	out, conflicted, _ := Merge3(base, ours, theirs)
	if conflicted {
		t.Fatalf("theirs 未改不应冲突")
	}
	// Merge3 统一归一 \r\n → \n 输出（行级合并语义）；断言比较归一后的 ours。
	want := bytes.ReplaceAll(ours, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(out, want) {
		t.Fatalf("CRLF 结果应取 ours, got %q", out)
	}
}

// TestMerge3_IdenticalAll 三方相同 → 无冲突原样返回。
func TestMerge3_IdenticalAll(t *testing.T) {
	t.Parallel()
	base := []byte("x\ny\n")
	out, conflicted, hunks := Merge3(base, base, base)
	if conflicted || len(hunks) != 0 {
		t.Fatalf("三方相同不应冲突")
	}
	if !bytes.Equal(out, base) {
		t.Fatalf("三方相同结果应不变")
	}
}
