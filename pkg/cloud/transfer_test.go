// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// memFS 是测试用内存 sync.FS（记录写入/目录生成/校验和判定）。
type memFS struct {
	files   map[string][]byte
	dirs    map[string]bool
	writeFn func(rel string) error // 可注入写失败模拟
}

func newMemFS() *memFS {
	return &memFS{files: map[string][]byte{}, dirs: map[string]bool{}}
}

func (m *memFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) { return nil, nil }
func (m *memFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	if _, ok := m.files[path]; ok {
		return &syncpkg.Entry{Name: filepath.Base(path), Path: path, IsDir: false, Size: int64(len(m.files[path]))}, nil
	}
	return nil, nil
}
func (m *memFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	b, ok := m.files[path]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}
func (m *memFS) WriteFile(ctx context.Context, path string, r io.Reader, size int64, mtime int64) error {
	if m.writeFn != nil {
		if err := m.writeFn(path); err != nil {
			return err
		}
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.files[path] = b
	m.dirs[filepath.Dir(path)] = true
	return nil
}
func (m *memFS) Rename(ctx context.Context, from, to string) error { return nil }
func (m *memFS) Delete(ctx context.Context, path string) error {
	delete(m.files, path)
	return nil
}
func (m *memFS) MakeDir(ctx context.Context, path string) error {
	m.dirs[path] = true
	return nil
}

// newTransferTestMgr 构造带转存 FS resolver 的 CloudDownloadManager。
func newTransferTestMgr(t *testing.T, fsFor func(volume string) (syncpkg.FS, string)) *CloudDownloadManager {
	t.Helper()
	mgr, _ := newCloudTestManager(t, t.TempDir(), nil, &CloudDownloadConfig{
		MaxConcurrent: 3, TaskTTL: time.Hour,
	})
	mgr.transferFSFor = fsFor
	t.Cleanup(mgr.Close)
	return mgr
}

// TestTransferDone_Success_WritesToTarget 转存成功：目标卷收到文件 + 返回 URL + 目录自动生成。
func TestTransferDone_Success_WritesToTarget(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string) {
		if vol == "secretdata-main" {
			return fs, "secretdata"
		}
		return nil, ""
	})
	task := &CloudTask{ID: "task-1", Filename: "movie.mp4", Transfer: &TransferSpec{Volume: "secretdata-main"}}
	result := &downloader.Result{Size: 5, Checksum: ""}

	dest := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(dest, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, err := mgr.transferDone(context.Background(), task, dest, result, nil)
	if err != nil {
		t.Fatalf("transferDone: %v", err)
	}
	if tr == nil || tr.URL == "" {
		t.Fatal("转存应返回 URL")
	}
	// 目标路径自动派生 pikpak/<taskID>/<filename>
	wantRel := "pikpak/task-1/movie.mp4"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("目标卷应收到 %s，实际文件: %v", wantRel, keys(fs.files))
	}
	// 目录自动生成（pikpak + pikpak/task-1）
	if !fs.dirs["pikpak"] || !fs.dirs["pikpak/task-1"] {
		t.Fatalf("目标目录应自动生成，实际 dirs: %v", fs.dirs)
	}
	if !strings.HasPrefix(tr.URL, "secretdata://secretdata-main/") {
		t.Fatalf("URL 应为 secretdata://secretdata-main/... 协议，实际 %q", tr.URL)
	}
}

// TestTransferDone_TargetVolumeError_RetriesExhausted 目标卷异常：3 次指数重试后失败 + 指标。
func TestTransferDone_TargetVolumeError_RetriesExhausted(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	fs.writeFn = func(rel string) error { return errors.New("write failed: volume offline") }
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string) { return fs, "secretdata" })
	task := &CloudTask{ID: "task-2", Filename: "a.mp4", Transfer: &TransferSpec{Volume: "vol-x"}}
	dest := filepath.Join(t.TempDir(), "a.mp4")
	_ = os.WriteFile(dest, []byte("data"), 0o600)

	start := time.Now()
	_, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil)
	if err == nil || !strings.Contains(err.Error(), "目标卷") {
		t.Fatalf("目标卷异常应失败（重试耗尽），got %v", err)
	}
	// 3 次重试 = 1+2 = 3s 指数退避（1s+2s；第 3 次 attempt=2 直接失败）
	if time.Since(start) < 3*time.Second {
		t.Fatalf("应 3 次指数退避（≥3s），实际 %v", time.Since(start))
	}
	if mgr.metrics.TransferTargetErrors.Load() != 1 {
		t.Fatalf("TransferTargetErrors 应 1，实际 %d", mgr.metrics.TransferTargetErrors.Load())
	}
	if mgr.metrics.TransfersFailed.Load() != 1 {
		t.Fatalf("TransfersFailed 应 1，实际 %d", mgr.metrics.TransfersFailed.Load())
	}
}

