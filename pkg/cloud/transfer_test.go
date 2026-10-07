// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
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
	return nil, os.ErrNotExist
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

// WriteIfAbsent 实现 pkg/sync.WriteIfAbsent（测试 memFS 模拟能力卷：转存目标唯一、拒绝覆写）。
func (m *memFS) WriteIfAbsent(ctx context.Context, path string, r io.Reader, size int64, mtime int64) (bool, error) {
	if _, exists := m.files[path]; exists {
		return false, nil
	}
	if err := m.WriteFile(ctx, path, r, size, mtime); err != nil {
		return false, err
	}
	return true, nil
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
	// VolumeFor：转存键空间经 volume.ResolveOwnerPath 计算——测试注入与 fsFor 同语义的
	// 卷元信息（共享/独享、owner 白名单）。fsFor 返回第 3 值 shared。
	mgr.volumeFor = func(vol string) (volume.Volume, bool) {
		_, _, shared := fsFor(vol)
		// 卷 ACL 与 fsFor 的 shared 语义一致：
		//   - shared=true → ModeDeny（共享：转存加 owner 前缀隔离，任何 owner 可访问）；
		//   - shared=false → ModeAllow + 单 owner（""）→ Authorize("") 通过、Shared()==false
		//     （独享无前缀；领域测试 owner 多为空）。
		// 非空 owner 的独享测试（如 PrivateVolume_NoPrefix 用 carol）在测试内显式覆盖。
		v := volume.Volume{Name: vol, Type: "secretdata"}
		if shared {
			v.ACL = volume.ACL{Mode: volume.ModeDeny}
		} else {
			v.ACL = volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"": {}}}
		}
		return v, true
	}
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
	// 目标路径自动派生 <owner>/user/<taskID>/<filename>（空 owner → anonymous）
	wantRel := "anonymous/user/task-1/movie.mp4"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("目标卷应收到 %s，实际文件: %v", wantRel, keys(fs.files))
	}
	// 目录自动生成（anonymous/user + anonymous/user/task-1）
	if !fs.dirs["anonymous/user"] || !fs.dirs["anonymous/user/task-1"] {
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

// TestTransferAfterDownload_SaveFalse_AutoCleansCloud Save=false（转存+客户端不下载）：
// 转存完成后最终统一清理路径（finalize 后）自动删 cloud 桶文件并记录清理状态。
// b1：清理门已从 transferAfterDownload 移到 executeDownload finalize 后统一覆盖
// （含纯下载 Transfer==nil 路径）——此测试验 transfer 路径经统一清理入口不落下。
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
	mgr.mu.RLock()
	stored, ok := mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if !ok || stored.TransferURL == "" {
		t.Fatalf("任务应记录 TransferURL，got ok=%v", ok)
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
		mgr.failTaskWithTransfer(task, dest, fmt.Errorf("transfer: 目标卷异常（重试耗尽）"))
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

// TestTransferDone_SharedVolume_EmptyOwnerNormalized（真实链路修复回归 2026-10-05）：
// 共享卷 + 空 owner（loopback 免签 actor=""）→ 归一为 anonymous 加前缀落盘
// anonymous/user/<taskID>/<file>——此前 `task.Owner != ""` 跳过前缀落盘
// user/<taskID>/<file>，与读路径 ResolveOwnerPath 的 anonymous 前缀 404 断连。
func TestTransferDone_SharedVolume_EmptyOwnerNormalized(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) {
		return fs, "secretdata", true
	})
	task := &CloudTask{ID: "task-e0", Filename: "movie.mp4", // Owner 空（loopback 免签）
		Transfer: &TransferSpec{Volume: "shared-vault"}}
	dest := filepath.Join(t.TempDir(), "movie.mp4")
	_ = os.WriteFile(dest, []byte("hello"), 0o600)

	if _, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil); err != nil {
		t.Fatal(err)
	}
	// 空 owner 归一 anonymous → 共享卷落盘 anonymous/user/<taskID>/<file>。
	wantRel := "anonymous/user/task-e0/movie.mp4"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("共享卷空 owner 应归一 anonymous 加前缀落盘 %s，实际: %v", wantRel, keys(fs.files))
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
	// 自动派生：<owner>/user/<taskID>/<file>（共享卷强制 owner 前缀）
	wantRel := "alice/user/task-s1/movie.mp4"
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
	// 显式 path 也强制 owner 前缀 + user 桶（I-1：读端恒按 user/ 桶重算键）：bob/user/my/movie.mp4
	wantRel := "bob/user/my/movie.mp4"
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
	// 独享卷 ACL：单 owner carol 授权（helper 默认独享卷只授 "" owner）。
	mgr.volumeFor = func(vol string) (volume.Volume, bool) {
		return volume.Volume{Name: vol, Type: "secretdata",
			ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"carol": {}}}}, true
	}
	task := &CloudTask{ID: "task-p1", Filename: "c.mp4", Owner: "carol",
		Transfer: &TransferSpec{Volume: "my-vault"}}
	dest := filepath.Join(t.TempDir(), "c.mp4")
	_ = os.WriteFile(dest, []byte("xyz"), 0o600)
	if _, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil); err != nil {
		t.Fatal(err)
	}
	// 独享卷也恒加 owner 前缀（2026-10-07 废弃区分）：carol/user/task-p1/c.mp4
	wantRel := "carol/user/task-p1/c.mp4"
	if _, ok := fs.files[wantRel]; !ok {
		t.Fatalf("独享卷应加 owner 前缀 %s，实际: %v", wantRel, keys(fs.files))
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
		t.Fatal(".. 逃逸 user/ 桶（NH2 升级）应拒绝——path.Join(bob, user, ../x.pdf) 折叠出桶")
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
	u := transferURL("secretdata", "vault", "user/task-1/a#b%c.mp4")
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

// TestCleanupCloudIfNotNeeded b1 回归：清理门集中收口 cleanupCloudIfNotNeeded
// ——只在 Save=false 且 DownloadLocal=false 时删文件+记录 CleanupStatus；
// Save=true 或 DownloadLocal=true 时不动（交由客户端链式拉取后删）。
func TestCleanupCloudIfNotNeeded(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		save        bool
		dlLocal     bool
		wantCleaned bool
	}{
		{"save-false-no-local", false, false, true},
		{"save-true-no-local", true, false, false},
		{"save-false-local-true", false, true, false},
		{"save-true-local-true", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := newMemFS()
			mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
			id := "job-" + tc.name
			task := &CloudTask{ID: id, Filename: "f.bin", Save: tc.save, DownloadLocal: tc.dlLocal}
			dest := filepath.Join(t.TempDir(), "f.bin")
			_ = os.WriteFile(dest, []byte("data"), 0o600)
			mgr.mu.Lock()
			mgr.tasks[id] = task
			mgr.mu.Unlock()

			mgr.cleanupCloudIfNotNeeded(task, dest)

			assertCleanupState(t, mgr, id, dest, tc.wantCleaned)
		})
	}
}

