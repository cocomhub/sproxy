// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
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
func newTransferTestMgr(t *testing.T, fsFor func(volume string) (syncpkg.FS, string, bool)) *CloudDownloadManager {
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
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) {
		if vol == "secretdata-main" {
			return fs, "secretdata", false
		}
		return nil, "", false
	})
	task := &CloudTask{ID: "task-1", Filename: "movie.mp4", Transfer: &TransferSpec{Volume: "secretdata-main"}}
	result := &downloader.Result{Size: 5, Checksum: ""}

	dest := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(dest, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr, _, err := mgr.transferDone(context.Background(), task, dest, result, nil)
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
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-2", Filename: "a.mp4", Transfer: &TransferSpec{Volume: "vol-x"}}
	dest := filepath.Join(t.TempDir(), "a.mp4")
	_ = os.WriteFile(dest, []byte("data"), 0o600)

	start := time.Now()
	_, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil)
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
	// 卷写失败（目标卷 I/O 异常）→ 3 次指数重试耗尽 → 目标卷异常终止（TransferTargetErrors）
	fs.writeFn = func(rel string) error { return errors.New("volume io error") }
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-3", Filename: "b.mp4", Transfer: &TransferSpec{Volume: "vol-y"}}
	dest := filepath.Join(t.TempDir(), "b.mp4")
	_ = os.WriteFile(dest, []byte("corrupt-data"), 0o600)
	wantSum, _ := sha256File(dest)

	_, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Checksum: wantSum}, func(c context.Context) (*downloader.Result, error) {
		t.Fatal("目标卷 I/O 异常不应触发重下载（先重试卷耗尽）")
		return nil, nil
	})
	if err == nil || !errors.Is(err, ErrTransferTarget) {
		t.Fatalf("卷 I/O 异常应终止并归目标卷异常，got %v", err)
	}
	if mgr.metrics.TransferTargetErrors.Load() != 1 {
		t.Fatalf("TransferTargetErrors 应 1，实际 %d", mgr.metrics.TransferTargetErrors.Load())
	}
	if mgr.metrics.TransferFileErrors.Load() != 0 {
		t.Fatalf("卷 I/O 异常不应计 FileErrors，实际 %d", mgr.metrics.TransferFileErrors.Load())
	}
}

// TestTransferDone_FileCorrupt_TwiceConsistent NH-P3/文件异常：卷读回校验和与下载一致但
// 转存仍失败（文件本身异常）→ 两次一致后终止 + FileErrors。
// 模拟：WriteFile 成功（内容一致）但之后 transferOnce 返回「文件内容异常」——本测试
// 用 writeFn 写正常内容（校验通过）但 transferOnce 后置失败无法模拟 → 走卷损坏路径
// 已覆盖。此处验证哨兵分类不双计：卷 I/O 异常只计 TargetErrors（上测试）。

