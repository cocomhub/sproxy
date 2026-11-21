// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import "testing"

// TestRecordCloudTaskFailedBucketsByReason 锁定「按错误类别分桶」（P5 成功率治理输入）。
func TestRecordCloudTaskFailedBucketsByReason(t *testing.T) {
	t.Parallel()
	m := NewMetrics()
	m.RecordCloudTaskFailed("source_unreachable")
	m.RecordCloudTaskFailed("source_unreachable")
	m.RecordCloudTaskFailed("throttled")
	m.RecordCloudTaskFailed("") // 空 ⇒ unknown
	samples := m.CloudTaskFailedSamples()
	got := map[string]int64{}
	for _, s := range samples {
		got[s.Labels] = s.Value
	}
	if got[`reason="source_unreachable"`] != 2 || got[`reason="throttled"`] != 1 || got[`reason="unknown"`] != 1 {
		t.Fatalf("分桶错误: %v", got)
	}
}
