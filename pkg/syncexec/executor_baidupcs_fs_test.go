// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

// executor_baidupcs_fs_test.go 钉住 P4 的**接缝**：`kind=baidupcs` 的远端由装配层注入的
// **baidupcs FS 工厂**构造（`pkg/syncexec` 不得依赖 `pkg/baidupcs` 具体类型、也不自己装配
// 网盘卷——卷名解析留在装配层），未注入时保持 fail-closed（`ErrBaidupcsNotWired`，
// 绝不回落 direct）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// fakeBaidupcsFS 是最小内存 `sync.FS`（仿 fakeMeshFS，只实现 push/pull 需要的读写）。
type fakeBaidupcsFS struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]bool
}

func newFakeBaidupcsFS() *fakeBaidupcsFS {
	return &fakeBaidupcsFS{files: map[string]string{}, dirs: map[string]bool{}}
}

func (f *fakeBaidupcsFS) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := p
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]bool{}
	out := []syncpkg.Entry{}
	for name := range f.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		top, _, _ := strings.Cut(rest, "/")
		if seen[top] {
			continue
		}
		seen[top] = true
		out = append(out, syncpkg.Entry{Path: path.Join(p, top), IsDir: strings.Contains(rest, "/")})
	}
	for d := range f.dirs {
		if !strings.HasPrefix(d, prefix) {
			continue
		}
		rest := strings.TrimPrefix(d, prefix)
		top, _, _ := strings.Cut(rest, "/")
		if seen[top] {
			continue
		}
		seen[top] = true
		out = append(out, syncpkg.Entry{Path: path.Join(p, top), IsDir: true})
	}
	return out, nil
}

func (f *fakeBaidupcsFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p == "" {
		return &syncpkg.Entry{Path: "", IsDir: true}, nil
	}
	if body, ok := f.files[p]; ok {
		return &syncpkg.Entry{Path: p, Size: int64(len(body))}, nil
	}
	if f.dirs[p] {
		return &syncpkg.Entry{Path: p, IsDir: true}, nil
	}
	return nil, nil
}

func (f *fakeBaidupcsFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (f *fakeBaidupcsFS) WriteFile(_ context.Context, p string, r io.Reader, _ int64, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = string(b)
	return nil
}

func (f *fakeBaidupcsFS) Rename(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.files[from]
	if !ok {
		return fmt.Errorf("not found: %s", from)
	}
	delete(f.files, from)
	f.files[to] = body
	return nil
}

func (f *fakeBaidupcsFS) Delete(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}

func (f *fakeBaidupcsFS) MakeDir(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = true
	return nil
}

func (f *fakeBaidupcsFS) content(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[p]
	return c, ok
}

// baidupcsRemote 返回一条完整可用的 baidupcs 远端配置（无 URL、无凭据；本机网盘卷）。
func baidupcsRemote(name string) syncmgr.RemoteConfig {
	return syncmgr.RemoteConfig{
		Name: name, Kind: syncmgr.RemoteKindBaidupcs,
		Volume: "mydisk",
	}
}

// volumeRemote 返回一条通用本机卷远端配置（kind=volume：WebDAV/baidupcs 统一走此）。
func volumeRemote(name string) syncmgr.RemoteConfig {
	return syncmgr.RemoteConfig{
		Name: name, Kind: syncmgr.RemoteKindVolume,
		Volume: "any-volume",
	}
}

// TestExecutor_Run_BaidupcsRemote 钉住 `kind=baidupcs` 远端经注入工厂构造 FS：
// push 上传到 fake 网盘卷、pull 从 fake 网盘卷拉取到本地，双向跑通；工厂只服务
// baidupcs 远端（不串道）。
func TestExecutor_Run_BaidupcsRemote(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "baidupcs push payload")

	fake := newFakeBaidupcsFS()
	var factoryCalls int
	exec.SetBaidupcsFSFactory(func(_ context.Context, rc2 syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		factoryCalls++
		if rc2.Name != "r-bd" {
			t.Errorf("工厂只应服务 baidupcs 远端, got %q", rc2.Name)
		}
		if rc2.Volume != "mydisk" {
			t.Errorf("工厂应拿到卷名 mydisk, got %q", rc2.Volume)
		}
		return fake, func() {}, nil
	})

	rc := baidupcsRemote("r-bd")

	// push：本地 → 网盘卷。
	pushRes, pushErr := exec.Run(context.Background(), &syncmgr.SyncTask{
		ID: "t-bd-push", Direction: "push", Remote: "r-bd", Src: "a.txt", Dst: "a.txt", ConflictPolicy: "skip",
	}, rc)
	if pushErr != nil {
		t.Fatalf("baidupcs 推送: %v", pushErr)
	}
	if pushRes.Status != "completed" {
		t.Fatalf("push 状态应为 completed, got %q", pushRes.Status)
	}
	if got, ok := fake.content("a.txt"); !ok || got != "baidupcs push payload" {
		t.Fatalf("网盘卷应收到 a.txt 且内容正确: %q ok=%v", got, ok)
	}
	if factoryCalls != 1 {
		t.Fatalf("push 工厂应被调用 1 次, got %d", factoryCalls)
	}

	// pull：网盘卷 → 本地。
	if wErr := fake.WriteFile(context.Background(), "b.txt", strings.NewReader("baidupcs pull payload"), 0, 0); wErr != nil {
		t.Fatalf("预置网盘卷文件: %v", wErr)
	}
	pullRes, pullErr := exec.Run(context.Background(), &syncmgr.SyncTask{
		ID: "t-bd-pull", Direction: "pull", Remote: "r-bd", Src: "b.txt", Dst: "b.txt", ConflictPolicy: "skip",
	}, rc)
	if pullErr != nil {
		t.Fatalf("baidupcs 拉取: %v", pullErr)
	}
	if pullRes.Status != "completed" {
		t.Fatalf("pull 状态应为 completed, got %q", pullRes.Status)
	}
	if got := readLocalFile(t, userRootFor(base, ""), "b.txt"); got != "baidupcs pull payload" {
		t.Fatalf("本地应收到 b.txt 且内容正确: %q", got)
	}
	if factoryCalls != 2 {
		t.Fatalf("pull 工厂应再被调用 1 次（共 2 次）, got %d", factoryCalls)
	}
}