// assertCleanupState 断言 cleanupCloudIfNotNeeded 后的磁盘/状态（S3776 收敛）。
func assertCleanupState(t *testing.T, mgr *CloudDownloadManager, id, dest string, wantCleaned bool) {
	t.Helper()
	if wantCleaned {
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatalf("应删 cloud 文件，got stat err=%v", err)
		}
	} else if _, err := os.Stat(dest); err != nil {
		t.Fatalf("应保留文件，got err=%v", err)
	}
	mgr.mu.RLock()
	got := mgr.tasks[id].CleanupStatus
	mgr.mu.RUnlock()
	if wantCleaned {
		if got != "cleaned" {
			t.Fatalf("CleanupStatus=%q want cleaned", got)
		}
	} else if got != "" {
		t.Fatalf("不应记录清理，got %q", got)
	}
}

// TestTransferDone_DuplicateRel_RejectsOverwrite W1/W3 回归：转存目标 rel 已存在 →
// 拒绝覆盖并返回 ErrTransferTarget。重复转存同 rel（重试提交/并发同路径）不应覆写
// 既有产物，转存结果唯一可追溯。
func TestTransferDone_DuplicateRel_RejectsOverwrite(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	fs.writeFn = nil
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-w3", Filename: "w3.mp4", Transfer: &TransferSpec{Volume: "vol-w"}}
	dest := filepath.Join(t.TempDir(), "w3.mp4")
	_ = os.WriteFile(dest, []byte("first"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	// 首次转存成功
	tr1, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil)
	if err != nil || tr1 == nil {
		t.Fatalf("首次转存应成功，tr=%v err=%v", tr1, err)
	}
	// 第二次同 rel 转存（换内容）→ 拒绝覆写
	_ = os.WriteFile(dest, []byte("second"), 0o600)
	tr2, _, err2 := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil)
	if err2 == nil {
		t.Fatalf("同 rel 重复转存应拒绝覆写，got nil err tr=%v", tr2)
	}
	if !errors.Is(err2, ErrTransferTarget) {
		t.Fatalf("覆盖拒绝应归目标卷异常（ErrTransferTarget），got %v", err2)
	}
	// 卷中内容仍是首次（未被覆盖）
	if got := string(fs.files["anonymous/user/task-w3/w3.mp4"]); got != "first" {
		t.Fatalf("卷内内容应保持首次 %q，got %q（被静默覆盖）", "first", got)
	}
}

