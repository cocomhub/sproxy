// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"errors"
	"strings"
	"testing"
)

// TestClassifyConflict 断言冲突分类：
// 目标已存在且 checksum 相同 → SKIPPED（幂等）；不同 → CONFLICT。
func TestClassifyConflict(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		exist   bool
		csEqual bool
		want    FileStatus
	}{
		{name: "不存在", exist: false, want: StatusPending},
		{name: "存在且相同", exist: true, csEqual: true, want: StatusSkipped},
		{name: "存在且不同", exist: true, csEqual: false, want: StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ClassifyConflict(tc.exist, tc.csEqual); got != tc.want {
				t.Errorf("ClassifyConflict(%v,%v) = %v, want %v", tc.exist, tc.csEqual, got, tc.want)
			}
		})
	}
}

// TestImportSummary 断言汇总统计：计数/字节数与失败聚合。
func TestImportSummary(t *testing.T) {
	t.Parallel()
	s := &ImportSummary{}
	s.Add(StatusImported, 5)
	s.Add(StatusSkipped, 2)
	s.Add(StatusSkipped, 3)
	s.Add(StatusConflict, 7)
	s.Add(StatusFailed, 9)
	if s.Imported != 1 || s.Skipped != 2 || s.Conflict != 1 || s.Failed != 1 {
		t.Errorf("计数不一致: %+v", s)
	}
	if s.ImportedBytes != 5 || s.SkippedBytes != 5 {
		t.Errorf("字节不一致: %+v", s)
	}
	s.RecordFailure("a.txt", errors.New("boom"))
	if len(s.Failures) != 1 || s.Failures[0].Name != "a.txt" || !strings.Contains(s.Failures[0].Err, "boom") {
		t.Errorf("失败清单记录不一致: %+v", s.Failures)
	}
	if s.Ok() {
		t.Error("含失败应 Ok()=false")
	}
	s2 := &ImportSummary{}
	if !s2.Ok() {
		t.Error("空汇总应 Ok()=true")
	}
}

// TestImportSummary_IgnoreErrors 断言 --ignore-errors 下失败不阻塞 Ok()。
func TestImportSummary_IgnoreErrors(t *testing.T) {
	t.Parallel()
	s := &ImportSummary{}
	s.RecordFailure("a.txt", errors.New("boom"))
	s.IgnoreErrors = true
	if !s.Ok() {
		t.Error("IgnoreErrors 时失败不应阻塞 Ok()")
	}
}
