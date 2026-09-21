// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"fmt"
)

// ActionVerifyFailed 表示校验核对失败：目标文件 checksum 与源不一致（传输/落盘损坏）。
const ActionVerifyFailed Action = "verify_failed"

// Verify 校验核对：对 job.Results 中 created/updated/conflict_renamed 的目标文件
// 逐一重读校验和，与源条目 checksum 比对。不一致的记录为 ActionVerifyFailed。
//
// 校验是**显式启用**的能力（job.VerifyAfter=true 时由调用方在 Sync 后调用），
// 默认关闭零回归：Verify 只处理调用方传入的 results，不做任何写入。
//
// 语义：
//   - 逐文件独立校验，单个失败不中断（错误聚合到返回值）；
//   - ctx 取消时返回剩余文件前的结果（可中断）；
//   - 目标 Stat 失败（文件缺失）视为校验失败（fail-closed）；
//   - 源 checksum 为空（远程 FS 未提供）时跳过该校验（无法比对不误报）。
//
// 返回校验失败清单（ActionVerifyFailed 结果，path 为目标侧路径）。
func Verify(ctx context.Context, src, dst FS, job *Job) ([]FileResult, error) {
	_ = job.VerifyAfter // 变异：忽略开关
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var failures []FileResult
	for i := range job.Results {
		if err := ctx.Err(); err != nil {
			return failures, err
		}
		r := &job.Results[i]
		switch r.Action {
		case ActionCreated, ActionUpdated, ActionConflictRenamed:
			// 源 checksum 缺失（如远程 ListDir 未提供）无法比对，跳过（不误报）。
			if r.Checksum == "" {
				continue
			}
			dstEntry, err := dst.Stat(ctx, r.Path)
			if err != nil {
				failures = append(failures, FileResult{Path: r.Path, Action: ActionVerifyFailed,
					Error: fmt.Sprintf("校验 stat 目标失败: %v", err)})
				continue
			}
			if dstEntry == nil {
				failures = append(failures, FileResult{Path: r.Path, Action: ActionVerifyFailed,
					Error: "校验失败：目标文件不存在"})
				continue
			}
			if dstEntry.Checksum != r.Checksum {
				failures = append(failures, FileResult{Path: r.Path, Action: ActionVerifyFailed,
					Error: fmt.Sprintf("校验失败：checksum 不一致（目标 %q ≠ 源 %q）", dstEntry.Checksum, r.Checksum)})
			}
		}
	}
	return failures, nil
}

// Summary 汇总一次同步的结果（按 Action 分组计数）。
// 供 CLI 报告输出与任务持久化统计。
type Summary struct {
	Created      int64 // 新文件
	Updated      int64 // 覆盖/更新
	Skipped      int64 // 相同跳过
	Conflict     int64 // 冲突跳过（skipped_conflict + conflict_renamed）
	SkippedSym   int64 // 符号链接跳过
	Deleted      int64 // 删除传播删除
	Errors       int64 // 错误
	VerifyFailed int64 // 校验失败
}

// SummaryOf 按 Action 分组统计 results。
func SummaryOf(results []FileResult) Summary {
	var s Summary
	for _, r := range results {
		switch r.Action {
		case ActionCreated:
			s.Created++
		case ActionUpdated:
			s.Updated++
		case ActionSkipped:
			s.Skipped++
		case ActionSkippedConflict, ActionConflictRenamed:
			s.Conflict++
		case ActionSkippedSymlink:
			s.SkippedSym++
		case ActionDeleted:
			s.Deleted++
		case ActionError:
			s.Errors++
		case ActionVerifyFailed:
			s.VerifyFailed++
		}
	}
	return s
}