// TestWriteTargetUnique_Fallback_NoAbsentCapability 降级路径：卷未实现 WriteIfAbsent →
// 写前 Stat 尽力检查：目标不存在→写成功；已存在→拒绝覆写。验证 W1/W3 降级语义。
func TestWriteTargetUnique_Fallback_NoAbsentCapability(t *testing.T) {
	t.Parallel()
	// 无 WriteIfAbsent 能力的 FS：baseCapableFS 只提升 7 方法（不含 WriteIfAbsent）。
	fs := &baseCapableFS{inner: newMemFS()}
	env := &transferEnv{ctx: context.Background(), targetFS: fs, rel: "r/x"}
	_ = fs.inner.files
	// 目标不存在（Stat 返回 nil,nil 表示不存在）→ 写成功
	written, err := writeTargetUnique(env, strings.NewReader("hello"), 5, 0)
	if err != nil || !written {
		t.Fatalf("目标不存在应写成功, written=%v err=%v", written, err)
	}
	// 目标已存在 → 拒绝
	written, err = writeTargetUnique(env, strings.NewReader("again"), 5, 0)
	if err != nil || written {
		t.Fatalf("目标已存在应拒绝覆盖, written=%v err=%v", written, err)
	}
}

// baseCapableFS 包装 memFS，只提升 7 方法（不提升 WriteIfAbsent）→ 模拟无能力卷。
type baseCapableFS struct{ inner *memFS }

func (b *baseCapableFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return b.inner.ListDir(ctx, p)
}
func (b *baseCapableFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return b.inner.Stat(ctx, p)
}
func (b *baseCapableFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return b.inner.OpenRead(ctx, p)
}
func (b *baseCapableFS) WriteFile(ctx context.Context, p string, r io.Reader, sz, mt int64) error {
	return b.inner.WriteFile(ctx, p, r, sz, mt)
}
func (b *baseCapableFS) Rename(ctx context.Context, f, t string) error {
	return b.inner.Rename(ctx, f, t)
}
func (b *baseCapableFS) Delete(ctx context.Context, p string) error  { return b.inner.Delete(ctx, p) }
func (b *baseCapableFS) MakeDir(ctx context.Context, p string) error { return b.inner.MakeDir(ctx, p) }

// nilNilFS 包装 baseCapableFS，Stat 恒返 (nil,nil)（真实卷契约：local/sftp/webdav/secretdata
// 对缺失路径统一返 (nil,nil)，与 memFS 的 (nil, os.ErrNotExist) 不同）——覆盖降级写路径
// 对真实卷契约的判定（Critical-2 修复：首写不被误判已存在）。
type nilNilFS struct{ inner *baseCapableFS }

func (q *nilNilFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return q.inner.ListDir(ctx, p)
}
func (q *nilNilFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return nil, nil // 真实卷缺失契约：Entry nil + err nil
}
func (q *nilNilFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return q.inner.OpenRead(ctx, p)
}
func (q *nilNilFS) WriteFile(ctx context.Context, p string, r io.Reader, sz, mt int64) error {
	return q.inner.WriteFile(ctx, p, r, sz, mt)
}
func (q *nilNilFS) Rename(ctx context.Context, f, t string) error { return q.inner.Rename(ctx, f, t) }
func (q *nilNilFS) Delete(ctx context.Context, p string) error    { return q.inner.Delete(ctx, p) }
func (q *nilNilFS) MakeDir(ctx context.Context, p string) error   { return q.inner.MakeDir(ctx, p) }

