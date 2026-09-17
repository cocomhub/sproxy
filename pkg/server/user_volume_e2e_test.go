// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// user_volume_e2e_test.go 是用户卷全链路端到端验证（U5）：
//  1. POST /api/volumes/user 创建用户卷（store 落盘 + Set.AddExternalVolume 注册）；
//  2. syncmgr 任务 push 到用户卷（fake executor 查 Set.External → pkg/sync.Engine 驱动，
//     本地 LocalFS → 用户卷 FS）；
//  3. 任务完成（状态 completed + 文件进入用户卷 FS）；
//  4. DELETE 删除（无活跃引用 → 成功，Set.External 清空 + store 删文件）。
//
// 用 fake backend（内存 StorageFS，唯一类型名 sync.Once 注册）——真实读写路径
// （NewStorageFS → StorageFS.Put 落网盘 fake），非 U3 的 nil-FS 桩。
//
// 附带验证 owner 归属：跨 owner 创建任务 → 拒绝（U4 闭包）。

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/baidupcs"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// userVolE2EType 是用户卷 e2e 的 fake backend 类型（唯一，sync.Once 注册）。
const userVolE2EType = "user-vol-e2e"

var registerUserVolE2EOnce sync.Once

// registerUserVolE2EBackend 注册 fake backend：从 v.Extra 读 local_root 构造
// NewStorageFS（内存 fake Storage）——真 FS 供 push 读写。
func registerUserVolE2EBackend() {
	registerUserVolE2EOnce.Do(func() {
		registry.RegisterBackend(userVolE2EType, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
			localRoot, _ := v.Extra["local_root"].(string)
			if localRoot == "" {
				localRoot = v.RootDir
			}
			fs, err := baidupcs.NewStorageFS(newFakeUserVolE2EStorage(), localRoot)
			if err != nil {
				return nil, err
			}
			return &userVolE2EBackend{fs: fs}, nil
		})
	})
}

// fakeUserVolE2EStorage 是内存 StorageAPI（Put/Get/Stat/List/Delete/Copy 全实现，
// 目录语义 markDirs）。供 StorageFS（baidupcs 适配层）驱动真实读写路径。
type fakeUserVolE2EStorage struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]struct{}
}

func newFakeUserVolE2EStorage() *fakeUserVolE2EStorage {
	return &fakeUserVolE2EStorage{files: map[string][]byte{}, dirs: map[string]struct{}{}}
}

func (f *fakeUserVolE2EStorage) markDirs(key string) {
	for i := strings.LastIndex(key, "/"); i > 0; i = strings.LastIndex(key[:i], "/") {
		f.dirs[key[:i]] = struct{}{}
	}
}

func (f *fakeUserVolE2EStorage) Put(_ context.Context, key string, r io.Reader) (*baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.files[key] = data
	f.markDirs(key)
	return &baidupcs.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeUserVolE2EStorage) Get(_ context.Context, key string) (io.ReadCloser, *baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[key]
	if !ok {
		return nil, nil, baidupcs.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), &baidupcs.ObjectMeta{Key: key, Size: int64(len(data))}, nil
}

func (f *fakeUserVolE2EStorage) Stat(_ context.Context, key string) (*baidupcs.ObjectMeta, error) {
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

func (f *fakeUserVolE2EStorage) List(_ context.Context, prefix string) ([]baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []baidupcs.ObjectMeta{}
	for k := range f.files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		out = append(out, baidupcs.ObjectMeta{Key: k, Size: int64(len(f.files[k]))})
	}
	for d := range f.dirs {
		if strings.HasPrefix(d, prefix) {
			out = append(out, baidupcs.ObjectMeta{Key: d, IsDir: true})
		}
	}
	return out, nil
}

func (f *fakeUserVolE2EStorage) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, key)
	delete(f.dirs, key)
	return nil
}

func (f *fakeUserVolE2EStorage) Copy(_ context.Context, srcKey, dstKey string) (*baidupcs.ObjectMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.files[srcKey]
	if !ok {
		return nil, baidupcs.ErrNotFound
	}
	f.files[dstKey] = append([]byte(nil), data...)
	f.markDirs(dstKey)
	return &baidupcs.ObjectMeta{Key: dstKey, Size: int64(len(data))}, nil
}

// userVolE2EBackend 是 e2e ExternalBackend（持有真 StorageFS）。
type userVolE2EBackend struct {
	fs syncpkg.FS
}

func (b *userVolE2EBackend) FS() syncpkg.FS { return b.fs }
func (b *userVolE2EBackend) Close() error   { return nil }

// userVolE2EFakeExecutor 是 fake syncmgr.Executor：direction=push 时用 pkg/sync.Engine
// 把本地 LocalFS（task.Owner 租户根）同步到用户卷 FS（闭包查 volSet.External）。
type userVolE2EFakeExecutor struct {
	volSet       *registry.Set
	ownerRootFor func(owner string) string
}