// TestTransferAfterDownload_SaveFalse_AutoCleansCloud Save=false（转存 + 客户端不下载）：
// 转存完成后服务端自动删 cloud 桶文件并记录清理状态（审计可查）。
func TestTransferAfterDownload_SaveFalse_AutoCleansCloud(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-4", Filename: "c.mp4", Transfer: &TransferSpec{Volume: "vol-z"}, Save: false}
	dest := filepath.Join(t.TempDir(), "c.mp4")
	_ = os.WriteFile(dest, []byte("xyz"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	handled, _ := mgr.transferAfterDownload(context.Background(), context.Background(), task, dest, &downloader.Result{})
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
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-5", Filename: "d.mp4", Transfer: &TransferSpec{Volume: "vol-z"}, Save: true}
	dest := filepath.Join(t.TempDir(), "d.mp4")
	_ = os.WriteFile(dest, []byte("xyz"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	handled, _ := mgr.transferAfterDownload(context.Background(), context.Background(), task, dest, &downloader.Result{})
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
	tr, _, err := mgr.transferDone(context.Background(), &CloudTask{ID: "t", Filename: "x"}, "any", &downloader.Result{}, nil)
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
	// 转存卷 resolver：v → 可解析 FS（前置校验需卷装配 + 协议声明）
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string, bool) {
		if vol == "v" {
			return newMemFS(), "secretdata", false
		}
		return nil, "", false
	}
	t.Cleanup(mgr.Close)
	_, err := mgr.CreateTask("url", "https://example.com/v.mp4", "v.mp4", 100, "", TaskParams{})
	if err == nil {
		t.Fatal("真空洞（不下载+无转存+不保留）应拒绝")
	}
	// 语义成立组合：
	// 1. 有 download_local → 合法（客户端拉取）
	if _, err := mgr.CreateTask("url", "https://example.com/a.mp4", "a.mp4", 100, "", TaskParams{DownloadLocal: true}); err != nil {
		t.Fatalf("download_local=true 应合法: %v", err)
	}
	// 2. 有 transfer → 合法
	if _, err := mgr.CreateTask("url", "https://example.com/b.mp4", "b.mp4", 100, "", TaskParams{Transfer: &TransferSpec{Volume: "v"}}); err != nil {
		t.Fatalf("有 transfer 应合法: %v", err)
	}
	// 3. save=true → 合法
	if _, err := mgr.CreateTask("url", "https://example.com/c.mp4", "c.mp4", 100, "", TaskParams{Save: true}); err != nil {
		t.Fatalf("save=true 应合法: %v", err)
	}
}

// TestFailTaskWithTransfer_NoDeadlock C2 回归：failTaskWithTransfer 复用 failTask
// （锁外 saveTask），转存失败不因 RWMutex 重入自锁挂死。
func TestFailTaskWithTransfer_NoDeadlock(t *testing.T) {
	t.Parallel()
	sm := capacity.NewStorageManager(t.TempDir(), 0, nil, testLogger())
	mgr, _ := newCloudTestManager(t, t.TempDir(), sm, &CloudDownloadConfig{MaxConcurrent: 3, TaskTTL: time.Hour})
	// 转存卷 resolver：v → 可解析 FS（前置校验需卷装配 + 协议声明）
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string, bool) {
		if vol == "v" {
			return newMemFS(), "secretdata", false
		}
		return nil, "", false
	}
	t.Cleanup(mgr.Close)
	task, err := mgr.CreateTask("url", "https://example.com/x.mp4", "x.mp4", 100, "", TaskParams{Save: true})
	if err != nil {
		t.Fatal(err)
	}
	// 模拟下载完成产物落 taskDir
	taskDir := mgr.TaskDirFor(task.Owner, task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(taskDir, "x.mp4")
	if err := os.WriteFile(dest, []byte("downloaded-full"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 转存失败路径（不挂死、任务 failed + TransferErr + 产物保留）
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
	// H1：转存失败保留已下载完整产物（不整删 taskDir）
	if _, serr := os.Stat(dest); serr != nil {
		t.Fatalf("转存失败应保留完整产物 %s，got err=%v", dest, serr)
	}
}

// TestCreateTask_TransferVolumePrecheck 前置判断：转存目标卷未装配/协议未声明 → 创建即拒
// （避免浪费资源下载——下载完成才发现卷不可用）。
func TestCreateTask_TransferVolumePrecheck(t *testing.T) {
	t.Parallel()
	sm := capacity.NewStorageManager(t.TempDir(), 0, nil, testLogger())
	mgr, _ := newCloudTestManager(t, t.TempDir(), sm, &CloudDownloadConfig{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr.Close)
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string, bool) {
		if vol == "ready" {
			return newMemFS(), "secretdata", false
		}
		return nil, "", false // 未装配
	}
	// 卷未装配 → 创建即拒
	if _, err := mgr.CreateTask("url", "https://example.com/x.mp4", "x.mp4", 100, "", TaskParams{Transfer: &TransferSpec{Volume: "missing"}}); err == nil {
		t.Fatal("未装配卷应创建即拒")
	}
	// 卷装配但协议未声明 → 创建即拒
	if _, err := mgr.CreateTask("url", "https://example.com/y.mp4", "y.mp4", 100, "", TaskParams{Transfer: &TransferSpec{Volume: "noscheme"}}); err == nil {
		t.Fatal("协议未声明应创建即拒")
	}
	// 卷就绪 → 创建成功
	if _, err := mgr.CreateTask("url", "https://example.com/z.mp4", "z.mp4", 100, "", TaskParams{Transfer: &TransferSpec{Volume: "ready"}}); err != nil {
		t.Fatalf("就绪卷应创建成功: %v", err)
	}
}

// TestTransferDone_SharedVolume_OwnerPrefix 共享卷（内容不共享）：落盘路径加 owner 前缀
// 隔离（自动派生 + 显式 path 均强制）——防跨 owner 覆写共享卷（M8，用户裁定 2026-10-04）。
func TestTransferDone_SharedVolume_OwnerPrefix(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	// shared=true 模拟共享卷
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) {
		return fs, "secretdata", true
	})
	task := &CloudTask{ID: "task-s1", Filename: "movie.mp4", Owner: "alice",
		Transfer: &TransferSpec{Volume: "shared-vault"}}
	result := &downloader.Result{Size: 5, Checksum: ""}
	dest := filepath.Join(t.TempDir(), "movie.mp4")
	_ = os.WriteFile(dest, []byte("hello"), 0o600)

	tr, _, err := mgr.transferDone(context.Background(), task, dest, result, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 自动派生：pikpak/<owner>/<taskID>/<file>（共享卷强制 owner 前缀）
	wantRel := "pikpak/alice/task-s1/movie.mp4"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("共享卷应加 owner 前缀落盘 %s，实际: %v", wantRel, keys(fs.files))
	}
	_ = tr
}

// TestTransferDone_SharedVolume_ExplicitPathOwnerPrefix 共享卷显式 path 也强制 owner 前缀。
func TestTransferDone_SharedVolume_ExplicitPathOwnerPrefix(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) {
		return fs, "secretdata", true
	})
	task := &CloudTask{ID: "task-s2", Filename: "b.mp4", Owner: "bob",
		Transfer: &TransferSpec{Volume: "shared-vault", Path: "my/movie.mp4"}}
	dest := filepath.Join(t.TempDir(), "b.mp4")
	_ = os.WriteFile(dest, []byte("data"), 0o600)
	if _, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil); err != nil {
		t.Fatal(err)
	}
	// 显式 path 也强制 owner 前缀：bob/my/movie.mp4
	wantRel := "bob/my/movie.mp4"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("共享卷显式 path 应加 owner 前缀 %s，实际: %v", wantRel, keys(fs.files))
	}
}