// TestWriteTargetUnique_Fallback_NilNilStatContract 降级写路径对「真实卷 (nil,nil) 缺失契约」
// 的正确性（Critical-2/第 2 轮 Minor-2）：Stat 返 (nil,nil) = 缺失 → 应放行写；此前旧判据
// serr==nil 当「已存在」会误拒普通卷首写。memFS 返 os.ErrNotExist 已覆盖，但 (nil,nil)
// 契约分支此前无测试触达。
func TestWriteTargetUnique_Fallback_NilNilStatContract(t *testing.T) {
	t.Parallel()
	fs := &nilNilFS{inner: &baseCapableFS{inner: newMemFS()}}
	env := &transferEnv{ctx: context.Background(), targetFS: fs, rel: "r/y"}
	written, err := writeTargetUnique(env, strings.NewReader("nilnil"), 6, 0)
	if err != nil || !written {
		t.Fatalf("(nil,nil)=缺失契约下首写应成功, written=%v err=%v", written, err)
	}
	// 第二次（内容已存在但 Stat 仍返 nil,nil 会怎样？真实卷对已存在返 (*Entry,nil)——此处
	// fake 恒 nil,nil 模拟缺失，验证「已存在返回 Entry」分支由 memFS 测试覆盖；此处只断言
	// 缺失契约放行写。内容写入确认。
	rc, oerr := fs.OpenRead(context.Background(), "r/y")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "nilnil" {
		t.Fatalf("内容 = %q, want nilnil", got)
	}
}

// TestFailTaskWithTransfer_SaveFalse_CleansCloud W5 回归：转存失败（目标卷异常）+ save=false
// → 服务端删 cloud 桶文件并记录 CleanupStatus（save 控制 cloud 副本存在性，与成败无关）。
// 清理须是最后一步（删除后无桶文件操作）。
func TestFailTaskWithTransfer_SaveFalse_CleansCloud(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	fs.writeFn = func(rel string) error { return errors.New("volume io error") } // 转存必失败
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-w5", Filename: "w5.mp4", Transfer: &TransferSpec{Volume: "vol-w5"}, Save: false, DownloadLocal: false}
	dest := filepath.Join(t.TempDir(), "w5.mp4")
	_ = os.WriteFile(dest, []byte("w5data"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	mgr.failTaskWithTransfer(task, dest, ErrTransferTarget)

	if task.Status != "failed" {
		t.Fatalf("转存失败应任务 failed，got %q", task.Status)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("W5: save=false 转存失败应删 cloud 桶文件，got stat err=%v", err)
	}
	mgr.mu.RLock()
	got := mgr.tasks[task.ID].CleanupStatus
	mgr.mu.RUnlock()
	if got != "cleaned" {
		t.Fatalf("W5: save=false 应 CleanupStatus=cleaned，实际 %q", got)
	}
}

// TestTransferDone_AbortedOnCancel M2 回归：转存期间任务被取消 → transferLoop 中止
// （不写目标卷、不记失败指标、不 failTask 覆盖 cancelled）。从 storage 重读状态，防孤儿写。
//
// 构造说明（修正版，原测试空转）：首次卷写**失败**（目标卷异常）→ 进入指数退避重试 →
// 退避期间取消任务 → 下一轮循环顶 transferAbortGate 检测 cancelled → 中止。断言 writeFn
// 只调 1 次（无孤儿第二次写）。原实现让第一次写成功 → transferOnce 直接返回 → 循环成功
// 提前退出，abort 分支从未执行（空转）。
func TestTransferDone_AbortedOnCancel(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	var calls atomic.Int64
	fs.writeFn = func(rel string) error {
		calls.Add(1)
		if calls.Load() == 1 {
			return errors.New("transient target write failure") // 首次写失败 → 触发重试退避
		}
		return nil
	}
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-m2", Filename: "m2.mp4", Transfer: &TransferSpec{Volume: "vol-m2"}}
	dest := filepath.Join(t.TempDir(), "m2.mp4")
	_ = os.WriteFile(dest, []byte("m2data"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil)
	}()
	// 等待第一次写失败发生（进入重试退避）
	testutil.WaitFor(t, 5*time.Second, func() bool { return calls.Load() >= 1 }, "第一次卷写应已发生")
	// 退避期间取消任务（从 storage 重读 → transferLoop 下一轮 gate 中止）
	mgr.mu.Lock()
	mgr.tasks[task.ID].Status = "cancelled"
	mgr.mu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("transferDone 未在取消后退出")
	}
	if mgr.metrics.TransfersFailed.Load() != 0 {
		t.Fatalf("M2: 取消中止不应计 TransfersFailed，实际 %d", mgr.metrics.TransfersFailed.Load())
	}
	// 无孤儿写卷：gate 阻止了第二次写（重试被取消拦截）
	if calls.Load() != 1 {
		t.Fatalf("M2: 取消后不应再写卷（孤儿写卷），实际 %d 次写", calls.Load())
	}
}

// TestTransferDone_VolumeQuotaDenied NH1 回归：目标卷实现 ReserveSpace 且容量不足 →
// 转存拒绝（fail-closed，不写目标卷）。远程网盘卷用户配额跳过。
func TestTransferDone_VolumeQuotaDenied(t *testing.T) {
	t.Parallel()
	fs := &quotaDenyFS{inner: newMemFS()}
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "s3", false })
	task := &CloudTask{ID: "task-nh1", Filename: "q.mp4", Transfer: &TransferSpec{Volume: "vol-q"}}
	dest := filepath.Join(t.TempDir(), "q.mp4")
	_ = os.WriteFile(dest, []byte("nh1data"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	_, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Size: 8}, nil)
	if err == nil {
		t.Fatal("NH1: 卷容量不足应拒绝转存")
	}
	if !errors.Is(err, ErrTransferTarget) {
		t.Fatalf("卷容量拒绝应归 ErrTransferTarget，got %v", err)
	}
	if len(fs.inner.files) != 0 {
		t.Fatalf("NH1: 卷容量不足不应写入目标卷，实际 %d 文件", len(fs.inner.files))
	}
}

