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
		assertChunkSizeResult(t, fileSize, preferred, maxChunk, cs)
	})
}

// assertChunkSizeResult 覆盖率 fuzz 主体断言：返回值合规（正数、受上下限约束、
// 2 的幂倍）。拆为独立子校验以降低认知复杂度。
func assertChunkSizeResult(t *testing.T, fileSize, preferred, maxChunk, cs int64) {
	t.Helper()
	effectiveMax := calcEffectiveMax(maxChunk)
	// 返回值必须为正数
	if cs <= 0 {
		t.Errorf("calcChunkSize(%d, %d, %d) = %d, expected > 0", fileSize, preferred, maxChunk, cs)
	}
	// 返回值不能超过 maxChunk 的有效上限
	if cs > effectiveMax {
		t.Errorf("calcChunkSize(%d, %d, %d) = %d, expected <= %d", fileSize, preferred, maxChunk, cs, effectiveMax)
	}
	assertChunkSizeMinAndPowerOf(t, fileSize, preferred, maxChunk, cs, effectiveMax)
}

// calcEffectiveMax 返回 maxChunk 的有效上限（<=0 时回落到默认 64 MiB）。
func calcEffectiveMax(maxChunk int64) int64 {
	if maxChunk > 0 {
		return maxChunk
	}
	return 64 * 1024 * 1024
}

// assertChunkSizeMinAndPowerOf 校验返回值下限与 2 的幂倍约束。
func assertChunkSizeMinAndPowerOf(t *testing.T, fileSize, preferred, maxChunk, cs, effectiveMax int64) {
	t.Helper()
	// 返回值不能小于 preferred 和 maxChunk 的有效下限
	effectiveMin := calcEffectiveMin(preferred, maxChunk)
	if cs < effectiveMin {
		t.Errorf("calcChunkSize(%d, %d, %d) = %d, expected >= %d", fileSize, preferred, maxChunk, cs, effectiveMin)
	}
	// cs 必须是 effectiveMin 的 2 的幂倍（除非被 effectiveMax 截断）
	if cs >= effectiveMax || cs%effectiveMin != 0 {
		return
	}
	ratio := cs / effectiveMin
	if ratio <= 0 || (ratio&(ratio-1)) != 0 {
		t.Errorf("calcChunkSize(%d, %d, %d) = %d: expected power-of-2 multiple of %d, got ratio %d",
			fileSize, preferred, maxChunk, cs, effectiveMin, ratio)
	}
}

// calcEffectiveMin 返回 preferred/maxChunk 的有效下限（<=0 时各自回退默认值后再取 min）。
func calcEffectiveMin(preferred, maxChunk int64) int64 {
	effectivePref := preferred
	if effectivePref <= 0 {
		effectivePref = 4 * 1024 * 1024
	}
	effectiveMax := maxChunk
	if effectiveMax <= 0 {
		effectiveMax = 64 * 1024 * 1024
	}
	return min(effectivePref, effectiveMax)
}