func (e *userVolE2EFakeExecutor) Run(ctx context.Context, task *syncmgr.SyncTask, remote syncmgr.RemoteConfig) (*syncmgr.RunResult, error) {
	if task.Direction != string(syncmgr.DirectionPush) {
		return &syncmgr.RunResult{Status: string(syncmgr.StatusFailed), Error: "e2e fake executor: only push supported"}, nil
	}
	be := e.volSet.External(remote.Volume)
	if be == nil {
		return &syncmgr.RunResult{Status: string(syncmgr.StatusFailed), Error: fmt.Sprintf("e2e: 用户卷 %q 未注册", remote.Volume)}, nil
	}
	dstFS := be.FS()
	localRoot := e.ownerRootFor(task.Owner)
	job := &syncpkg.Job{
		ID:             task.ID,
		Direction:      syncpkg.DirectionPush,
		Src:            task.Src,
		Dst:            task.Dst,
		Recursive:      task.Recursive,
		ConflictPolicy: syncpkg.ConflictPolicy(task.ConflictPolicy),
	}
	engine := &syncpkg.Engine{}
	if err := engine.Sync(ctx, syncpkg.NewLocalFS(localRoot, nil), dstFS, job); err != nil {
		return &syncmgr.RunResult{Status: string(syncmgr.StatusFailed), Error: err.Error()}, nil
	}
	return &syncmgr.RunResult{
		Status:     string(syncmgr.StatusCompleted),
		FilesTotal: job.Stats.FilesTotal,
		FilesDone:  job.Stats.FilesDone,
	}, nil
}

// waitUserVolTaskStatus 轮询任务状态到目标（completed/failed/cancelled）或超时。
func waitUserVolTaskStatus(t *testing.T, mgr *syncmgr.Manager, owner, want string) syncmgr.SyncTaskMeta {
	t.Helper()
	var got syncmgr.SyncTaskMeta
	testutil.WaitFor(t, 15*time.Second, func() bool {
		for _, m := range mgr.List(owner) {
			if m.Status == want {
				got = m
				return true
			}
			if m.Status == string(syncmgr.StatusFailed) || m.Status == string(syncmgr.StatusCancelled) {
				got = m
				return true
			}
		}
		return false
	}, "wait task status")
	return got
}

// mkTestFile 写本地测试文件（递归建目录）。
func mkTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
}

// withActorContext 绑定请求 actor（用户卷 handler owner 派生）。
func withActorContext(r *http.Request, actor string) *http.Request {
	return r.WithContext(withActor(r.Context(), actor))
}

// ioReadAll 读全部（测试辅助，减少重复）。
func ioReadAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