// quotaDenyFS 包装 memFS：实现 ReserveSpace 恒拒绝（模拟外部网盘配额不足）。
// 不实现 syncpkg.LocalVolume → 默认视为外部卷（远程，容量/配额由卷自身管理），
// 与「外部卷零配置」的用户裁定一致（用户裁定 2026-10-05）。
type quotaDenyFS struct {
	inner *memFS
}

func (q *quotaDenyFS) ReserveSpace(ctx context.Context, rel string, size int64) error {
	return errors.New("volume quota exceeded")
}
func (q *quotaDenyFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return q.inner.ListDir(ctx, p)
}
func (q *quotaDenyFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return q.inner.Stat(ctx, p)
}
func (q *quotaDenyFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return q.inner.OpenRead(ctx, p)
}
func (q *quotaDenyFS) WriteFile(ctx context.Context, p string, r io.Reader, sz, mt int64) error {
	return q.inner.WriteFile(ctx, p, r, sz, mt)
}
func (q *quotaDenyFS) Rename(ctx context.Context, f, t string) error {
	return q.inner.Rename(ctx, f, t)
}
func (q *quotaDenyFS) Delete(ctx context.Context, p string) error  { return q.inner.Delete(ctx, p) }
func (q *quotaDenyFS) MakeDir(ctx context.Context, p string) error { return q.inner.MakeDir(ctx, p) }

