// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"errors"
	"fmt"
	"regexp"
	"testing"
)

// findDiff 按路径查找 diff 条目。
func findDiff(t *testing.T, diffs []DiffEntry, path string) *DiffEntry {
	t.Helper()
	for i := range diffs {
		if diffs[i].Path == path {
			return &diffs[i]
		}
	}
	t.Fatalf("未找到路径 %q 的 diff 条目（全部: %+v）", path, diffs)
	return nil
}

// TestComputeDiff_Created 验证目标不存在 → created。
func TestComputeDiff_Created(t *testing.T) {
	srcs := []Entry{{Path: "new.txt", Size: 5, MTime: 1, Checksum: "c1"}}
	diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return nil, nil }, ConflictSkip)
	if err != nil {
		t.Fatalf("ComputeDiff 不应返回 error: %v", err)
	}
	d := findDiff(t, diffs, "new.txt")
	if d.Action != ActionCreated {
		t.Fatalf("应返回 created，got %q", d.Action)
	}
	if d.Src == nil || d.Src.Path != "new.txt" {
		t.Fatalf("Src 应为源条目，got %+v", d.Src)
	}
	if d.Dst != nil {
		t.Fatalf("Dst 应为 nil，got %+v", d.Dst)
	}
}

// TestComputeDiff_Skipped_SameChecksum 验证 checksum 相同 → skipped（即使 mtime 不同）。
func TestComputeDiff_Skipped_SameChecksum(t *testing.T) {
	srcs := []Entry{{Path: "a.txt", Size: 10, MTime: 100, Checksum: "same"}}
	diffs, err := ComputeDiff(srcs, func(path string) (*Entry, error) {
		return &Entry{Path: path, Size: 10, MTime: 50, Checksum: "same"}, nil
	}, ConflictSkip)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if d := findDiff(t, diffs, "a.txt"); d.Action != ActionSkipped {
		t.Fatalf("应返回 skipped，got %q", d.Action)
	}
}

// TestComputeDiff_Skipped_MTimeFallback 验证 checksum 缺失时按 mtime 判定相同。
func TestComputeDiff_Skipped_MTimeFallback(t *testing.T) {
	srcs := []Entry{{Path: "a.txt", Size: 10, MTime: 100, Checksum: ""}}
	diffs, err := ComputeDiff(srcs, func(path string) (*Entry, error) {
		return &Entry{Path: path, Size: 10, MTime: 100, Checksum: ""}, nil
	}, ConflictSkip)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if d := findDiff(t, diffs, "a.txt"); d.Action != ActionSkipped {
		t.Fatalf("mtime 相同应返回 skipped，got %q", d.Action)
	}
}

// TestComputeDiff_Different_SizeMismatch 验证大小不同 → 不同 → 按策略决策。
func TestComputeDiff_Different_SizeMismatch(t *testing.T) {
	srcs := []Entry{{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}}
	dst := &Entry{Path: "a.txt", Size: 20, MTime: 100, Checksum: "c2"}
	diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return dst, nil }, ConflictOverwrite)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	d := findDiff(t, diffs, "a.txt")
	if d.Action != ActionUpdated {
		t.Fatalf("overwrite 策略应返回 updated，got %q", d.Action)
	}
	if d.Dst == nil || d.Dst.Size != 20 {
		t.Fatalf("Dst 应为目标条目，got %+v", d.Dst)
	}
}

// TestComputeDiff_ConflictSkip 验证 skip 策略下目标不同 → skipped_conflict。
func TestComputeDiff_ConflictSkip(t *testing.T) {
	srcs := []Entry{{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}}
	dst := &Entry{Path: "a.txt", Size: 20, MTime: 100, Checksum: "c2"}
	diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return dst, nil }, ConflictSkip)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if d := findDiff(t, diffs, "a.txt"); d.Action != ActionSkippedConflict {
		t.Fatalf("skip 策略应返回 skipped_conflict，got %q", d.Action)
	}
}

