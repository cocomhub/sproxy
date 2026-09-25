// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"fmt"
)

// FileStatus 是导入/导出中单个文件的处置分类（防假成功：checksum 校验失败
// 绝不标成功，对齐 via-direct 假成功教训）。
type FileStatus int

const (
	// StatusPending 表示尚未处置（目标不存在 → 待上传）。
	StatusPending FileStatus = iota
	// StatusImported 表示已成功上传并通过校验。
	StatusImported
	// StatusSkipped 表示目标已存在且 checksum 相同（幂等跳过）。
	StatusSkipped
	// StatusConflict 表示目标已存在但 checksum 不同（须人工处置）。
	StatusConflict
	// StatusFailed 表示上传/校验失败。
	StatusFailed
)

func (s FileStatus) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusImported:
		return "imported"
	case StatusSkipped:
		return "skipped"
	case StatusConflict:
		return "conflict"
	case StatusFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// ClassifyConflict 判断目标文件处置分类：
// 目标不存在 → Pending（待上传）；已存在且 checksum 相同 → Skipped（幂等）；
// 已存在但 checksum 不同 → Conflict（报错，绝不静默覆盖）。
func ClassifyConflict(exists, checksumEqual bool) FileStatus {
	if !exists {
		return StatusPending
	}
	if checksumEqual {
		return StatusSkipped
	}
	return StatusConflict
}

// FileFailure 是单个文件失败记录（含原因，供汇总报告）。
type FileFailure struct {
	Name string `json:"name"`
	Err  string `json:"error"`
}

// ImportSummary 汇总导入结果（计数/字节/失败清单）。
type ImportSummary struct {
	Imported      int   `json:"imported"`
	Skipped       int   `json:"skipped"`
	Conflict      int   `json:"conflict"`
	Failed        int   `json:"failed"`
	ImportedBytes int64 `json:"imported_bytes"`
	SkippedBytes  int64 `json:"skipped_bytes"`

	// IgnoreErrors 为 true 时失败不阻塞整体 Ok()（--ignore-errors）。
	IgnoreErrors bool          `json:"-"`
	Failures     []FileFailure `json:"failures,omitempty"`
}

// Add 累计一个处置结果（冲突/失败不计字节）。
func (s *ImportSummary) Add(status FileStatus, size int64) {
	switch status {
	case StatusImported:
		s.Imported++
		s.ImportedBytes += size
	case StatusSkipped:
		s.Skipped++
		s.SkippedBytes += size
	case StatusConflict:
		s.Conflict++
	case StatusFailed:
		s.Failed++
	}
}

// RecordFailure 记录单文件失败（含原因）。
func (s *ImportSummary) RecordFailure(name string, err error) {
	s.Failures = append(s.Failures, FileFailure{Name: name, Err: err.Error()})
}

// Ok 报告整体是否成功：无失败，或 --ignore-errors 下失败被容忍。
func (s *ImportSummary) Ok() bool {
	return s.Failed == 0 || s.IgnoreErrors
}

// SummaryError 返回汇总错误（非零退出用）；无失败返回 nil。
// 含冲突时包装 ErrConflict 哨兵（调用方 errors.Is 精确判定）。
func (s *ImportSummary) SummaryError() error {
	if s.Ok() {
		return nil
	}
	if s.Conflict > 0 {
		return fmt.Errorf("%d 个文件失败（含 %d 冲突）: %w", s.Failed, s.Conflict, ErrConflict)
	}
	return fmt.Errorf("%d 个文件失败；失败清单见报告", s.Failed)
}