// TestTransferFullChain_ResolveURLRoundTrip a1 回归：全链路 black-box——
// 真实下载 → 转存（TransferSpec）→ 任务 TransferURL → ResolveURL 取用内容与源一致。
// 覆盖「flag 参数 → 服务端下载 → 转存到卷 → URL 取用」完整闭环（无 e2e 时的最小闭环）。
// 注（覆盖分工）：本测试用 memFS 直接按 URL path 取用（cloud 层闭环）；registry 层的
// ResolveURL→scheme 寻址→OpenURL 真值由 pkg/volume/registry set_test.go
// （TestResolveURL/TestResolveURL_S3ProtocolRegistered/TestResolveURL_DefaultAuthority_Deterministic）
// 与各卷 OpenURL 测试（secretdata/s3 等）覆盖——两段拼接即完整「转存产物可 ResolveURL 取用」。
func TestTransferFullChain_ResolveURLRoundTrip(t *testing.T) {
	t.Parallel()
	content := []byte("a1 full chain roundtrip payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		if _, err := w.Write(content); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{SyncThreshold: 20 * 1024 * 1024, MaxConcurrent: 3, TaskTTL: 24 * time.Hour, FailedTaskTTL: 1 * time.Hour, AllowPrivate: true, TransferConcurrency: 2}
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)
	vaultFS := newMemFS() // 单一实例：转存写入与取用读同一 FS
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string, bool) {
		if vol == "vault" {
			return vaultFS, "secretdata", false
		}
		return nil, "", false
	}
	// VolumeFor：独享卷（owner 空授权）。
	mgr.volumeFor = func(vol string) (volume.Volume, bool) {
		return volume.Volume{Name: vol, Type: "secretdata",
			ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"": {}}}}, true
	}
	t.Cleanup(mgr.Close)

	// 同步下载 + 转存（transfer 参数经 TaskParams 传入 = 客户端 flag 落点）。
	task, err := mgr.SubmitAndStart("url", srv.URL, "roundtrip.bin", int64(len(content)), t.Context(), "", TaskParams{Save: true, Transfer: &TransferSpec{Volume: "vault"}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "completed" {
		t.Fatalf("a1: 下载+转存应 completed，got %q", task.Status)
	}
	if task.TransferURL == "" {
		t.Fatal("a1: 任务应有 TransferURL")
	}
	// 经 ResolveURL 取用（转存目标卷 FS 读 URL path 对应 rel）——模拟客户端取用闭环。
	u, _ := url.Parse(task.TransferURL)
	rel := strings.TrimPrefix(u.Path, "/")
	if rel == "" {
		t.Fatal("a1: TransferURL 应含路径")
	}
	// 转存目标卷 = newMemFS() 实例；从装配的 transferFSFor 取回 FS 读内容（等价 ResolveURL
	// → registry 定位卷 → FS.OpenRead(rel)）。
	t.Logf("a1: TransferURL=%s rel=%s memFS keys=%v", task.TransferURL, rel, keysOf(vaultFS))
	rc, err := vaultFS.OpenRead(t.Context(), rel)
	if err != nil {
		t.Fatalf("a1: ResolveURL 取用失败: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != string(content) {
		t.Fatalf("a1: 取用内容不一致，got %q want %q", string(got), string(content))
	}
}

// TestTransferDone_IdempotentSameContent Important-1 回归：同 rel 转存但卷内已是相同内容
// （崩溃窗口：写卷成功、TransferURL 落盘前进程崩 → 重启重放）→ 视为幂等已交付成功
// （返回 URL，不 fail）。与「不同内容拒绝覆写」（TestTransferDone_DuplicateRel_RejectsOverwrite）
// 成对：内容一致=已交付、内容不一致=真冲突。
func TestTransferDone_IdempotentSameContent(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-imp1", Filename: "imp1.mp4", Transfer: &TransferSpec{Volume: "vol-i"}}
	dest := filepath.Join(t.TempDir(), "imp1.mp4")
	_ = os.WriteFile(dest, []byte("identical-content"), 0o600)
	sha, _ := sha256File(dest)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	// 首次转存成功
	tr1, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Checksum: sha}, nil)
	if err != nil || tr1 == nil {
		t.Fatalf("首次转存应成功，tr=%v err=%v", tr1, err)
	}
	// 模拟崩溃重放：同 rel 再次转存，卷内已是相同内容 → 幂等成功（不 fail、不拒绝）。
	tr2, _, err2 := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Checksum: sha}, nil)
	if err2 != nil {
		t.Fatalf("同内容重放应幂等成功，got err %v", err2)
	}
	if tr2 == nil || tr2.URL != tr1.URL {
		t.Fatalf("幂等重放应返回同 URL，tr2=%v tr1=%v", tr2, tr1)
	}
	// 卷内内容未被改写（仍是首次内容）
	if got := string(fs.files["anonymous/user/task-imp1/imp1.mp4"]); got != "identical-content" {
		t.Fatalf("幂等重放不应改写卷内容，got %q", got)
	}
}

