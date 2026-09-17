// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// baidupcs_sync_e2e_test.go 是 P4 的**端到端**验证：真实装配链路上本地↔网盘双向同步。
//
// 链路（与生产 root.go 装配同构）：
//
//	syncmgr.Manager（kind=baidupcs 远端）
//	  → syncexec.Executor（BaidupcsFS 工厂注入）
//	    → setupBaidupcsFSFactory（T4 装配：VolumeBackend map + quota 适配器）
//	      → StorageFS（T1 真 ListDir/Stat）→ fake 内存网盘
//
// 与 T4 的单元测试不同：T4 只验证「工厂注入 + FS 可读写」；本文件验证**完整任务流**
// （SubmitAndStart → 异步执行 → 引擎 WalkEntries 遍历 → 文件落地/上传 → 统计回填）。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	baidupcs "github.com/cocomhub/sproxy/pkg/baidupcs"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// fakeBaidupcsE2EStorage 是完整语义的内存 StorageAPI（T5 专用）：
//   - Put 隐式建父目录标记（网盘目录语义，供 List/Stat 目录条目）；
//   - List 单层列举（prefix 下直接子项：文件 + 子目录，key 为完整相对路径）；
//   - Stat 支持目录（dirs 标记）与文件。
//
// 与 T4 的简化 fake（List 返回 nil）不同：端到端同步依赖引擎对 StorageFS 做
// WalkEntries 遍历（ListDir 单层 + 递归），List 必须有真实单层语义。
type fakeBaidupcsE2EStorage struct {
	mu    sync.Mutex
	files map[string][]byte // key → 内容
	dirs  map[string]struct{}
}

func newFakeBaidupcsE2EStorage() *fakeBaidupcsE2EStorage {
	return &fakeBaidupcsE2EStorage{
		files: map[string][]byte{},
		dirs:  map[string]struct{}{"/": {}},
	}
}

func (f *fakeBaidupcsE2EStorage) markDirs(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir := path.Dir(strings.TrimSuffix(key, "/"))
	for dir != "/" && dir != "." && dir != "" {
		f.dirs[dir] = struct{}{}
		dir = path.Dir(dir)
	}
	f.dirs["/"] = struct{}{}
}

func (f *fakeBaidupcsE2EStorage) Put(ctx context.Context, key string, r io.Reader) (*baidupcs.ObjectMeta, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.files[key] = data
	f.mu.Unlock()
	f.markDirs(key)
	return &baidupcs.ObjectMeta{Key: key, Size: int64(len(data)), ModTime: time.Now()}, nil
}

func (f *fakeBaidupcsE2EStorage) Get(ctx context.Context, key string) (io.ReadCloser, *baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[key]
	if !ok {
		return nil, nil, baidupcs.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(data))), &baidupcs.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeBaidupcsE2EStorage) Stat(ctx context.Context, key string) (*baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.dirs[key]; ok {
		return &baidupcs.ObjectMeta{Key: key, IsDir: true}, nil
	}
	data, ok := f.files[key]
	if !ok {
		return nil, baidupcs.ErrNotFound
	}
	return &baidupcs.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeBaidupcsE2EStorage) List(ctx context.Context, prefix string) ([]baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	base := strings.TrimSuffix(prefix, "/")
	if base != "" {
		base += "/"
	}
	out := make([]baidupcs.ObjectMeta, 0)
	seen := make(map[string]bool)
	// 文件（单层：prefix 下直接子项）
	for k := range f.files {
		rel, ok := strings.CutPrefix(k, base)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, baidupcs.ObjectMeta{Key: k, Size: int64(len(f.files[k])), ModTime: time.Now()})
	}
	// 子目录条目（单层）
	for d := range f.dirs {
		if d == "/" {
			continue
		}
		rel, ok := strings.CutPrefix(d, base)
		if !ok || rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, baidupcs.ObjectMeta{Key: d, IsDir: true})
	}
	return out, nil
}

func (f *fakeBaidupcsE2EStorage) Delete(ctx context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, key)
	return nil
}

func (f *fakeBaidupcsE2EStorage) Copy(ctx context.Context, srcKey, dstKey string) (*baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[srcKey]
	if !ok {
		return nil, baidupcs.ErrNotFound
	}
	f.files[dstKey] = data
	return &baidupcs.ObjectMeta{Key: dstKey, Size: int64(len(data))}, nil
}

var _ baidupcs.StorageAPI = (*fakeBaidupcsE2EStorage)(nil)

// e2eTenantRoot 返回测试用租户根解析器（<base>/<owner>/user 为 user 根；空 owner → anonymous）。
// 与 pkg/syncmgr integration 外部测试的 newTestTenantEnv 同构（cmd/sproxy 无法访问该包内部 helper）。
func e2eTenantRoot(t *testing.T) (base string, resolver syncmgr.TenantRootResolver) {
	t.Helper()
	base = t.TempDir()
	resolver = func(owner string) (string, string, bool) {
		if owner == "" {
			owner = "anonymous"
		}
		return filepath.Join(base, owner, "user"), filepath.Join(base, owner, "meta", "sync"), true
	}
	return base, resolver
}

