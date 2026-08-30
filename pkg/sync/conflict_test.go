// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"regexp"
	"testing"
)

// TestDecide_Skip 验证 skip 策略：目标存在且不同 → skipped_conflict，无 rename。
func TestDecide_Skip(t *testing.T) {
	src := &Entry{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}
	dst := &Entry{Path: "a.txt", Size: 20, MTime: 200, Checksum: "c2"}
	action, rename := Decide(ConflictSkip, src, dst)
	if action != ActionSkippedConflict {
		t.Fatalf("skip 策略应返回 skipped_conflict，got %q", action)
	}
	if rename != "" {
		t.Fatalf("skip 策略不应产生 rename，got %q", rename)
	}
}

// TestDecide_Overwrite 验证 overwrite 策略：无条件返回 updated。
func TestDecide_Overwrite(t *testing.T) {
	src := &Entry{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}
	dst := &Entry{Path: "a.txt", Size: 20, MTime: 200, Checksum: "c2"}
	action, rename := Decide(ConflictOverwrite, src, dst)
	if action != ActionUpdated {
		t.Fatalf("overwrite 策略应返回 updated，got %q", action)
	}
	if rename != "" {
		t.Fatalf("overwrite 策略不应产生 rename，got %q", rename)
	}
}

// TestDecide_LWW 验证 last-writer-wins：mtime 新者胜。
func TestDecide_LWW(t *testing.T) {
	cases := []struct {
		name        string
		srcMTime    int64
		dstMTime    int64
		srcChecksum string
		dstChecksum string
		want        Action
	}{
		{
			name:     "src 更新（mtime 更大）→ updated",
			srcMTime: 300, dstMTime: 200,
			srcChecksum: "c1", dstChecksum: "c2",
			want: ActionUpdated,
		},
		{
			name:     "dst 更新（src mtime 更小）→ skipped_conflict",
			srcMTime: 100, dstMTime: 200,
			srcChecksum: "c1", dstChecksum: "c2",
			want: ActionSkippedConflict,
		},
		{
			name:     "mtime 相等且 checksum 都可得且不同 → src 胜（updated）",
			srcMTime: 200, dstMTime: 200,
			srcChecksum: "c1", dstChecksum: "c2",
			want: ActionUpdated,
		},
		{
			name:     "mtime 相等且 checksum 都可得且相同 → skipped_conflict",
			srcMTime: 200, dstMTime: 200,
			srcChecksum: "same", dstChecksum: "same",
			want: ActionSkippedConflict,
		},
		{
			name:     "mtime 相等且 checksum 不可得 → src 胜（updated）",
			srcMTime: 200, dstMTime: 200,
			srcChecksum: "", dstChecksum: "",
			want: ActionUpdated,
		},
		{
			name:     "mtime 相等且 src 有 checksum、dst 无 → src 胜（updated）",
			srcMTime: 200, dstMTime: 200,
			srcChecksum: "c1", dstChecksum: "",
			want: ActionUpdated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &Entry{Path: "a.txt", Size: 10, MTime: tc.srcMTime, Checksum: tc.srcChecksum}
			dst := &Entry{Path: "a.txt", Size: 20, MTime: tc.dstMTime, Checksum: tc.dstChecksum}
			action, rename := Decide(ConflictLWW, src, dst)
			if action != tc.want {
				t.Fatalf("lww 应返回 %q，got %q", tc.want, action)
			}
			if rename != "" {
				t.Fatalf("lww 不应产生 rename，got %q", rename)
			}
		})
	}
}

// TestDecide_ConflictRename 验证 conflict_rename：目标改名保留，返回冲突文件名。
func TestDecide_ConflictRename(t *testing.T) {
	src := &Entry{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}
	dst := &Entry{Path: "dir/a.txt", Size: 20, MTime: 200, Checksum: "c2"}
	action, rename := Decide(ConflictRename, src, dst)
	if action != ActionConflictRenamed {
		t.Fatalf("conflict_rename 策略应返回 conflict_renamed，got %q", action)
	}
	re := regexp.MustCompile(`^dir/a\.txt\.conflict-\d+$`)
	if !re.MatchString(rename) {
		t.Fatalf("rename 目标格式应为 <dst.Path>.conflict-<unixnano>，got %q", rename)
	}
}

// TestDecide_LWW_EqualMTime_ChecksumFallback 验证 lww mtime 相等时回落 checksum
// （审查 M1：该分支在 ComputeDiff 流程中通常已被 entriesSame 提前拦截，但 Decide
// 作为纯函数仍需定义明确，供类型冲突等绕过 entriesSame 的场景使用）。
func TestDecide_LWW_EqualMTime_ChecksumFallback(t *testing.T) {
	src := &Entry{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}
	t.Run("checksum 不同 → src 胜 updated", func(t *testing.T) {
		dst := &Entry{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c2"}
		action, rename := Decide(ConflictLWW, src, dst)
		if action != ActionUpdated {
			t.Fatalf("mtime 相等且 checksum 不同应 updated，got %q", action)
		}
		if rename != "" {
			t.Fatalf("updated 不应产生 rename，got %q", rename)
		}
	})
	t.Run("checksum 相同 → skipped_conflict", func(t *testing.T) {
		dst := &Entry{Path: "a.txt", Size: 10, MTime: 100, Checksum: "c1"}
		action, _ := Decide(ConflictLWW, src, dst)
		if action != ActionSkippedConflict {
			t.Fatalf("mtime 相等且 checksum 相同应 skipped_conflict，got %q", action)
		}
	})
	t.Run("checksum 缺失 → src 胜 updated", func(t *testing.T) {
		dst := &Entry{Path: "a.txt", Size: 10, MTime: 100, Checksum: ""}
		action, _ := Decide(ConflictLWW, src, dst)
		if action != ActionUpdated {
			t.Fatalf("mtime 相等且 checksum 不可得应 src 胜 updated，got %q", action)
		}
	})
}