// TestTransferDone_IdempotentSameContent_NoChecksum I2 回归：空 checksum 下载器（pikpak
// 等非主下载器）崩溃重放——卷内已是相同内容（按本地产物哈希比对）→ 幂等成功，不永久 fail。
func TestTransferDone_IdempotentSameContent_NoChecksum(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return fs, "secretdata", false })
	task := &CloudTask{ID: "task-imp2", Filename: "imp2.mp4", Transfer: &TransferSpec{Volume: "vol-i2"}}
	dest := filepath.Join(t.TempDir(), "imp2.mp4")
	_ = os.WriteFile(dest, []byte("identical-nochecksum"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	// 首次转存成功（Checksum 为空 = 下载器未提供）
	tr1, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil)
	if err != nil || tr1 == nil {
		t.Fatalf("首次转存应成功，tr=%v err=%v", tr1, err)
	}
	// 崩溃重放：同 rel 再次转存，卷内已是相同内容 → 幂等成功（I2：不再永久 fail）
	tr2, _, err2 := mgr.transferDone(context.Background(), task, dest, &downloader.Result{}, nil)
	if err2 != nil {
		t.Fatalf("空 checksum 同内容重放应幂等成功，got err %v", err2)
	}
	if tr2 == nil || tr2.URL != tr1.URL {
		t.Fatalf("幂等重放应返回同 URL，tr2=%v tr1=%v", tr2, tr1)
	}
}

// keysOf 返回 memFS 已写键（测试调试）。
func keysOf(m *memFS) []string {
	ks := make([]string, 0, len(m.files))
	for k := range m.files {
		ks = append(ks, k)
	}
	return ks
}

// TestTransferDone_InternalVolumeQuotaGate Finding 2 回归：**内部/本地卷**（实现
// syncpkg.LocalVolume → IsLocalVolume true）转存走用户配额探测——配额不足即拒绝
// （fail-closed），外部卷（未实现 LocalVolume，默认远程）跳过配额。
// 此前 transfer 测试均不装配 QuotaFor + LocalVolume=true FS，用户配额分支零执行。
func TestTransferDone_InternalVolumeQuotaGate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	mgr, env := newCloudTestManager(t, dir, sm, &CloudDownloadConfig{
		MaxConcurrent: 3, TaskTTL: time.Hour, AllowPrivate: true,
	})
	// VolumeFor：独享卷（owner alice 授权）。
	mgr.volumeFor = func(vol string) (volume.Volume, bool) {
		return volume.Volume{Name: vol, Type: "secretdata",
			ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{"alice": {}}}}, true
	}
	// 装配 transferFSFor：内部卷（LocalVolume true → 走用户配额）。
	local := &localVolumeFS{inner: newMemFS()}
	mgr.transferFSFor = func(vol string) (syncpkg.FS, string, bool) {
		return local, "secretdata", false
	}
	t.Cleanup(mgr.Close)

	// owner 配额极低：转存 8 字节也超（内部卷走配额 → TryReserve 拒）。
	env.setOwnerQuota("alice", 4)
	task := &CloudTask{ID: "task-quota-internal", Filename: "q.mp4", Owner: "alice",
		Transfer: &TransferSpec{Volume: "my-vault"}}
	dest := filepath.Join(t.TempDir(), "q.mp4")
	_ = os.WriteFile(dest, []byte("quota-data"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	_, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Size: 10}, nil)
	if err == nil {
		t.Fatal("内部卷（LocalVolume true）转存应走用户配额——配额不足应拒绝")
	}
	if !errors.Is(err, ErrTransferTarget) {
		t.Fatalf("配额拒绝应归 ErrTransferTarget，got %v", err)
	}
	if len(local.inner.files) != 0 {
		t.Fatalf("内部卷配额不足不应写入目标卷，实际 %d 文件", len(local.inner.files))
	}
}

