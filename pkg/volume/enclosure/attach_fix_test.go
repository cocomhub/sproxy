// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// TestFix_LedgerOnLocalRootForExternalSource：外部底层（源 FS 未实现 LocalVolume ⇒ 本地性
// 探测 false）时，封装卷账本落**节点本地**（默认卷根 meta/quota/<卷名>），不得落源视图
// （外部后端/加密 blob 不可解析 ⇒ 恒 degraded 拒写）。
// 变异：去掉 attach.go 的本地账本分支（回退 NewVolumeCounter(ledgerFS...)）→ 账本落
// 外部源视图 → 红。
func TestFix_LedgerOnLocalRootForExternalSource(t *testing.T) {
	t.Parallel()
	newCaptureWrapper(t, "capture_extsrc")
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	// extvol = 外部源后端（内存 FS，未实现 LocalVolume），vault = 封装声明 source=extvol:a。
	extBe := &extNilBackend{fs: newMemFS()}
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: dir},
		{Name: "extvol", Type: "extsrc"},
		{Name: "vault", Type: "capture_extsrc", Extra: map[string]any{"source": "extvol:a"}},
	}
	pools := map[string]*quota.Pool{"vault": quota.NewPool(1 << 20)}
	set := registry.NewSet(vols,
		map[string]*storage.Root{"main": rt},
		map[string]registry.ExternalBackend{"extvol": extBe},
		pools, "main")
	if aerr := AttachAll(context.Background(), set, nil); aerr != nil {
		t.Fatalf("AttachAll: %v", aerr)
	}
	be := set.External("vault")
	if be == nil {
		t.Fatal("封装卷后端应已挂回")
	}
	if werr := be.FS().WriteFile(context.Background(), storage.LogicalKey{}, "x.bin",
		strings.NewReader("12345678901234567890"), 20, 0); werr != nil {
		t.Fatalf("写 20B 应成功: %v", werr)
	}
	// 账本应在默认卷根的 meta/quota/vault 下（节点本地）。
	if _, serr := os.Stat(filepath.Join(dir, "meta", "quota", "vault")); serr != nil {
		t.Fatalf("外部源封装卷账本应落节点本地 meta/quota/vault: %v", serr)
	}
	_ = be.FS()
}

// TestFix_LedgerOnLocalFSForLocalSource：本地源（LocalFS 实现 LocalVolume）账本仍落源视图
// （跨进程可见性保留）——修复不得破坏本地封装卷的既有行为。
func TestFix_LedgerOnLocalFSForLocalSource(t *testing.T) {
	t.Parallel()
	newCaptureWrapper(t, "capture_localsrc")
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if merr := os.MkdirAll(filepath.Join(dir, "base"), 0o755); merr != nil {
		t.Fatal(merr)
	}
	vols := []volume.Volume{
		{Name: "main", Type: volume.TypeLocal, RootDir: dir},
		{Name: "vault", Type: "capture_localsrc", Extra: map[string]any{"source": "main:base"}},
	}
	pools := map[string]*quota.Pool{"vault": quota.NewPool(1 << 20)}
	set := registry.NewSet(vols, map[string]*storage.Root{"main": rt}, nil, pools, "main")
	if aerr := AttachAll(context.Background(), set, nil); aerr != nil {
		t.Fatalf("AttachAll: %v", aerr)
	}
	be := set.External("vault")
	if werr := be.FS().WriteFile(context.Background(), storage.LogicalKey{}, "y.bin",
		strings.NewReader("12345678901234567890"), 20, 0); werr != nil {
		t.Fatalf("写 20B 应成功: %v", werr)
	}
	// 本地源：账本仍在源视图 base/meta/quota（跨进程正确）。
	if entries, _ := os.ReadDir(filepath.Join(dir, "base", "meta", "quota")); len(entries) == 0 {
		t.Fatalf("本地源封装卷账本应落源视图（跨进程可见性）")
	}
}

// extNilBackend 外部后端替身（FS = 内存 FS，未实现 LocalVolume ⇒ 本地性探测 false）。
type extNilBackend struct{ fs syncpkg.FS }

func (b *extNilBackend) FS() syncpkg.FS { return b.fs }
func (b *extNilBackend) Close() error   { return nil }

// memFS 完整内存 sync.FS（模拟 baidupcs StorageFS：未实现 LocalVolume）。
type memFS struct {
	mu    sync.RWMutex
	files map[string][]byte
	dirs  map[string]bool
}

func newMemFS() *memFS {
	return &memFS{files: map[string][]byte{}, dirs: map[string]bool{"": true}}
}

func (m *memFS) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p = strings.Trim(p, "/")
	prefix := ""
	if p != "" {
		prefix = p + "/"
	}
	var out []syncpkg.Entry
	seen := map[string]bool{}
	for name := range m.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := strings.TrimPrefix(name, prefix)
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[:i]
		}
		if !seen[rest] {
			seen[rest] = true
			out = append(out, syncpkg.Entry{Name: rest, Path: p + "/" + rest, IsDir: m.dirs[prefix+rest] || !m.hasDirectFile(prefix+rest)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *memFS) hasDirectFile(key string) bool {
	_, ok := m.files[key]
	return ok
}

func (m *memFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p = strings.Trim(p, "/")
	if m.dirs[p] {
		return &syncpkg.Entry{Name: filepath.Base(p), Path: p, IsDir: true}, nil
	}
	b, ok := m.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return &syncpkg.Entry{Name: filepath.Base(p), Path: p, Size: int64(len(b)), MTime: time.Now().UnixNano()}, nil
}

func (m *memFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.files[strings.Trim(p, "/")]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memFS) WriteFile(_ context.Context, _ syncpkg.LogicalKey, p string, r io.Reader, n, _ int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p = strings.Trim(p, "/")
	b, err := io.ReadAll(io.LimitReader(r, n))
	if err != nil {
		return err
	}
	m.files[p] = b
	return nil
}

func (m *memFS) Rename(_ context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	from, to = strings.Trim(from, "/"), strings.Trim(to, "/")
	b, ok := m.files[from]
	if !ok {
		return os.ErrNotExist
	}
	delete(m.files, from)
	m.files[to] = b
	return nil
}

func (m *memFS) Delete(_ context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, strings.Trim(p, "/"))
	return nil
}

func (m *memFS) MakeDir(_ context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[strings.Trim(p, "/")] = true
	return nil
}
