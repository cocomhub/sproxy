// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"math"
	"testing"
	"time"
)

func TestApdex_Boundary(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.SetApdexThreshold(500 * time.Millisecond)
	m.RecordRequestLatency(500 * time.Millisecond)          // d == T → satisfied
	m.RecordRequestLatency(2 * time.Second)                 // d == 4T → tolerated
	m.RecordRequestLatency(2*time.Second + time.Nanosecond) // d > 4T → frustrated

	if got := m.apdexSatisfied.Load(); got != 1 {
		t.Errorf("satisfied = %d, want 1", got)
	}
	if got := m.apdexTolerated.Load(); got != 1 {
		t.Errorf("tolerated = %d, want 1", got)
	}
	if got := m.apdexFrustrated.Load(); got != 1 {
		t.Errorf("frustrated = %d, want 1", got)
	}
}

func TestApdex_ScoreAndZeroTotal(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	if got := m.apdexScore(); got != 0 {
		t.Errorf("total=0 时 score = %v, want 0（不 NaN）", got)
	}
	if got := m.apdexScore(); math.IsNaN(got) {
		t.Error("total=0 时 score 不得为 NaN")
	}

	m.apdexSatisfied.Add(3)
	m.apdexTolerated.Add(2)
	m.apdexFrustrated.Add(1)
	want := (3.0 + 2.0/2) / 6.0 // (sat + tol/2) / total = 4/6
	if got := m.apdexScore(); math.Abs(got-want) > 1e-9 {
		t.Errorf("score = %v, want %v", got, want)
	}
}
