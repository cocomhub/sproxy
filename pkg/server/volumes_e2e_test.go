// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volumes_e2e_test.go 是 V3 框架的端到端验证：外部后端（plugin 注册）→ 装配
// （assembleVolumes 按 Type 分派）→ registry.Set 持有 → 通过 Set.External 取回 →
// FS() 同步视图**真实可用**（调用 sync.FS 方法往返数据）。
//
// 与 T3 的单测（构造参数透传/roots 不含外部卷/pool 仍建）互补：T3 证明「装配分派正确」，
// 本文件证明「外部卷的同步视图真的能驱动 sync 语义」（WriteFile → ListDir → OpenRead 往返）。

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// e2eFSV3 是 V3 e2e 用的内存 sync.FS：支持 WriteFile/ListDir/OpenRead/Stat，
// 足以断言外部卷同步视图可用（往返数据 + 目录列举）。
type e2eFSV3 struct {
	files map[string]string
	dirs  map[string]bool
}

var _ syncpkg.FS = (*e2eFSV3)(nil)

func newE2EFSV3() *e2eFSV3 { return &e2eFSV3{files: map[string]string{}, dirs: map[string]bool{}} }

func (f *e2eFSV3) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) {
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
		out = append(out, syncpkg.Entry{Path: strings.TrimSuffix(prefix+top, "/"), IsDir: strings.Contains(rest, "/")})
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
		out = append(out, syncpkg.Entry{Path: strings.TrimSuffix(prefix+top, "/"), IsDir: true})
	}
	return out, nil
}

func (f *e2eFSV3) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	if p == "" {
		return &syncpkg.Entry{Name: "", Path: "", IsDir: true}, nil
	}
	if _, ok := f.files[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, IsDir: false}, nil
	}
	if _, ok := f.dirs[p]; ok {
		return &syncpkg.Entry{Name: p, Path: p, IsDir: true}, nil
	}
	return nil, nil
}

func (f *e2eFSV3) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	content, ok := f.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (f *e2eFSV3) WriteFile(_ context.Context, p string, r io.Reader, _ int64, _ int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.files[p] = string(data)
	// 隐式建父目录标记（父路径全建）。
	rest := p
	for {
		i := strings.LastIndex(rest, "/")
		if i <= 0 {
			break
		}
		rest = rest[:i]
		f.dirs[rest] = true
	}
	return nil
}

func (f *e2eFSV3) Rename(_ context.Context, from, to string) error {
	content, ok := f.files[from]
	if !ok {
		return os.ErrNotExist
	}
	delete(f.files, from)
	f.files[to] = content
	return nil
}

func (f *e2eFSV3) Delete(_ context.Context, p string) error {
	delete(f.files, p)
	delete(f.dirs, p)
	return nil
}

func (f *e2eFSV3) MakeDir(_ context.Context, p string) error {
	f.dirs[p] = true
	return nil
}

// e2eBackendV3 是 V3 e2e 的 fake 外部后端：持有内存 sync.FS，Close 幂等。
type e2eBackendV3 struct {
	fs     *e2eFSV3
	closed bool
}

func (b *e2eBackendV3) FS() syncpkg.FS { return b.fs }
func (b *e2eBackendV3) Close() error   { b.closed = true; return nil }

// registerE2EBackendV3 注册 e2e fake 后端（返回 unregister 语义——注册表不导出反注册，
// 类型名唯一即可）。
func registerE2EBackendV3(typ string) (*e2eBackendV3, func()) {
	be := &e2eBackendV3{fs: newE2EFSV3()}
	registry.RegisterBackend(typ, func(_ context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return be, nil
	})
	return be, func() {
		registry.UnregisterBackendForTest(typ)
	}
}

// TestVolumeFrameworkE2E_ExternalFS_Roundtrip 端到端验证外部卷同步视图可用：
// 装配 → Set.External 取回 → FS().WriteFile → ListDir → OpenRead 往返。
func TestVolumeFrameworkE2E_ExternalFS_Roundtrip(t *testing.T) {
	t.Parallel()
	typ := "fakev3e2e-roundtrip"
	_, unreg := registerE2EBackendV3(typ)
	defer unreg()

	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dir},
		{Name: "ext1", Type: typ, VolCapacity: 1000},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	t.Cleanup(func() { _ = vs.Close() })

	// 装配后 Set.External 取回外部后端 → FS 可用（非 nil）。
	got := vs.External("ext1")
	if got == nil {
		t.Fatal("External(ext1) = nil, want 外部后端")
	}
	fs := got.FS()
	if fs == nil {
		t.Fatal("外部后端 FS() = nil, want 同步视图")
	}

	// WriteFile → ListDir → OpenRead 往返（sync 语义真实可用）。
	if wErr := fs.WriteFile(context.Background(), "sub/a.txt", strings.NewReader("hello-e2e"), 9, 0); wErr != nil {
		t.Fatalf("WriteFile: %v", wErr)
	}
	if wErr := fs.WriteFile(context.Background(), "b.txt", strings.NewReader("world"), 5, 0); wErr != nil {
		t.Fatalf("WriteFile b.txt: %v", wErr)
	}
	entries, listErr := fs.ListDir(context.Background(), "")
	if listErr != nil {
		t.Fatalf("ListDir: %v", listErr)
	}
	found := map[string]bool{}
	for _, e := range entries {
		found[e.Path] = true
	}
	if !found["b.txt"] || !found["sub"] {
		t.Fatalf("ListDir 根条目 = %v, want b.txt + sub 目录", entries)
	}
	subEntries, err := fs.ListDir(context.Background(), "sub")
	if err != nil {
		t.Fatalf("ListDir sub: %v", err)
	}
	if len(subEntries) != 1 || subEntries[0].Path != "sub/a.txt" {
		t.Fatalf("ListDir sub = %v, want [sub/a.txt]", subEntries)
	}
	rc, err := fs.OpenRead(context.Background(), "sub/a.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello-e2e" {
		t.Fatalf("sub/a.txt = %q, want %q", string(data), "hello-e2e")
	}
}

// TestVolumeFrameworkE2E_ExternalFS_CloseIdempotent 验证外部后端随 Set.Close 统一关闭且幂等。
func TestVolumeFrameworkE2E_ExternalFS_CloseIdempotent(t *testing.T) {
	t.Parallel()
	typ := "fakev3e2e-close"
	be, unreg := registerE2EBackendV3(typ)
	defer unreg()

	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: dir},
		{Name: "ext1", Type: typ},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	vs, err := assembleVolumes(cfg, testLogger())
	if err != nil {
		t.Fatalf("assembleVolumes: %v", err)
	}
	if err := vs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !be.closed {
		t.Fatal("外部后端 Close 未被调用（Set.Close 应统一关闭 external）")
	}
	if err := vs.Close(); err != nil {
		t.Fatalf("Close 幂等: %v", err)
	}
}