// TestTransferDone_FileCorrupt_RetryTwiceFails 文件异常：转存写成功但读回校验和与下载不一致
// （文件被截断/损坏）→ 删本地重下载；两次重下载校验和一致但转存仍失败 → 终止 + 原因。
func TestTransferDone_FileCorrupt_RetryTwiceFails(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	// 写成功但内容被篡改（读回校验和 ≠ 下载 checksum → 文件内容异常）。
	fs.writeFn = func(rel string) error { return nil }
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string) { return fs, "secretdata" })
	task := &CloudTask{ID: "task-3", Filename: "b.mp4", Transfer: &TransferSpec{Volume: "vol-y"}}
	dest := filepath.Join(t.TempDir(), "b.mp4")
	_ = os.WriteFile(dest, []byte("corrupt-data"), 0o600)
	// 篡改写：写进去的是坏内容（校验和固定 ≠ 下载 checksum）
	origWrite := fs.writeFn
	fs.writeFn = func(rel string) error {
		_ = origWrite
		// 写坏数据到卷（读回校验失败）
		fs.files[rel] = []byte("bad-bad-bad")
		return nil
	}

	retries := 0
	_, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Checksum: "deadbeef"}, func(c context.Context) (*downloader.Result, error) {
		retries++
		// 重下载：重新写本地文件（内容仍与 checksum 不符 → 转存读回仍失败）
		_ = os.WriteFile(dest, []byte("corrupt-data-again"), 0o600)
		return &downloader.Result{Checksum: "deadbeef"}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "文件内容异常") {
		t.Fatalf("文件内容异常应终止，got %v", err)
	}
	if retries < 1 {
		t.Fatalf("应重下载 ≥1 次（M3 先重试卷），实际 %d", retries)
	}
	if mgr.metrics.TransferFileErrors.Load() != 1 {
		t.Fatalf("TransferFileErrors 应 1，实际 %d", mgr.metrics.TransferFileErrors.Load())
	}
}

