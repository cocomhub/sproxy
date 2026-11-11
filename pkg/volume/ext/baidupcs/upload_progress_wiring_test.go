// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// upload_progress_wiring_test.go 锁定二期 T3（2026-11-10）：**远端卷上传阶段必须上报进度**。
// 起点 0 / 终点 size（明文口径，与加密阶段一致，便于 UI 分区展示）。
//
// 变异验证：删掉 uploadStaged 里的两处 ReportUploadProgress ⇒ 本用例红。

import (
	"context"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestUploadStaged_ReportsRemoteProgress 远端阶段上报契约：至少 2 次（起点 0 + 终点 size），
// stage 全是 remote，最终 done == size。
func TestUploadStaged_ReportsRemoteProgress(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var donts []int64
	var total int64
	allRemote := true

	ctx := syncpkg.WithUploadProgress(context.Background(),
		func(stage syncpkg.UploadStage, done, tot int64) {
			mu.Lock()
			defer mu.Unlock()
			if stage != syncpkg.StageRemote {
				allRemote = false
			}
			total = tot
			donts = append(donts, done)
		})

	ad := newGatedAdapter()
	st := newGateStorage(t, ad, 1)
	const size = 4096
	if err := st.uploadStaged(ctx, "d/a.bin", "/r/d/a.bin",
		&stagedUpload{tmpPath: writeTemp(t, size), size: size, md5: strings.Repeat("a", 32)}); err != nil {
		t.Fatalf("uploadStaged: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(donts) < 2 {
		t.Fatalf("远端阶段至少应上报 2 次（起点+终点），got %v", donts)
	}
	if !allRemote {
		t.Fatal("上报的 stage 必须全是 remote")
	}
	if total != size {
		t.Fatalf("total 应为 %d，got %d", size, total)
	}
	if donts[0] != 0 || donts[len(donts)-1] != size {
		t.Fatalf("应从 0 报到 %d，got %v", size, donts)
	}
}
