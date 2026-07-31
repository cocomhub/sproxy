// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"math"
	"testing"
)

// FuzzCalcChunkSize 检查 calcChunkSize 在任意输入下不 panic 且返回值合规。
func FuzzCalcChunkSize(f *testing.F) {
	seeds := []struct {
		fileSize int64
		pref     int64
		maxChunk int64
	}{
		{0, 0, 0},
		{1024, 4 * 1024 * 1024, 64 * 1024 * 1024},
		{0, 0, -1},
		{-100, 4 * 1024 * 1024, 64 * 1024 * 1024},
		{math.MaxInt64, 4 * 1024 * 1024, 64 * 1024 * 1024},
	}
	for _, s := range seeds {
		f.Add(s.fileSize, s.pref, s.maxChunk)
	}

	f.Fuzz(func(t *testing.T, fileSize, preferred, maxChunk int64) {
		cs := calcChunkSize(fileSize, preferred, maxChunk)
		// 返回值必须为正数
		if cs <= 0 {
			t.Errorf("calcChunkSize(%d, %d, %d) = %d, expected > 0", fileSize, preferred, maxChunk, cs)
		}
		// 返回值不能超过 maxChunk 的有效上限
		effectiveMax := maxChunk
		if effectiveMax <= 0 {
			effectiveMax = 64 * 1024 * 1024
		}
		if cs > effectiveMax {
			t.Errorf("calcChunkSize(%d, %d, %d) = %d, expected <= %d", fileSize, preferred, maxChunk, cs, effectiveMax)
		}
		// \xe8\xbf\x94\xe5\x9b\x9e\xe5\x80\xbc\xe4\xb8\x8d\xe8\x83\xbd\xe5\xb0\x8f\xe4\xba\x8e preferred \xe5\x92\x8c maxChunk \xe7\x9a\x84\xe6\x9c\x89\xe6\x95\x88\xe4\xb8\x8b\xe9\x99\x90
		effectivePref := preferred
		if effectivePref <= 0 {
			effectivePref = 4 * 1024 * 1024
		}
		effectiveMax2 := maxChunk
		if effectiveMax2 <= 0 {
			effectiveMax2 = 64 * 1024 * 1024
		}
		effectiveMin := min(effectivePref, effectiveMax2)
		if cs < effectiveMin {
			t.Errorf("calcChunkSize(%d, %d, %d) = %d, expected >= %d", fileSize, preferred, maxChunk, cs, effectiveMin)
		}
	})
}
