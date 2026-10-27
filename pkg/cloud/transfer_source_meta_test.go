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
		ETag:     `"abc-123"`,
		Checksum: sha256Hex([]byte("hello source meta")),
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
