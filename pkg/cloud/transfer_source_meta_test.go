// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// TestTransferSourceMeta_RecordsSourceAndExtra 转存成功后目标卷 meta Extra 记录来源：
// 客户端 meta_extra 合并 + source=transfer + source_url=task.URL + etag。
func TestTransferSourceMeta_RecordsSourceAndExtra(t *testing.T) {
	t.Parallel()
	fs := newMetaCapableMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{
		ID:        "task-src-1",
		Filename:  "src1.mp4",
		URL:       "https://example.com/share?spx_target=a.mp4",
		Transfer:  &TransferSpec{Volume: "vol-src"},
		MetaExtra: map[string]any{"gcid": "GCID123", "share_url": "https://ks/a"},
	}
	dest := filepath.Join(t.TempDir(), "src1.mp4")
	_ = os.WriteFile(dest, []byte("hello source meta"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	tr, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{
		ETag:                   `"abc-123"`,
		Checksum:               sha256Hex([]byte("hello source meta")),
		ChecksumAlgorithm:      downloader.IntegrityChecksumSHA256,
		AuthorityHash:          "GCID-ABC",
		AuthorityHashAlgorithm: downloader.IntegrityAlgPikpakGCID,
	}, nil)
	if err != nil || tr == nil {
		t.Fatalf("transferDone: err=%v tr=%v", err, tr)
	}
	// 卷内 meta extra 应含来源信息 + 客户端 meta_extra
	got := fs.metaExtras["anonymous/user/task-src-1/src1.mp4"]
	if got == nil {
		t.Fatalf("未写 meta extra（fs.metaExtras=%v）", fs.metaExtras)
	}
	if got["source"] != "transfer" {
		t.Errorf("meta.source = %v, want transfer", got["source"])
	}
	if got["source_url"] != task.URL {
		t.Errorf("meta.source_url = %v, want %q", got["source_url"], task.URL)
	}
	if got["etag"] != `"abc-123"` {
		t.Errorf("meta.etag = %v, want %q", got["etag"], `"abc-123"`)
	}
	// 算法标识来自下载方声明（不硬编码推断）
	if got["checksum_algorithm"] != downloader.IntegrityChecksumSHA256 {
		t.Errorf("meta.checksum_algorithm = %v, want %q（下载方声明）", got["checksum_algorithm"], downloader.IntegrityChecksumSHA256)
	}
	if got["authority_hash_algorithm"] != downloader.IntegrityAlgPikpakGCID {
		t.Errorf("meta.authority_hash_algorithm = %v, want %q", got["authority_hash_algorithm"], downloader.IntegrityAlgPikpakGCID)
	}
	if got["authority_hash"] != "GCID-ABC" {
		t.Errorf("meta.authority_hash = %v", got["authority_hash"])
	}
	if got["gcid"] != "GCID123" {
		t.Errorf("meta.gcid = %v, want GCID123（客户端 meta_extra 合并）", got["gcid"])
	}
	if got["share_url"] != "https://ks/a" {
		t.Errorf("meta.share_url = %v（客户端 meta_extra 合并）", got["share_url"])
	}
}

// TestTransferSourceMeta_NoMetaExtra 未传 meta_extra → 仍写 source/source_url/etag。
func TestTransferSourceMeta_NoMetaExtra(t *testing.T) {
	t.Parallel()
	fs := newMetaCapableMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-src-2", Filename: "src2.mp4", URL: "https://example.com/u",
		Transfer: &TransferSpec{Volume: "vol-src"}}
	dest := filepath.Join(t.TempDir(), "src2.mp4")
	_ = os.WriteFile(dest, []byte("no extra"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	if _, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{ETag: "etag2"}, nil); err != nil {
		t.Fatalf("transferDone: %v", err)
	}
	got := fs.metaExtras["anonymous/user/task-src-2/src2.mp4"]
	if got == nil {
		t.Fatalf("未写 meta extra")
	}
	if got["source"] != "transfer" || got["source_url"] != task.URL || got["etag"] != "etag2" {
		t.Errorf("meta = %v, want source=transfer + source_url + etag", got)
	}
}

// TestCreateTask_TransferTargetPreExisting_AllowsCreate 创建时目标已存在（同内容幂等）→
// 允许创建（转存阶段幂等命中直接成功；不同内容则由 writeTransferOnce 拒绝）。
// 本测试只验证创建**不因目标已存在而拒绝**。
func TestCreateTask_TransferTargetPreExisting_AllowsCreate(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	fs.files["anonymous/user/pre/exist.mp4"] = []byte("existing content")
	sm := capacity.NewStorageManager(t.TempDir(), 0, nil, testLogger())
	mgr, _ := newCloudTestManager(t, t.TempDir(), sm, &CloudDownloadConfig{MaxConcurrent: 3, TaskTTL: time.Hour})
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string) { return fs, "secretdata" }
	mgr.volumeFor = func(vol string) (volume.Volume, bool) {
		return volume.Volume{Name: vol, Type: "secretdata", ACL: volume.ACL{Mode: volume.ModeDeny}}, true
	}
	t.Cleanup(mgr.Close)
	_, err := mgr.CreateTask("url", "https://example.com/u", "exist.mp4", 100, "", TaskParams{
		Transfer: &TransferSpec{Volume: "vol-pre", Path: "pre/exist.mp4"},
		Save:     true,
	})
	if err != nil {
		t.Fatalf("CreateTask 目标已存在不应拒绝（转存时判定），err=%v", err)
	}
}