// newBaidupcsE2EManager 装配完整链路（T5 专用）：fake 网盘 Storage → setupBaidupcsFSFactory
// → syncexec.Executor → syncmgr.Manager。返回 manager 与 fake storage（断言用）。
func newBaidupcsE2EManager(t *testing.T) (*syncmgr.Manager, *fakeBaidupcsE2EStorage, string) {
	t.Helper()
	_, resolver := e2eTenantRoot(t)
	userRoot, _, _ := resolver("")
	cfg := server.Default()
	cfg.LogLevel = "error"
	cfg.Baidupcs.Enabled = true
	cfg.Baidupcs.Name = "mydisk"
	cfg.Baidupcs.BDUSS = "test-bduss"

	st := newFakeBaidupcsE2EStorage()
	factory := func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) { return st, nil }

	exec := syncexec.NewExecutor(resolver, discardLoggerMain())
	setupBaidupcsFSFactory(exec, cfg, discardLoggerMain(), factory)
	if exec.BaidupcsFS == nil {
		t.Fatal("setupBaidupcsFSFactory 应注入 BaidupcsFS 工厂")
	}

	remotes := []syncmgr.RemoteConfig{{
		Name: "r-bd", Kind: syncmgr.RemoteKindBaidupcs, Volume: "mydisk",
	}}
	mgr := syncmgr.NewManager(resolver, nil, nil, 0, remotes, exec, discardLoggerMain(),
		&syncmgr.Config{MaxConcurrent: 2, TaskTTL: time.Hour})
	t.Cleanup(mgr.Stop)
	return mgr, st, userRoot
}

// waitBaidupcsStatus 轮询任务状态直到 want（或超时 30s，-race 余量）。
func waitBaidupcsStatus(t *testing.T, mgr *syncmgr.Manager, id, want string) *syncmgr.SyncTask {
	t.Helper()
	var last string
	testutil.WaitFor(t, 30*time.Second, func() bool {
		task := mgr.Get(id, "")
		if task == nil {
			last = "<not found>"
			return false
		}
		last = task.Status
		if task.Status == want {
			return true
		}
		if task.Status == "failed" && want != "failed" {
			t.Fatalf("task %s 失败（want %s）: %s", id, want, task.Error)
		}
		return false
	}, func() string { return fmt.Sprintf("wait status %s=%s 超时，最后观测 %s", id, want, last) })
	task := mgr.Get(id, "")
	if task == nil {
		t.Fatalf("task %s 在达到 %s 后被删除", id, want)
	}
	return task
}

// e2eWriteLocal 在本地根写文件（父目录自动创建）。
func e2eWriteLocal(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestBaidupcsE2E_Push 端到端 push：本地文件 → baidupcs 卷，网盘出现（含递归子目录）。
func TestBaidupcsE2E_Push(t *testing.T) {
	t.Parallel()
	mgr, st, userRoot := newBaidupcsE2EManager(t)

	// 本地 user 根写文件（owner="" → anonymous 租户 user 根）。
	e2eWriteLocal(t, userRoot, "a.txt", "hello push")
	e2eWriteLocal(t, userRoot, "sub/b.txt", "world push")

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{
		Direction: "push", Remote: "r-bd", Recursive: true, ConflictPolicy: syncmgr.ConflictSkip,
	})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	done := waitBaidupcsStatus(t, mgr, task.ID, "completed")
	if done.FilesTotal != 2 || done.FilesDone != 2 {
		t.Fatalf("统计不符: total=%d done=%d, want 2/2", done.FilesTotal, done.FilesDone)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if got := string(st.files["a.txt"]); got != "hello push" {
		t.Fatalf("网盘 a.txt = %q, want %q", got, "hello push")
	}
	if got := string(st.files["sub/b.txt"]); got != "world push" {
		t.Fatalf("网盘 sub/b.txt = %q, want %q", got, "world push")
	}
	if _, ok := st.dirs["sub"]; !ok {
		t.Fatal("网盘应有 sub 目录标记")
	}
}

// TestBaidupcsE2E_Pull 端到端 pull：网盘文件 → 本地 user 根落盘（含递归子目录）。
func TestBaidupcsE2E_Pull(t *testing.T) {
	t.Parallel()
	mgr, st, userRoot := newBaidupcsE2EManager(t)

	// 预置网盘文件（fake Put 隐式建目录）。
	if _, err := st.Put(context.Background(), "r.txt", strings.NewReader("remote content")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), "sub/x.txt", strings.NewReader("nested")); err != nil {
		t.Fatal(err)
	}

	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{
		Direction: "pull", Remote: "r-bd", Recursive: true, ConflictPolicy: syncmgr.ConflictSkip,
	})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	done := waitBaidupcsStatus(t, mgr, task.ID, "completed")
	if done.FilesTotal != 2 || done.FilesDone != 2 {
		t.Fatalf("统计不符: total=%d done=%d, want 2/2", done.FilesTotal, done.FilesDone)
	}
	if got := readE2EFile(t, userRoot, "r.txt"); got != "remote content" {
		t.Fatalf("本地 r.txt = %q, want %q", got, "remote content")
	}
	if got := readE2EFile(t, userRoot, "sub/x.txt"); got != "nested" {
		t.Fatalf("本地 sub/x.txt = %q, want %q", got, "nested")
	}
}

func readE2EFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}