// TestComputeDiff_ConflictRename 验证 conflict_rename 产生 renameDstTo。
func TestComputeDiff_ConflictRename(t *testing.T) {
	srcs := []Entry{{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}}
	dst := &Entry{Path: "a.txt", Size: 20, MTime: 100, Checksum: "c2"}
	diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return dst, nil }, ConflictRename)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	d := findDiff(t, diffs, "a.txt")
	if d.Action != ActionConflictRenamed {
		t.Fatalf("应返回 conflict_renamed，got %q", d.Action)
	}
	re := regexp.MustCompile(`^a\.txt\.conflict-\d+$`)
	if !re.MatchString(d.RenameDstTo) {
		t.Fatalf("renameDstTo 格式不正确，got %q", d.RenameDstTo)
	}
}

// TestComputeDiff_DstStatError 验证 dstStat 返回 error → ActionError + 非 nil error。
func TestComputeDiff_DstStatError(t *testing.T) {
	srcs := []Entry{{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}}
	wantErr := errors.New("stat 失败")
	diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return nil, wantErr }, ConflictSkip)
	if err == nil {
		t.Fatalf("dstStat 失败应返回非 nil error")
	}
	d := findDiff(t, diffs, "a.txt")
	if d.Action != ActionError {
		t.Fatalf("应返回 error action，got %q", d.Action)
	}
	if d.Err == nil || !errors.Is(d.Err, wantErr) {
		t.Fatalf("DiffEntry.Err 应保留原始 error，got %v", d.Err)
	}
}

// TestComputeDiff_DirDirSkipped 验证源/目标都是目录 → skipped。
func TestComputeDiff_DirDirSkipped(t *testing.T) {
	srcs := []Entry{{Path: "empty", IsDir: true, Size: 0, MTime: 100}}
	diffs, err := ComputeDiff(srcs, func(path string) (*Entry, error) {
		return &Entry{Path: path, IsDir: true, Size: 0, MTime: 200}, nil
	}, ConflictSkip)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if d := findDiff(t, diffs, "empty"); d.Action != ActionSkipped {
		t.Fatalf("目录对目录应返回 skipped，got %q", d.Action)
	}
}

// TestComputeDiff_DirVsFile 验证源目录 vs 目标文件 → 按策略冲突处理。
func TestComputeDiff_DirVsFile(t *testing.T) {
	srcs := []Entry{{Path: "x", IsDir: true, Size: 0, MTime: 100}}
	dst := &Entry{Path: "x", IsDir: false, Size: 10, MTime: 100, Checksum: "c2"}
	diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return dst, nil }, ConflictSkip)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if d := findDiff(t, diffs, "x"); d.Action != ActionSkippedConflict {
		t.Fatalf("目录 vs 文件（skip）应返回 skipped_conflict，got %q", d.Action)
	}
}

// TestComputeDiff_EmptySrc 验证空源列表返回空 diff。
func TestComputeDiff_EmptySrc(t *testing.T) {
	diffs, err := ComputeDiff(nil, func(string) (*Entry, error) { return nil, nil }, ConflictSkip)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if len(diffs) != 0 {
		t.Fatalf("空源应返回空 diff，got %d", len(diffs))
	}
}

// TestComputeDiff_MultiplePaths 验证多条目顺序与 dstStat 按路径分发。
func TestComputeDiff_MultiplePaths(t *testing.T) {
	srcs := []Entry{
		{Path: "b.txt", Size: 10, MTime: 100, Checksum: "c1"},
		{Path: "a.txt", Size: 5, MTime: 1, Checksum: "cA"},
	}
	diffs, err := ComputeDiff(srcs, func(path string) (*Entry, error) {
		if path == "b.txt" {
			return &Entry{Path: path, Size: 10, MTime: 100, Checksum: "c1"}, nil
		}
		return nil, nil
	}, ConflictSkip)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if len(diffs) != 2 {
		t.Fatalf("应有 2 个 diff，got %d", len(diffs))
	}
	if findDiff(t, diffs, "a.txt").Action != ActionCreated {
		t.Fatalf("a.txt 应 created")
	}
	if findDiff(t, diffs, "b.txt").Action != ActionSkipped {
		t.Fatalf("b.txt 应 skipped")
	}
	// 保持输入顺序（非按路径排序）
	if diffs[0].Path != "b.txt" || diffs[1].Path != "a.txt" {
		t.Fatalf("应保持输入顺序，got %s, %s", diffs[0].Path, diffs[1].Path)
	}
}