// TestTransferDone_DifferentContent_Rejects 目标已存在但内容不一致 → 拒绝覆写。
func TestTransferDone_DifferentContent_Rejects(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	fs.files["anonymous/user/diff/diff.mp4"] = []byte("old content")
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-diff-1", Filename: "diff.mp4", Transfer: &TransferSpec{Volume: "vol-diff", Path: "diff/diff.mp4"}}
	dest := filepath.Join(t.TempDir(), "diff.mp4")
	_ = os.WriteFile(dest, []byte("new content"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	_, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Checksum: sha256Hex([]byte("new content"))}, nil)
	if err == nil {
		t.Fatal("不同内容应拒绝覆写")
	}
	if !errors.Is(err, ErrTransferTarget) {
		t.Fatalf("应归 ErrTransferTarget，got %v", err)
	}
	if string(fs.files["anonymous/user/diff/diff.mp4"]) != "old content" {
		t.Fatal("卷内内容应保持旧值（未被覆盖）")
	}
}

// ---- 测试辅助 ----

// sha256Hex 返回 b 的 sha256 hex。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// metaCapableMemFS 在 memFS 基础上实现 UpdateMetaExtra（记录 meta extra）。
type metaCapableMemFS struct {
	*memFS
	metaExtras map[string]map[string]any
}

func newMetaCapableMemFS() *metaCapableMemFS {
	return &metaCapableMemFS{memFS: newMemFS(), metaExtras: map[string]map[string]any{}}
}

func (m *metaCapableMemFS) UpdateMetaExtra(_ context.Context, rel string, extra map[string]any) error {
	m.metaExtras[strings.TrimPrefix(rel, "/")] = extra
	return nil
}