// TestExecutor_Run_Baidupcs_NilFSFromFactory 钉住装配错误：工厂返回空 FS → 明确报错。
func TestExecutor_Run_Baidupcs_NilFSFromFactory(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "payload")

	exec.SetBaidupcsFSFactory(func(context.Context, syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		return nil, nil, nil
	})

	rc := baidupcsRemote("r-bd")
	_, err := exec.Run(context.Background(), &syncmgr.SyncTask{
		ID: "t-bd-nilfs", Direction: "push", Remote: "r-bd", Src: "a.txt", Dst: "a.txt", ConflictPolicy: "skip",
	}, rc)
	if err == nil {
		t.Fatal("工厂返回空 FS 应报错（装配错误）")
	}
	if !strings.Contains(err.Error(), "空 FS") {
		t.Fatalf("错误应点名空 FS 装配错误: %v", err)
	}
}

// TestExecutor_Run_Baidupcs_NoFactory 钉住 fail-closed：未注入工厂时 kind=baidupcs 远端
// 明确报 ErrBaidupcsNotWired，**绝不回落 direct**。
func TestExecutor_Run_Baidupcs_NoFactory(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "payload")

	rc := baidupcsRemote("r-bd")
	_, err := exec.Run(context.Background(), &syncmgr.SyncTask{
		ID: "t-bd-nof", Direction: "push", Remote: "r-bd", Src: "a.txt", Dst: "a.txt", ConflictPolicy: "skip",
	}, rc)
	if err == nil {
		t.Fatal("未注入工厂应报错（fail-closed）")
	}
	if !errors.Is(err, ErrBaidupcsNotWired) {
		t.Fatalf("错误应可判定为 ErrBaidupcsNotWired: %v", err)
	}
}

// TestExecutor_Run_KindVolume 钉住 `kind=volume`（通用本机卷）远端经注入工厂构造 FS：
// kind=baidupcs 归一后与 volume 同构（工厂同一入口）；push 上传到 fake 卷。
func TestExecutor_Run_KindVolume(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "volume push payload")

	fake := newFakeBaidupcsFS()
	var factoryCalls int
	exec.SetBaidupcsFSFactory(func(_ context.Context, rc2 syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		factoryCalls++
		if rc2.Volume != "any-volume" {
			t.Errorf("工厂应按卷名服务, got volume %q", rc2.Volume)
		}
		return fake, func() {}, nil
	})

	rc := volumeRemote("r-vol")
	task := &syncmgr.SyncTask{ID: "t-vol", Direction: "push", Remote: "r-vol", Src: "a.txt", Dst: "a.txt", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, rc)
	if err != nil {
		t.Fatalf("volume 推送应成功: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态 = %q, want completed", res.Status)
	}
	if factoryCalls != 1 {
		t.Fatalf("工厂应被调用 1 次, got %d", factoryCalls)
	}
	rc2, err := fake.OpenRead(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("fake 卷读回: %v", err)
	}
	defer rc2.Close()
	data, rErr := io.ReadAll(rc2)
	if rErr != nil {
		t.Fatalf("fake 卷读回内容: %v", rErr)
	}
	if string(data) != "volume push payload" {
		t.Fatalf("fake 卷 a.txt = %q, want 原始 payload", string(data))
	}
}

// TestExecutor_Run_KindVolume_NoFactory 钉住 fail-closed：kind=volume 未注入工厂 →
// 明确报 ErrVolumeNotWired（与 baidupcs 别名同一错误，errors.Is 兼容），**绝不回落 direct**。
func TestExecutor_Run_KindVolume_NoFactory(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "payload")

	rc := volumeRemote("r-vol")
	_, err := exec.Run(context.Background(), &syncmgr.SyncTask{
		ID: "t-vol-nof", Direction: "push", Remote: "r-vol", Src: "a.txt", Dst: "a.txt", ConflictPolicy: "skip",
	}, rc)
	if err == nil {
		t.Fatal("未注入工厂应报错（fail-closed）")
	}
	if !errors.Is(err, ErrVolumeNotWired) {
		t.Fatalf("错误应可判定为 ErrVolumeNotWired: %v", err)
	}
}
