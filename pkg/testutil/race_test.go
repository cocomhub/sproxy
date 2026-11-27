// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

// race_test.go 锁「-race 下等待窗口被放大，非 race 下保持原值」。
//
// 这条语义很重要：若某天有人把 ScaleTimeout 改成"总是放大"，非 race job 会失去对
// 真泄漏/真超时的检出能力（全变宽松）；若改成"总是不放大"，全包 -race 的负载型 flake 回归。

import (
	"testing"
	"time"
)

// TestScaleTimeout_RaceAware 断言放大语义与 -race 构建一致。
func TestScaleTimeout_RaceAware(t *testing.T) {
	t.Parallel()
	const base = 5 * time.Second
	got := ScaleTimeout(base)
	if RaceEnabled {
		if got <= base {
			t.Fatalf("-race 构建下窗口应被放大，得到 %s（基准 %s）", got, base)
		}
		if got != base*raceScale {
			t.Fatalf("-race 构建下窗口应为 %s×%d，得到 %s", base, raceScale, got)
		}
		return
	}
	if got != base {
		t.Fatalf("非 race 构建下窗口必须保持原值，得到 %s（基准 %s）", got, base)
	}
}

// TestScaleTimeout_ZeroAndNegative 零/负值原样返回（调用方语义不被打乱）。
func TestScaleTimeout_ZeroAndNegative(t *testing.T) {
	t.Parallel()
	if got := ScaleTimeout(0); got != 0 {
		t.Fatalf("0 应原样返回，得到 %s", got)
	}
	if got := ScaleTimeout(-time.Second); got != -time.Second {
		t.Fatalf("负值应原样返回，得到 %s", got)
	}
}