// TestComputeDiff_MixedErrors 验证一个 dstStat 失败不影响其他条目，且错误被聚合。
func TestComputeDiff_MixedErrors(t *testing.T) {
	srcs := []Entry{
		{Path: "ok.txt", Size: 1, MTime: 1, Checksum: "c1"},
		{Path: "bad.txt", Size: 1, MTime: 1, Checksum: "c2"},
	}
	diffs, err := ComputeDiff(srcs, func(path string) (*Entry, error) {
		if path == "bad.txt" {
			return nil, fmt.Errorf("bad stat: %w", errors.New("boom"))
		}
		return &Entry{Path: path, Size: 1, MTime: 1, Checksum: "c1"}, nil
	}, ConflictSkip)
	if err == nil {
		t.Fatalf("存在 dstStat 失败应返回非 nil error")
	}
	if len(diffs) != 2 {
		t.Fatalf("应有 2 个 diff，got %d", len(diffs))
	}
	if findDiff(t, diffs, "ok.txt").Action != ActionSkipped {
		t.Fatalf("ok.txt 应 skipped")
	}
	if findDiff(t, diffs, "bad.txt").Action != ActionError {
		t.Fatalf("bad.txt 应 error")
	}
}

// TestComputeDiff_FileVsDir_TypeConflict 验证文件 vs 目录的类型冲突即使 Size/MTime
// 巧合相等也不被 entriesSame 误判为 skipped（审查 I-4 回归）。
func TestComputeDiff_FileVsDir_TypeConflict(t *testing.T) {
	srcs := []Entry{{Path: "x", Size: 0, MTime: 100, Checksum: "c1"}} // 文件
	dst := &Entry{Path: "x", IsDir: true, Size: 0, MTime: 100}        // 目录（Size/MTime 与文件巧合相等）

	t.Run("skip → skipped_conflict", func(t *testing.T) {
		diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return dst, nil }, ConflictSkip)
		if err != nil {
			t.Fatalf("不应返回 error: %v", err)
		}
		if d := findDiff(t, diffs, "x"); d.Action != ActionSkippedConflict {
			t.Fatalf("文件 vs 目录（skip）应 skipped_conflict，got %q", d.Action)
		}
	})
	t.Run("overwrite → updated（Engine 侧拒绝覆盖目录）", func(t *testing.T) {
		diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return dst, nil }, ConflictOverwrite)
		if err != nil {
			t.Fatalf("不应返回 error: %v", err)
		}
		if d := findDiff(t, diffs, "x"); d.Action != ActionUpdated {
			t.Fatalf("文件 vs 目录（overwrite）应 updated，got %q", d.Action)
		}
	})
}

// TestComputeDiff_DirVsFile_TypeConflict 验证目录 vs 文件的类型冲突（审查 I-4）。
func TestComputeDiff_DirVsFile_TypeConflict(t *testing.T) {
	srcs := []Entry{{Path: "x", IsDir: true, Size: 0, MTime: 100}} // 目录
	dst := &Entry{Path: "x", Size: 0, MTime: 100, Checksum: "c1"}  // 文件（Size/MTime 巧合相等）

	diffs, err := ComputeDiff(srcs, func(string) (*Entry, error) { return dst, nil }, ConflictOverwrite)
	if err != nil {
		t.Fatalf("不应返回 error: %v", err)
	}
	if d := findDiff(t, diffs, "x"); d.Action != ActionUpdated {
		t.Fatalf("目录 vs 文件（overwrite）应 updated（Engine syncDir 删除冲突文件后建目录），got %q", d.Action)
	}
}
