// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// inspect_diff_test.go 锁「退出码 3 = 真实存储差异」的判定口径。
//
// 真实缺陷（2026-11-05 审查）：此前 `len(anomalies) > 0` ⇒ 上传中（upload_in_progress）
// 和去重共享（dedup_shared）也会退 3，脚本无法区分「有差异」与「正常的中间态」；
// 且 JSON 分支提前 return，同一份数据文本退 3、JSON 退 0（模式不一致）。

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

func TestInspectHasDiff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []volume.Anomaly
		want bool
	}{
		{"空", nil, false},
		{"仅上传中", []volume.Anomaly{{Code: volume.AnomalyUploadInProgress}}, false},
		{"仅去重共享", []volume.Anomaly{{Code: volume.AnomalyDedupShared}}, false},
		{"上传中+去重共享", []volume.Anomaly{
			{Code: volume.AnomalyUploadInProgress}, {Code: volume.AnomalyDedupShared},
		}, false},
		{"缺失 blob", []volume.Anomaly{{Code: volume.AnomalyBlobMissing}}, true},
		{"孤儿 blob", []volume.Anomaly{{Code: volume.AnomalyBlobOrphan}}, true},
		{"大小不符", []volume.Anomaly{{Code: volume.AnomalySizeMismatch}}, true},
		{"信息码+真实差异", []volume.Anomaly{
			{Code: volume.AnomalyUploadInProgress}, {Code: volume.AnomalyBlobMissing},
		}, true},
	}
	for _, c := range cases {
		if got := inspectHasDiff(c.in); got != c.want {
			t.Errorf("%s: inspectHasDiff = %v, want %v", c.name, got, c.want)
		}
	}
}