// TestCreateTask_PersistsMetaExtra CreateTask 应把 params.MetaExtra 填入任务
// （此前遗漏：只加字段没赋值 → env.task.MetaExtra 恒 nil → 客户端 extra 不落卷）。
func TestCreateTask_PersistsMetaExtra(t *testing.T) {
	t.Parallel()
	sm := capacity.NewStorageManager(t.TempDir(), 0, nil, testLogger())
	mgr, _ := newCloudTestManager(t, t.TempDir(), sm, &CloudDownloadConfig{MaxConcurrent: 3, TaskTTL: time.Hour})
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string) { return newMemFS(), "secretdata" }
	mgr.volumeFor = func(vol string) (volume.Volume, bool) {
		return volume.Volume{Name: vol, Type: "secretdata", ACL: volume.ACL{Mode: volume.ModeDeny}}, true
	}
	t.Cleanup(mgr.Close)
	task, err := mgr.CreateTask("url", "https://example.com/u", "u.mp4", 100, "", TaskParams{
		Transfer:  &TransferSpec{Volume: "v", Path: "u.mp4"},
		Save:      true,
		MetaExtra: map[string]any{"gcid": "GCID-X"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.MetaExtra == nil || task.MetaExtra["gcid"] != "GCID-X" {
		t.Fatalf("task.MetaExtra = %v, want {gcid:GCID-X}", task.MetaExtra)
	}
}

// TestTransferSourceMeta_NoAlgorithmDeclared 下载方未声明算法 → 不写 algorithm 字段
// （用户裁定 2026-10-28：算法由下载方提供，转存层不推断/不无脑写 GCID）。
func TestTransferSourceMeta_NoAlgorithmDeclared(t *testing.T) {
	t.Parallel()
	fs := newMetaCapableMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-alg-1", Filename: "alg1.mp4", URL: "https://example.com/u",
		Transfer: &TransferSpec{Volume: "vol-alg"}}
	dest := filepath.Join(t.TempDir(), "alg1.mp4")
	_ = os.WriteFile(dest, []byte("no alg"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()
	// Result 无算法声明（模拟旧下载器；Checksum 须与 dest 内容一致避免走重下路径）
	realSum := sha256Hex([]byte("no alg"))
	if _, _, err := mgr.transferDone(context.Background(), task, dest,
		&downloader.Result{Checksum: realSum, AuthorityHash: "GH"}, nil); err != nil {
		t.Fatalf("transferDone: %v", err)
	}
	got := fs.metaExtras["anonymous/user/task-alg-1/alg1.mp4"]
	if got == nil {
		t.Fatal("未写 meta extra")
	}
	if _, exists := got["checksum_algorithm"]; exists {
		t.Errorf("未声明算法时不应写 checksum_algorithm（不推断）: %v", got["checksum_algorithm"])
	}
	if _, exists := got["authority_hash_algorithm"]; exists {
		t.Errorf("未声明算法时不应写 authority_hash_algorithm（禁无脑 GCID）: %v", got["authority_hash_algorithm"])
	}
}

// TestTransferWrite_PreservesNanoMTime 转存写入目标卷的 mtime 必须是 **UnixNano**
// （FS 契约），且优先取下载结果里的源 mtime——回归：此前传 st.ModTime().Unix()（秒），
// secretdata 存成 1.79s after epoch → `/api/files/stat` 的 mod_time 变成 1970 值。
func TestTransferWrite_PreservesNanoMTime(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-mtime-1", Filename: "m1.mp4", URL: "https://example.com/u",
		Transfer: &TransferSpec{Volume: "vol-mtime"}}
	dest := filepath.Join(t.TempDir(), "m1.mp4")
	_ = os.WriteFile(dest, []byte("mtime check"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	srcTime := time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC)
	if _, _, err := mgr.transferDone(context.Background(), task, dest,
		&downloader.Result{Checksum: sha256Hex([]byte("mtime check")), ModTime: srcTime}, nil); err != nil {
		t.Fatalf("transferDone: %v", err)
	}
	// memFS 记录写入的 mtime（UnixNano 契约）
	got := fs.mtimes["anonymous/user/task-mtime-1/m1.mp4"]
	if got != srcTime.UnixNano() {
		t.Fatalf("写入 mtime = %d (UnixNano)，want %d（源文件 mtime 纳秒）", got, srcTime.UnixNano())
	}
	if got < 1_000_000_000_000_000_000 { // 2001 年后的纳秒下界
		t.Fatalf("mtime 明显是秒单位（%d）而非纳秒——单位 bug 回归", got)
	}
}

// TestTransferSourceMeta_DownloadAudit 验证下载审计信息写入目标卷 meta：
// 源文件时间、起止/耗时、分通道字节、带宽（全部来自 Result 实测/带外字段）。
// 回归守卫：此前 meta 只有 source/checksum/transferred_at，无法审计下载耗时与分源字节。
func TestTransferSourceMeta_DownloadAudit(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 10, 28, 2, 0, 0, 0, time.UTC)
	finished := started.Add(90 * time.Second)
	created := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	modified := time.Date(2026, 10, 21, 4, 5, 6, 0, time.UTC)
	res := &downloader.Result{
		Size: 300, Checksum: "abc", ChecksumAlgorithm: downloader.IntegrityChecksumSHA256,
		ModTime: modified, SourceCreatedTime: created,
		StartedAt: started, FinishedAt: finished,
		AnonymousBytes: 100, AccountBytes: 200, BytesPerSecond: 3,
	}
	task := &CloudTask{URL: "https://example.com/share"}
	meta := buildTransferSourceMeta(task, "", "abc", "", downloader.IntegrityChecksumSHA256, "")
	applyDownloadAudit(meta, res)

	checks := map[string]any{
		"x_source_created_at":        "2026-10-20T01:02:03Z",
		"x_source_modified_at":       "2026-10-21T04:05:06Z",
		"x_download_started_at":      "2026-10-28T02:00:00Z",
		"x_download_finished_at":     "2026-10-28T02:01:30Z",
		"downloaded_at":              "2026-10-28T02:01:30Z",
		"x_download_duration_ms":     int64(90000),
		"x_download_anonymous_bytes": int64(100),
		"x_download_account_bytes":   int64(200),
		"x_download_bytes_per_sec":   int64(3),
	}
	for k, want := range checks {
		if got, ok := meta[k]; !ok || got != want {
			t.Errorf("%s = %v (present=%v), want %v", k, got, ok, want)
		}
	}
	// 关键回归：source=transfer（服务端来源键）+ 审计键并存。
	if meta["source"] != "transfer" {
		t.Errorf("source = %v", meta["source"])
	}
}

// TestTransferSourceMeta_AuditSkipsUnknown 验证未知时间/带宽不写入（不编造）。
func TestTransferSourceMeta_AuditSkipsUnknown(t *testing.T) {
	t.Parallel()
	meta := map[string]any{"source": "transfer"}
	applyDownloadAudit(meta, &downloader.Result{Size: 10})
	for _, k := range []string{
		"x_source_created_at", "x_source_modified_at", "x_download_started_at",
		"x_download_finished_at", "downloaded_at", "x_download_duration_ms",
		"x_download_anonymous_bytes", "x_download_account_bytes", "x_download_bytes_per_sec",
	} {
		if _, ok := meta[k]; ok {
			t.Errorf("未知值不应写入 %s", k)
		}
	}
}
