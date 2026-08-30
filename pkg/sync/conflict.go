// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"fmt"
	"time"
)

// Decide 在「目标已存在且内容不同」时按策略给出动作。
// src/dst 均非 nil。返回动作 + 是否需要 rename 目标（ActionConflictRenamed 时需要 renameDstTo）。
//
// 各策略：
//   - skip：ActionSkippedConflict
//   - overwrite：ActionUpdated
//   - lww：src.MTime>dst.MTime → ActionUpdated；否则 ActionSkippedConflict；
//     mtime 相等回落 checksum（均可得时按 checksum 是否相同判定），checksum 亦不可得时 src 胜。
//   - conflict_rename：ActionConflictRenamed + renameDstTo="<dst.Path>.conflict-<unixnano>"。
func Decide(policy ConflictPolicy, src, dst *Entry) (action Action, renameDstTo string) {
	switch policy {
	case ConflictOverwrite:
		return ActionUpdated, ""
	case ConflictLWW:
		if src.MTime > dst.MTime {
			return ActionUpdated, ""
		}
		if src.MTime < dst.MTime {
			return ActionSkippedConflict, ""
		}
		// mtime 相等：回落 checksum 比较
		if src.Checksum != "" && dst.Checksum != "" {
			if src.Checksum != dst.Checksum {
				return ActionUpdated, ""
			}
			return ActionSkippedConflict, ""
		}
		// checksum 不可得：src 胜
		return ActionUpdated, ""
	case ConflictRename:
		return ActionConflictRenamed, fmt.Sprintf("%s.conflict-%d", dst.Path, time.Now().UnixNano())
	default: // ConflictSkip
		return ActionSkippedConflict, ""
	}
}
