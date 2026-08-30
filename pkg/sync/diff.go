// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"errors"
	"fmt"
)

// DiffEntry 表示单个条目的差异决策。
type DiffEntry struct {
	Path        string
	Action      Action
	Src, Dst    *Entry
	RenameDstTo string
	// Err 为 dstStat 失败时的原始错误（Action=ActionError 时非 nil）。
	// 规格未列出该字段，但 ActionError 结果需要携带错误文本，故补充。
	Err error
}

// ComputeDiff 纯函数：对每个 src 条目调用 dstStat 得出差异动作。
//
//   - dstStat 返回 (nil, nil) 表示目标不存在 → ActionCreated；
//   - dstStat 返回 error → ActionError（Err 保留原始错误），不中止其余条目；
//   - 目标存在且相同 → ActionSkipped；不同 → Decide(policy,...)。
//
// 任一 dstStat 失败时返回聚合的非 nil error，但其余条目照常产出。
func ComputeDiff(srcEntries []Entry, dstStat func(path string) (*Entry, error), policy ConflictPolicy) ([]DiffEntry, error) {
	diffs := make([]DiffEntry, 0, len(srcEntries))
	var errs []error
	for i := range srcEntries {
		src := srcEntries[i]
		dst, err := dstStat(src.Path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Path, err))
			diffs = append(diffs, DiffEntry{Path: src.Path, Action: ActionError, Src: &src, Err: err})
			continue
		}
		if dst == nil {
			diffs = append(diffs, DiffEntry{Path: src.Path, Action: ActionCreated, Src: &src})
			continue
		}
		if src.IsDir && dst.IsDir {
			// 目录结构由枚举递归处理，两侧都是目录视为相同（不做内容 diff）
			diffs = append(diffs, DiffEntry{Path: src.Path, Action: ActionSkipped, Src: &src, Dst: dst})
			continue
		}
		if src.IsDir != dst.IsDir {
			// 类型冲突（文件 vs 目录）：不进入 entriesSame（只看 size/checksum/mtime，
			// 目录与文件 Size/MTime 巧合相等会被误判为相同而漏同步），显式按策略 Decide
			// （审查 I-4）。skip → skipped_conflict；overwrite/lww → updated（Engine 对
			// src 文件/dst 目录的 updated 会拒绝覆盖，见 engine.syncFile）；rename → 冲突改名。
			action, rename := Decide(policy, &src, dst)
			diffs = append(diffs, DiffEntry{Path: src.Path, Action: action, Src: &src, Dst: dst, RenameDstTo: rename})
			continue
		}
		if entriesSame(&src, dst) {
			diffs = append(diffs, DiffEntry{Path: src.Path, Action: ActionSkipped, Src: &src, Dst: dst})
			continue
		}
		action, rename := Decide(policy, &src, dst)
		diffs = append(diffs, DiffEntry{Path: src.Path, Action: action, Src: &src, Dst: dst, RenameDstTo: rename})
	}
	return diffs, errors.Join(errs...)
}

// entriesSame 判定两条目内容相同：
// size 相同 且（checksum 均可得则 checksum 相同；checksum 缺失则 mtime 相同）。
func entriesSame(src, dst *Entry) bool {
	if src.Size != dst.Size {
		return false
	}
	if src.Checksum != "" && dst.Checksum != "" {
		return src.Checksum == dst.Checksum
	}
	return src.MTime == dst.MTime
}
