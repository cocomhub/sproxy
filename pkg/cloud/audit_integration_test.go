// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestCloudTask_Audit_Collected 验证云下载/转存阶段已接入 pkg/audit：
// 下载 + 转存到本地卷的任务完成后，task.Audit 非空，且至少包含：
//   - 一行 Type=download（DurMS>=0，Bytes>0）；
//   - 一行 Type=transfer（DurMS>=0）。
//
// 加密卷转存的 Type=encrypt 行由 secretdata 包独立单测覆盖（heavy e2e 不再本测试承载）。
func TestCloudTask_Audit_Collected(t *testing.T) {
	t.Parallel()
	content := []byte("audit integration test payload 0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		if _, err := w.Write(content); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	fs := newMemFS()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 16*1024*1024, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, &CloudDownloadConfig{
		MaxConcurrent: 3,
		SyncThreshold: 20 * 1024 * 1024,
		AllowPrivate:  true,
		TaskTTL:       time.Hour,
	})
	// 转存目标 = 内存 FS（shared=false + ModeAllow 空 owner，与非空 owner 无关）。
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false }
	mgr.volumeFor = func(vol string) (volume.Volume, bool) {
		return volume.Volume{
			Name: vol, Type: "secretdata",
			ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"": {}}},
		}, true
	}

	// 同步路径：content 小於 SyncThreshold 且 syncCtx 非 nil → 下载+转存在同一 goroutine
	// 完成（deterministic，无需轮询终态）。
	task, err := mgr.SubmitAndStart("url", srv.URL, "audit.bin", int64(len(content)), t.Context(), "",
		TaskParams{Transfer: &TransferSpec{Volume: "sec"}, Save: true})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "completed" {
		t.Fatalf("expected completed, got %q", task.Status)
	}

	// 转存落盘成功是前提（否则无后续审计可断言）。自动派生 rel = <taskID>/<filename>，
	// 经 ResolveOwnerPath 加 user/ 桶前缀。
	wantRel := "user/" + task.ID + "/audit.bin"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("memFS 应收到转存产物 %s，实际 files: %v", wantRel, keys(fs.files))
	}

	// task.Audit 由 executeDownload 收尾 defer 填好后持久化——轮询等待（防时序抖动）。
	testutil.WaitFor(t, 10*time.Second, func() bool {
		snap, ok := mgr.SnapshotTask(task.ID, "")
		return ok && len(snap.Audit) > 0
	}, func() string { return "等待 task.Audit 非空" })
	snap, ok := mgr.SnapshotTask(task.ID, "")
	if !ok {
		t.Fatal("SnapshotTask not found")
	}
	if len(snap.Audit) == 0 {
		t.Fatal("task.Audit 应为空（下载+转存至少两行）")
	}

	var dlRow, trRow *audit.Row
	for i := range snap.Audit {
		r := &snap.Audit[i]
		switch r.Type {
		case audit.TypeDownload:
			if dlRow == nil {
				dlRow = r
			}
		case audit.TypeTransfer:
			if trRow == nil {
				trRow = r
			}
		}
	}
	if dlRow == nil {
		t.Fatalf("应含 Type=%q 的下载行，实际 rows: %+v", audit.TypeDownload, snap.Audit)
	}
	if dlRow.DurMS < 0 {
		t.Fatalf("download 行 DurMS 应为 >=0，实际 %d", dlRow.DurMS)
	}
	if dlRow.Bytes <= 0 {
		t.Fatalf("download 行 Bytes 应 >0，实际 %d", dlRow.Bytes)
	}
	if trRow == nil {
		t.Fatalf("应含 Type=%q 的转存行，实际 rows: %+v", audit.TypeTransfer, snap.Audit)
	}
	if trRow.DurMS < 0 {
		t.Fatalf("transfer 行 DurMS 应为 >=0，实际 %d", trRow.DurMS)
	}
}
