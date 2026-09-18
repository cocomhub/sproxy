// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

// executor_mesh_fs_test.go 钉住 P3-d 的**接缝**：`kind=mesh` 的远端由装配层注入的
// **mesh FS 工厂**构造（`pkg/syncexec` 不得依赖 `pkg/tunnel/mesh` 子 module、也不自己拨号），
// 未注入时保持 fail-closed（既有 `ErrMeshTransportNotWired`，绝不回落 direct）。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// fakeMeshFS 是最小内存 `sync.FS`（只实现 push 需要的读写，其余返回明确错误）。
type fakeMeshFS struct {
	mu    sync.Mutex
	files map[string]string
	dirs  map[string]bool
}

func newFakeMeshFS() *fakeMeshFS {
	return &fakeMeshFS{files: map[string]string{}, dirs: map[string]bool{}}
}

func (f *fakeMeshFS) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) {
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

func (f *fakeMeshFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
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

func (f *fakeMeshFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, ok := f.files[p]
	if !ok {
		return nil, fmt.Errorf("not found: %s", p)
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func (f *fakeMeshFS) WriteFile(_ context.Context, p string, r io.Reader, _ int64, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = string(b)
	return nil
}

func (f *fakeMeshFS) Rename(_ context.Context, from, to string) error {
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

func (f *fakeMeshFS) Delete(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, p)
	return nil
}

func (f *fakeMeshFS) MakeDir(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs[p] = true
	return nil
}

func (f *fakeMeshFS) content(p string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.files[p]
	return c, ok
}

const meshTestPin = "sha256:" + "3f2a1b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708"

// TestExecutor_MeshKind_UsesInjectedFactory 钉住：装配层注入 mesh FS 工厂后，`kind=mesh`
// 的推送经由该 FS 完成（工厂收到完整远端配置，任务结束调用 close）。
func TestExecutor_MeshKind_UsesInjectedFactory(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "hello mesh push")

	fake := newFakeMeshFS()
	var (
		mu      sync.Mutex
		gotCfg  syncmgr.RemoteConfig
		closed  bool
		calls   int
		closedC = make(chan struct{})
	)
	exec.SetMeshFSFactory(func(_ context.Context, rc syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		mu.Lock()
		gotCfg = rc
		calls++
		mu.Unlock()
		return fake, func() {
			mu.Lock()
			closed = true
			mu.Unlock()
			close(closedC)
		}, nil
	})

	task := &syncmgr.SyncTask{ID: "t-mesh", Direction: "push", Remote: "r-mesh", Src: "", Dst: "", ConflictPolicy: "skip"}
	rc := syncmgr.RemoteConfig{
		Name: "r-mesh", Kind: syncmgr.RemoteKindMesh,
		Node: "nodeB", Volume: "main", PeerPins: []string{meshTestPin}, Transport: "relay",
	}
	res, err := exec.Run(context.Background(), task, rc)
	if err != nil {
		t.Fatalf("注入工厂后 mesh 推送应成功: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed, got %q", res.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("工厂应被调用 1 次, got %d", calls)
	}
	// 工厂必须收到**完整**远端配置（否则装配层无法按 node/volume/pins/transport 拨号）。
	if gotCfg.Node != "nodeB" || gotCfg.Volume != "main" || gotCfg.Transport != "relay" ||
		len(gotCfg.PeerPins) != 1 || gotCfg.PeerPins[0] != meshTestPin {
		t.Fatalf("工厂收到的配置不完整: %+v", gotCfg)
	}
	if got, ok := fake.content("a.txt"); !ok || got != "hello mesh push" {
		t.Fatalf("mesh FS 上应出现 a.txt 且内容正确: %q ok=%v", got, ok)
	}
	select {
	case <-closedC:
	case <-time.After(3 * time.Second):
		t.Fatal("任务结束应调用工厂返回的 close（关闭 mesh 链路）")
	}
	if !closed {
		t.Fatal("close 状态未置位")
	}
}

// TestExecutor_MeshKind_FactoryErrorPropagates 钉住工厂错误原样上抛（带远端名），
// **不得**被吞掉后回落 direct。
func TestExecutor_MeshKind_FactoryErrorPropagates(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "x")

	sentinel := errors.New("拨号失败：未宣告服务 volwrite")
	exec.SetMeshFSFactory(func(context.Context, syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		return nil, nil, sentinel
	})

	task := &syncmgr.SyncTask{ID: "t-mesh-err", Direction: "push", Remote: "r-mesh", Src: "", Dst: "", ConflictPolicy: "skip"}
	rc := syncmgr.RemoteConfig{
		Name: "r-mesh", Kind: syncmgr.RemoteKindMesh,
		Node: "nodeB", Volume: "main", PeerPins: []string{meshTestPin},
	}
	_, err := exec.Run(context.Background(), task, rc)
	if err == nil {
		t.Fatal("工厂报错时任务必须失败")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("应保留工厂错误（errors.Is 可判定）, got %v", err)
	}
	if errors.Is(err, ErrMeshTransportNotWired) {
		t.Fatal("已注入工厂，不应再报「未装配」——说明回落了旧分支")
	}
}

// TestExecutor_MeshKind_NoFactoryStillFailClosed 钉住未注入工厂时仍是明确的
// `ErrMeshTransportNotWired`（绝不回落 direct）——与既有 TestExecutor_MeshKind_NotWired
// 互补：那条走 Run 全链路，本条直接钉接缝取值。
func TestExecutor_MeshKind_NoFactoryStillFailClosed(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	_, _, err := exec.newRemoteFS(context.Background(), syncmgr.RemoteConfig{
		Name: "r-mesh", Kind: syncmgr.RemoteKindMesh, Node: "nodeB", Volume: "main",
		PeerPins: []string{meshTestPin},
	}, "")
	if !errors.Is(err, ErrMeshTransportNotWired) {
		t.Fatalf("未注入工厂应返回 ErrMeshTransportNotWired, got %v", err)
	}
}