// TestUserVolumeE2E_FullChain 全链路：创建 → push → 完成 → 删除。
func TestUserVolumeE2E_FullChain(t *testing.T) {
	t.Parallel()
	registerUserVolE2EBackend()

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)

	volSet := h.Volumes()
	exec := &userVolE2EFakeExecutor{
		volSet: volSet,
		ownerRootFor: func(owner string) string {
			// 对齐生产 tenantRoot：本地同步根是 user 桶（<storageRoot>/<owner>/user），
			// 不含 meta/ 等其它桶（否则 meta/volume JSON 会被同步进网盘）。
			return cfg.StorageRoot + "/" + owner + "/user"
		},
	}

	// remote 名 = 用户卷名（U3 疑虑 3 约定）。
	remotes := []syncmgr.RemoteConfig{
		{Name: "alice-disk1", Kind: syncmgr.RemoteKindBaidupcs, Volume: "alice-disk1"},
	}
	tenantRoot := func(owner string) (string, string, bool) { return cfg.StorageRoot, owner, true }
	mgr := syncmgr.NewManager(tenantRoot, nil, nil, 0, remotes, exec, nil, &syncmgr.Config{MaxConcurrent: 3, TaskTTL: 0})
	t.Cleanup(mgr.Stop)
	mgr.SetUserVolumeOwner(func(owner, volumeName string) bool {
		// 1. 用户卷：store 有且 Owner == owner → 归属。
		v, gErr := store.Get(owner, volumeName)
		if gErr == nil && v != nil && v.Owner == owner {
			return true
		}
		// 2. 系统盘：Set.External 有，且该卷名**不属于任何用户卷**（排除用户卷——
		//    Set.External 同时含系统盘与用户卷；动态 ScanRestore 取全局用户卷名，
		//    任务创建低频可接受；优化空间：API 创建/删除时更新快照）。
		isUserVol := false
		if allUVs, sErr := store.ScanRestore(); sErr == nil {
			for _, uv := range allUVs {
				if uv.Name == volumeName {
					isUserVol = true
					break
				}
			}
		}
		if !isUserVol && volSet != nil && volSet.External(volumeName) != nil {
			return true
		}
		// 3. 其它（未知卷/跨 owner 用户卷）→ 拒绝（404 防枚举语义）。
		return false
	})
	h.SetSyncMgr(mgr)

	mux := userVolWrap(h, "alice")
	// 1. 创建用户卷（fake backend：local_root = t.TempDir()）。
	volLocalRoot := t.TempDir()
	rec := postUserVolume(t, mux, map[string]any{
		"name": "alice-disk1", "type": userVolE2EType,
		"extra": map[string]any{"local_root": volLocalRoot},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("创建用户卷: got %d body=%s", rec.Code, rec.Body.String())
	}
	if be := volSet.External("alice-disk1"); be == nil {
		t.Fatal("Set.External(alice-disk1) = nil（创建后应注册）")
	}

	// 2. 本地源文件 → push 任务到用户卷（本地根 = user 桶）。
	mkTestFile(t, cfg.StorageRoot+"/alice/user", "hello.txt", "alice-content")
	task, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{
		Direction: "push", Remote: "alice-disk1", Src: "", Dst: "",
		Recursive: true, ConflictPolicy: "skip", Owner: "alice",
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	_ = task
	// 3. 等任务完成（fake executor 同步引擎跑完）。
	done := waitUserVolTaskStatus(t, mgr, "alice", string(syncmgr.StatusCompleted))
	if done.FilesDone != 1 || done.FilesTotal != 1 {
		t.Fatalf("任务统计: total=%d done=%d, want 1/1", done.FilesTotal, done.FilesDone)
	}
	// 文件进入用户卷 FS（OpenRead 往返）。
	be := volSet.External("alice-disk1")
	rc, err := be.FS().OpenRead(context.Background(), "hello.txt")
	if err != nil {
		t.Fatalf("OpenRead 用户卷 hello.txt: %v", err)
	}
	got, _ := ioReadAll(rc)
	_ = rc.Close()
	if string(got) != "alice-content" {
		t.Fatalf("用户卷 hello.txt = %q, want %q", string(got), "alice-content")
	}

	// 4. 删除（任务已 completed，无活跃引用 → 成功）。
	del := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name=alice-disk1", nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, withActorContext(del, "alice"))
	if delRec.Code != http.StatusOK {
		t.Fatalf("删除用户卷: got %d body=%s", delRec.Code, delRec.Body.String())
	}
	if be := volSet.External("alice-disk1"); be != nil {
		t.Fatal("删除后 Set.External(alice-disk1) 应清空")
	}
	if v, _ := store.Get("alice", "alice-disk1"); v != nil {
		t.Fatal("删除后 store.Get 应不存在")
	}
}

// TestUserVolumeE2E_CrossOwnerDenied 跨 owner 创建任务 → 拒绝（U4 闭包）。
func TestUserVolumeE2E_CrossOwnerDenied(t *testing.T) {
	t.Parallel()
	registerUserVolE2EBackend()

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	store := NewUserVolumeStore(cfg.StorageRoot)
	h.SetUserVolumeStore(store)
	volSet := h.Volumes()

	remotes := []syncmgr.RemoteConfig{
		{Name: "bob-disk1", Kind: syncmgr.RemoteKindBaidupcs, Volume: "bob-disk1"},
	}
	tenantRoot := func(owner string) (string, string, bool) { return cfg.StorageRoot, owner, true }
	mgr := syncmgr.NewManager(tenantRoot, nil, nil, 0, remotes, nil, nil, &syncmgr.Config{MaxConcurrent: 3, TaskTTL: 0})
	t.Cleanup(mgr.Stop)
	mgr.SetUserVolumeOwner(func(owner, volumeName string) bool {
		// 1. 用户卷：store 有且 Owner == owner → 归属。
		v, gErr := store.Get(owner, volumeName)
		if gErr == nil && v != nil && v.Owner == owner {
			return true
		}
		// 2. 系统盘：Set.External 有，且该卷名**不属于任何用户卷**（排除用户卷——
		//    Set.External 同时含系统盘与用户卷；动态 ScanRestore 取全局用户卷名，
		//    任务创建低频可接受；优化空间：API 创建/删除时更新快照）。
		isUserVol := false
		if allUVs, sErr := store.ScanRestore(); sErr == nil {
			for _, uv := range allUVs {
				if uv.Name == volumeName {
					isUserVol = true
					break
				}
			}
		}
		if !isUserVol && volSet != nil && volSet.External(volumeName) != nil {
			return true
		}
		// 3. 其它（未知卷/跨 owner 用户卷）→ 拒绝（404 防枚举语义）。
		return false
	})
	h.SetSyncMgr(mgr)

	// bob 的用户卷存在，但 alice 用 bob 的卷名创建任务 → 拒绝（跨 owner 404 语义）。
	mux := userVolWrap(h, "bob")
	rec := postUserVolume(t, mux, map[string]any{
		"name": "bob-disk1", "type": userVolE2EType,
		"extra": map[string]any{"local_root": t.TempDir()},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("bob 创建: %d %s", rec.Code, rec.Body.String())
	}
	_, _, err := mgr.SubmitAndStart(syncmgr.CreateRequest{
		Direction: "push", Remote: "bob-disk1", Src: "", Dst: "", Owner: "alice",
	})
	if err == nil {
		t.Fatal("跨 owner 创建任务应被拒绝（ErrUserVolumeNotOwned）")
	}
	if !strings.Contains(err.Error(), "不属于当前用户") {
		t.Fatalf("错误文案: %v", err)
	}
}
