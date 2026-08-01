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
		// 返回值不能小于 preferred 和 maxChunk 的有效下限
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
		// cs 必须是 effectiveMin 的 2 的幂倍（除非被 effectiveMax 截断）
		if cs < effectiveMax && cs%effectiveMin == 0 {
			ratio := cs / effectiveMin
			if ratio <= 0 || (ratio&(ratio-1)) != 0 {
				t.Errorf("calcChunkSize(%d, %d, %d) = %d: expected power-of-2 multiple of %d, got ratio %d",
					fileSize, preferred, maxChunk, cs, effectiveMin, ratio)
			}
		}
	})
}