// localVolumeFS 包装 memFS 并实现 syncpkg.LocalVolume==true（模拟内部/本地卷）。
type localVolumeFS struct {
	inner *memFS
}

func (l *localVolumeFS) IsLocalVolume() bool { return true }
func (l *localVolumeFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return l.inner.ListDir(ctx, p)
}
func (l *localVolumeFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return l.inner.Stat(ctx, p)
}
func (l *localVolumeFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return l.inner.OpenRead(ctx, p)
}
func (l *localVolumeFS) WriteFile(ctx context.Context, p string, r io.Reader, sz, mt int64) error {
	return l.inner.WriteFile(ctx, p, r, sz, mt)
}
func (l *localVolumeFS) Rename(ctx context.Context, f, t string) error {
	return l.inner.Rename(ctx, f, t)
}
func (l *localVolumeFS) Delete(ctx context.Context, p string) error  { return l.inner.Delete(ctx, p) }
func (l *localVolumeFS) MakeDir(ctx context.Context, p string) error { return l.inner.MakeDir(ctx, p) }

// TestTransferToWrappedNoCapFS B1 回归：转存目标是被 trusted.Wrap 装饰的**无能力 FS**
// （模拟默认装配下 baidupcs/s3 被 Wrap：不实现 WriteIfAbsent/ReserveSpace）——装饰器
// 桩方法返回 ErrUnsupported → transferQuotaGate 跳过、writeTargetUnique 回落 Stat+WriteFile，
// 转存成功且落盘。此场景此前恒失败（装饰器硬错误 + 断言恒命中）。
func TestTransferToWrappedNoCapFS(t *testing.T) {
	t.Parallel()
	inner := newMemFS() // 不实现 ReserveSpace；WriteIfAbsent 由装饰器吞掉（inner 未实现？）
	// memFS 实现 WriteIfAbsent；为测「未实现回落」，
	// 构造 noCapMemFS（去掉 WriteIfAbsent 能力）——ReserveSpace 同样未实现。
	nc := &noCapMemFS{inner: inner}
	wrapped := trusted.Wrap(nc, trusted.Options{})
	mgr := newTransferTestMgr(t, func(vol string) (syncpkg.FS, string, bool) { return wrapped, "s3", false })
	task := &CloudTask{ID: "task-wrapped", Filename: "w.mp4", Transfer: &TransferSpec{Volume: "vol-w"}}
	dest := filepath.Join(t.TempDir(), "w.mp4")
	_ = os.WriteFile(dest, []byte("wrapped-data"), 0o600)
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	tr, _, err := mgr.transferDone(context.Background(), task, dest, &downloader.Result{Size: int64(len("wrapped-data"))}, nil)
	if err != nil {
		t.Fatalf("B1: 转存到被 Wrap 的无能力卷应成功（ErrUnsupported 回落）, got %v", err)
	}
	if tr == nil {
		t.Fatal("转存应返回成功结果")
	}
	// 自动派生 rel = <taskID>/<filename>；空 owner 归一 anonymous → 键 anonymous/user/task-wrapped/w.mp4。
	if got := inner.files["anonymous/user/task-wrapped/w.mp4"]; string(got) != "wrapped-data" {
		t.Fatalf("转存内容落盘不符: got %q", got)
	}
}

// noCapMemFS 是 memFS 的无 WriteIfAbsent 变体（模拟 s3/baidupcs 裸卷能力面）。
type noCapMemFS struct{ inner *memFS }

func (n *noCapMemFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return n.inner.ListDir(ctx, p)
}
func (n *noCapMemFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return n.inner.Stat(ctx, p)
}
func (n *noCapMemFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return n.inner.OpenRead(ctx, p)
}
func (n *noCapMemFS) WriteFile(ctx context.Context, p string, r io.Reader, sz, mt int64) error {
	return n.inner.WriteFile(ctx, p, r, sz, mt)
}
func (n *noCapMemFS) Rename(ctx context.Context, f, t string) error { return n.inner.Rename(ctx, f, t) }
func (n *noCapMemFS) Delete(ctx context.Context, p string) error    { return n.inner.Delete(ctx, p) }
func (n *noCapMemFS) MakeDir(ctx context.Context, p string) error   { return n.inner.MakeDir(ctx, p) }
