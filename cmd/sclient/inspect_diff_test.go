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

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func TestInspectHasDiff(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []syncpkg.Anomaly
		want bool
	}{
		{"空", nil, false},
		{"仅上传中", []syncpkg.Anomaly{{Code: syncpkg.AnomalyUploadInProgress}}, false},
		{"仅去重共享", []syncpkg.Anomaly{{Code: syncpkg.AnomalyDedupShared}}, false},
		{"上传中+去重共享", []syncpkg.Anomaly{
			{Code: syncpkg.AnomalyUploadInProgress}, {Code: syncpkg.AnomalyDedupShared},
		}, false},
		{"缺失 blob", []syncpkg.Anomaly{{Code: syncpkg.AnomalyBlobMissing}}, true},
		{"孤儿 blob", []syncpkg.Anomaly{{Code: syncpkg.AnomalyBlobOrphan}}, true},
		{"大小不符", []syncpkg.Anomaly{{Code: syncpkg.AnomalySizeMismatch}}, true},
		{"信息码+真实差异", []syncpkg.Anomaly{
			{Code: syncpkg.AnomalyUploadInProgress}, {Code: syncpkg.AnomalyBlobMissing},
		}, true},
	}
	for _, c := range cases {
		if got := inspectHasDiff(c.in); got != c.want {
			t.Errorf("%s: inspectHasDiff = %v, want %v", c.name, got, c.want)
		}
	}
}