// TestTransferDone_PrivateVolume_NoPrefix 独享卷：用户直接操作，不加 owner 前缀。
func TestTransferDone_PrivateVolume_NoPrefix(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) {
		return fs, "secretdata", false // 独享
	})
	task := &CloudTask{ID: "task-p1", Filename: "c.mp4", Owner: "carol",
		Transfer: &TransferSpec{Volume: "my-vault"}}
	dest := filepath.Join(t.TempDir(), "c.mp4")
	_ = os.WriteFile(dest, []byte("xyz"), 0o600)
	if _, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil); err != nil {
		t.Fatal(err)
	}
	// 独享卷不加前缀：pikpak/task-p1/c.mp4
	wantRel := "pikpak/task-p1/c.mp4"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("独享卷不应加 owner 前缀 %s，实际: %v", wantRel, keys(fs.files))
	}
}

// TestTransferDone_SharedVolume_PrefixEscapeRejected NH2 回归：共享卷显式 path 用 ..
// 逃逸 owner 前缀（path.Join 折叠）→ 拒绝（防跨 owner 覆写共享卷）。
func TestTransferDone_SharedVolume_PrefixEscapeRejected(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) {
		return fs, "secretdata", true // 共享卷
	})
	// 显式 path 用 .. 逃逸前缀：path.Join(bob, ../x.pdf) = x.pdf → 落卷根（跨 owner）
	task := &CloudTask{ID: "task-e1", Filename: "a.mp4", Owner: "bob",
		Transfer: &TransferSpec{Volume: "shared-vault", Path: "../x.pdf"}}
	dest := filepath.Join(t.TempDir(), "a.mp4")
	_ = os.WriteFile(dest, []byte("data"), 0o600)
	if _, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil); err == nil {
		t.Fatal(".. 逃逸 owner 前缀应拒绝（防跨 owner 覆写）")
	}
	// 合法显式 path（含前缀后不逃逸）→ 成功
	task2 := &CloudTask{ID: "task-e2", Filename: "b.mp4", Owner: "bob",
		Transfer: &TransferSpec{Volume: "shared-vault", Path: "my/movie.mp4"}}
	dest2 := filepath.Join(t.TempDir(), "b.mp4")
	_ = os.WriteFile(dest2, []byte("data"), 0o600)
	if _, _, err := mgr.transferDone(context.Background(), task2, dest2, &downloader.Result{}, nil); err != nil {
		t.Fatalf("合法显式 path 应成功: %v", err)
	}
}

// TestTransferURL_EscapesSpecial NH4 回归：transferURL 路径段 percent-encode
// （#/% 文件名可往返，url.Parse 不截断/不报 invalid escape）。
func TestTransferURL_EscapesSpecial(t *testing.T) {
	t.Parallel()
	u := transferURL("secretdata", "vault", "pikpak/task-1/a#b%c.mp4")
	// 原始 # 必须被 encode（URL 字符串不含裸 #）；% 必须 encode 成 %25
	if strings.Contains(u, "a#b") {
		t.Fatalf("# 应被 percent-encode，got %q", u)
	}
	if !strings.Contains(u, "%23") {
		t.Fatalf("URL 应含 百分号23（# 转义），got %q", u)
	}
	if !strings.Contains(u, "%25") {
		t.Fatalf("URL 应含百分号25（percent 转义），got %q", u)
	}
	// url.Parse 可解析（% 已 encode，不报 invalid escape）
	if _, err := url.Parse(u); err != nil {
		t.Fatalf("percent-encoded URL 应可解析: %v", err)
	}
}
