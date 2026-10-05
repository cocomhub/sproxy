// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

// TestCloudTask_Audit_Collected 验证云下载/转存阶段已接入 pkg/audit：
// 下载 + 转存到本地卷的任务完成后，task.Audit 非空，且至少包含：
//   - 一行 Type=download（DurMS>=0，Bytes>0）；
//   - 一行 Type=transfer（DurMS>=0）。
//
// 转存到真实 secretdata 加密卷的 Type=encrypt 行（明密文对比 + 算法档位）由
// TestCloudTask_Audit_EncryptRow_SecretVolume 覆盖（本测试用内存 FS 验证下载/转存
// 阶段的审计链路本身）。
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
	if dlRow.DurMS < 0 || dlRow.DurMS >= 60000 {
		t.Fatalf("download 行 DurMS 应为 [0,60000)ms 量级（buggy 的 Begin 未初始化 start 会溢出为 ~292 年），实际 %d", dlRow.DurMS)
	}
	if dlRow.Bytes <= 0 {
		t.Fatalf("download 行 Bytes 应 >0，实际 %d", dlRow.Bytes)
	}
	if trRow == nil {
		t.Fatalf("应含 Type=%q 的转存行，实际 rows: %+v", audit.TypeTransfer, snap.Audit)
	}
	if trRow.DurMS < 0 || trRow.DurMS >= 60000 {
		t.Fatalf("transfer 行 DurMS 应为 [0,60000)ms 量级（buggy 的 Begin 未初始化 start 会溢出为 ~292 年），实际 %d", trRow.DurMS)
	}
}

// TestCloudTask_Audit_EncryptRow_SecretVolume 验证转存到**真实 secretdata 加密卷**的任务
// 会在 task.Audit 中产出 Type=encrypt 行（明密文对比埋点）：Bytes>0（明文体积）、Meta 含
// cipher_bytes>0（密文体积）、encrypted==true 与 algorithm（算法档位标识，非空）。转存目标
// 用真 secretdata wrapper（内层 memFS），写路径触发真实分块加密 → 端到端锁定「转存到加密
// 卷 → 审计含加密行」的路径；并读回校验转存产物内容一致（真实副作用）。
func TestCloudTask_Audit_EncryptRow_SecretVolume(t *testing.T) {
	t.Parallel()
	content := []byte("secretdata audit encrypt integration payload 0123456789")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		if _, err := w.Write(content); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	inner := newMemFS()
	secretFS, err := secretdata.NewFS(inner, secretdata.Options{
		Secret:  []byte("test-secret-key-000"),
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 16*1024*1024, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, &CloudDownloadConfig{
		MaxConcurrent: 3,
		SyncThreshold: 20 * 1024 * 1024,
		AllowPrivate:  true,
		TaskTTL:       time.Hour,
	})
	// 转存目标 = 真实 secretdata 卷（独享 + ModeAllow 空 owner，与非空 owner 无关）。
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string, bool) { return secretFS, "secretdata", false }
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

	// 真实副作用：转存产物可经 secretdata 卷读回且内容一致（wrapper 分块加密/解密往返）。
	wantRel := "user/" + task.ID + "/audit.bin"
	rc, oerr := secretFS.OpenRead(t.Context(), wantRel)
	if oerr != nil {
		t.Fatalf("secretdata 卷应可读回 %s: %v", wantRel, oerr)
	}
	got, rerr := io.ReadAll(rc)
	rc.Close()
	if rerr != nil {
		t.Fatalf("read back: %v", rerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("读回内容应与上传明文一致，got %q want %q", got, content)
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

	var encRow *audit.Row
	for i := range snap.Audit {
		r := &snap.Audit[i]
		if r.Type == audit.TypeEncrypt && encRow == nil {
			encRow = r
		}
	}
	if encRow == nil {
		t.Fatalf("应含 Type=%q 行（转存目标为 secretdata 加密卷），实际 rows: %+v", audit.TypeEncrypt, snap.Audit)
	}
	if encRow.DurMS < 0 || encRow.DurMS >= 60000 {
		t.Fatalf("encrypt 行 DurMS 应为 [0,60000)ms 量级，实际 %d", encRow.DurMS)
	}
	if encRow.Bytes != int64(len(content)) {
		t.Fatalf("encrypt 行 Bytes 应为明文体积 %d，实际 %d", len(content), encRow.Bytes)
	}
	meta, ok := encRow.Meta.(map[string]any)
	if !ok {
		t.Fatalf("encrypt Meta 应为 map，got %T", encRow.Meta)
	}
	if meta["encrypted"] != true {
		t.Fatalf("encrypt meta.encrypted 应为 true，got %v", meta["encrypted"])
	}
	if cb, ok := meta["cipher_bytes"].(float64); !ok || cb <= 0 {
		t.Fatalf("encrypt meta.cipher_bytes 应 >0，got %v (%T)", meta["cipher_bytes"], meta["cipher_bytes"])
	}
	if alg, ok := meta["algorithm"].(string); !ok || alg == "" {
		t.Fatalf("encrypt meta.algorithm 应非空，got %v (%T)", meta["algorithm"], meta["algorithm"])
	}
}