// TestTransferAfterDownload_SaveFalse_AutoCleansCloud Save=false（转存 + 客户端不下载）：
// 转存完成后服务端自动删 cloud 桶文件并记录清理状态（审计可查）。
func TestTransferAfterDownload_SaveFalse_AutoCleansCloud(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string) { return fs, "secretdata" })
	task := &CloudTask{ID: "task-4", Filename: "c.mp4", Transfer: &TransferSpec{Volume: "vol-z"}, Save: false}
	dest := filepath.Join(t.TempDir(), "c.mp4")
	_ = os.WriteFile(dest, []byte("xyz"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	handled := mgr.transferAfterDownload(context.Background(), context.Background(), task, dest, &downloader.Result{})
	if handled {
		t.Fatal("转存成功不应 handled")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("Save=false 转存后应自动删 cloud 桶文件，got stat err=%v", err)
	}
	mgr.mu.RLock()
	stored, ok := mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if !ok || stored.TransferURL == "" {
		t.Fatalf("任务应记录 TransferURL，got ok=%v", ok)
	}
	if stored.CleanupStatus != "cleaned" {
		t.Fatalf("Save=false 应记录 CleanupStatus=cleaned，实际 %q", stored.CleanupStatus)
	}
	if mgr.metrics.TransfersSucceeded.Load() != 1 {
		t.Fatalf("TransfersSucceeded 应 1，实际 %d", mgr.metrics.TransfersSucceeded.Load())
	}
}

// TestTransferAfterDownload_SaveTrue_KeepsCloud Save=true（默认）：转存后保留 cloud 桶文件
// （由客户端链式 keep-files/显式 delete 控制清理），服务端不动。
func TestTransferAfterDownload_SaveTrue_KeepsCloud(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string) { return fs, "secretdata" })
	task := &CloudTask{ID: "task-5", Filename: "d.mp4", Transfer: &TransferSpec{Volume: "vol-z"}, Save: true}
	dest := filepath.Join(t.TempDir(), "d.mp4")
	_ = os.WriteFile(dest, []byte("xyz"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	handled := mgr.transferAfterDownload(context.Background(), context.Background(), task, dest, &downloader.Result{})
	if handled {
		t.Fatal("转存成功不应 handled")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("Save=true 应保留 cloud 桶文件，got err=%v", err)
	}
	mgr.mu.RLock()
	stored, _ := mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if stored.CleanupStatus != "" {
		t.Fatalf("Save=true 不应记录清理（CleanupStatus 空），实际 %q", stored.CleanupStatus)
	}
}

// TestTransferDone_NoTransfer_Noop 无转存要求 → 直接返回 nil（仅下载）。
func TestTransferDone_NoTransfer_Noop(t *testing.T) {
	t.Parallel()
	mgr := newTransferTestMgr(t, nil)
	tr, err := mgr.transferDone(context.Background(), &CloudTask{ID: "t", Filename: "x"}, "any", &downloader.Result{}, nil)
	if err != nil || tr != nil {
		t.Fatalf("无转存应 noop（tr=nil, err=nil），got tr=%v err=%v", tr, err)
	}
}

// keys 返回 map key 列表（测试断言用）。
func keys[K comparable, V any](m map[K]V) []K {
	out := make([]K, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 避免 fmt 未用
var _ = fmt.Sprintf

// TestCreateTask_VoidSemantics 真空洞校验：不下载本地 + 无 transfer + save=false → 拒绝。
func TestCreateTask_VoidSemantics(t *testing.T) {
	t.Parallel()
	sm := capacity.NewStorageManager(t.TempDir(), 0, nil, testLogger())
	mgr, _ := newCloudTestManager(t, t.TempDir(), sm, &CloudDownloadConfig{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Close)
	_, err := mgr.CreateTask("url", "https://example.com/v.mp4", "v.mp4", 100, "", nil, false, false)
	if err == nil {
		t.Fatal("真空洞（不下载+无转存+不保留）应拒绝")
	}
	// 语义成立组合：
	// 1. 有 download_local → 合法（客户端拉取）
	if _, err := mgr.CreateTask("url", "https://example.com/a.mp4", "a.mp4", 100, "", nil, true, false); err != nil {
		t.Fatalf("download_local=true 应合法: %v", err)
	}
	// 2. 有 transfer → 合法
	if _, err := mgr.CreateTask("url", "https://example.com/b.mp4", "b.mp4", 100, "", &TransferSpec{Volume: "v"}, false, false); err != nil {
		t.Fatalf("有 transfer 应合法: %v", err)
	}
	// 3. save=true → 合法
	if _, err := mgr.CreateTask("url", "https://example.com/c.mp4", "c.mp4", 100, "", nil, false, true); err != nil {
		t.Fatalf("save=true 应合法: %v", err)
	}
}

// TestFailTaskWithTransfer_NoDeadlock C2 回归：failTaskWithTransfer 复用 failTask
// （锁外 saveTask），转存失败不因 RWMutex 重入自锁挂死。
func TestFailTaskWithTransfer_NoDeadlock(t *testing.T) {
	t.Parallel()
	sm := capacity.NewStorageManager(t.TempDir(), 0, nil, testLogger())
	mgr, _ := newCloudTestManager(t, t.TempDir(), sm, &CloudDownloadConfig{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Close)
	task, err := mgr.CreateTask("url", "https://example.com/x.mp4", "x.mp4", 100, "", nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	// 转存失败路径（不挂死、任务 failed + TransferErr）
	done := make(chan struct{})
	go func() {
		mgr.failTaskWithTransfer(task, fmt.Errorf("transfer: 目标卷异常（重试耗尽）"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("failTaskWithTransfer 死锁（5s 未返回）")
	}
	mgr.mu.RLock()
	stored, ok := mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if !ok || stored.Status != "failed" {
		t.Fatalf("任务应 failed，got %+v", stored)
	}
	if stored.TransferErr == "" {
		t.Fatal("应记录 TransferErr")
	}
}
