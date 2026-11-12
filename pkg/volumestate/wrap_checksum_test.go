// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

// wrap_checksum_test.go 钉住 R46 S2：**本地卷**在列举 cache-miss 回源后自报 checksum
// （读 sidecar `TotalSHA256`），并**随目录快照缓存** ⇒ 命中路径零 I/O。
//
// 为什么只对本地卷：远端卷 sidecar 读是每文件一次网络往返（N 条目 = N 往返）⇒ 性能回归；
// 本地卷读本地小文件且随快照缓存（设计 docs/designs/2026-11-11-checksum-ledger-read-path.md §S2）。
//
// 变异：不做「仅本地卷」判定 ⇒ 远端用例红；不在 fetch 内填充（如仅在返回后填充）⇒
// 缓存命中仍重复读 sidecar ⇒ metaCalls==2 红。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// fakeMetaFS 是「可列举 + 可选本地性 + 可读 FileMeta」的替身（嵌入接口省掉无关方法）。
type fakeMetaFS struct {
	syncpkg.FS
	local     bool
	entries   []syncpkg.Entry
	listCalls int
	metaCalls int
	sidecar   map[string]string // rel → TotalSHA256（缺失 = 无 sidecar）
}

func (f *fakeMetaFS) IsLocalVolume() bool { return f.local }

func (f *fakeMetaFS) ListDir(_ context.Context, _ string) ([]syncpkg.Entry, error) {
	f.listCalls++
	out := make([]syncpkg.Entry, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

func (f *fakeMetaFS) FileMeta(_ context.Context, rel string) (*meta.FileMeta, error) {
	f.metaCalls++
	s, ok := f.sidecar[rel]
	if !ok {
		return nil, errors.New("no sidecar")
	}
	return &meta.FileMeta{TotalSHA256: s}, nil
}

func newChecksumEnv(t *testing.T, f *fakeMetaFS) syncpkg.FS {
	t.Helper()
	c := New(Options{})
	return Wrap("vol", f, c)
}

// TestCacheFS_ListDirFillsLocalChecksums 本地卷：空 checksum 由 sidecar 补齐，已有值不覆盖，
// 目录条目不动，sidecar 缺失留空。
func TestCacheFS_ListDirFillsLocalChecksums(t *testing.T) {
	t.Parallel()
	f := &fakeMetaFS{
		local: true,
		entries: []syncpkg.Entry{
			{Name: "a.txt", Path: "dir/a.txt"},
			{Name: "b.txt", Path: "dir/b.txt", Checksum: "preset", ChecksumType: "sha256"},
			{Name: "sub", IsDir: true},
			{Name: "c.txt", Path: "dir/c.txt"}, // 无 sidecar
		},
		sidecar: map[string]string{"alice/user/dir/a.txt": "sha-a", "alice/user/dir/b.txt": "sha-b-new"},
	}
	fsys := newChecksumEnv(t, f)
	entries, err := fsys.ListDir(context.Background(), "alice/user/dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if f.metaCalls != 2 {
		t.Fatalf("应只对空 checksum 的两个文件读 sidecar（a、c），实得 %d 次", f.metaCalls)
	}
	if entries[0].Checksum != "sha-a" || entries[0].ChecksumType != "sha256" || entries[0].Checksums["sha256"] != "sha-a" {
		t.Fatalf("a.txt 应由 sidecar 补齐 sha256，got %+v", entries[0])
	}
	if entries[1].Checksum != "preset" {
		t.Fatalf("已有 checksum 不得被覆盖，got %q", entries[1].Checksum)
	}
	if entries[2].Checksum != "" || entries[2].IsDir != true {
		t.Fatalf("目录条目不得填 checksum，got %+v", entries[2])
	}
	if entries[3].Checksum != "" {
		t.Fatalf("sidecar 缺失应留空（不伪造），got %q", entries[3].Checksum)
	}
}

// TestCacheFS_ListDirChecksumCached 命中缓存不得重复读 sidecar（填充必须发生在 fetch 内）。
func TestCacheFS_ListDirChecksumCached(t *testing.T) {
	t.Parallel()
	f := &fakeMetaFS{
		local:   true,
		entries: []syncpkg.Entry{{Name: "a.txt", Path: "dir/a.txt"}},
		sidecar: map[string]string{"alice/user/dir/a.txt": "sha-a"},
	}
	fsys := newChecksumEnv(t, f)
	ctx := context.Background()
	if _, err := fsys.ListDir(ctx, "alice/user/dir"); err != nil {
		t.Fatalf("ListDir#1: %v", err)
	}
	entries, err := fsys.ListDir(ctx, "alice/user/dir")
	if err != nil {
		t.Fatalf("ListDir#2: %v", err)
	}
	if f.listCalls != 1 {
		t.Fatalf("第二次应命中缓存（ListDir 只回源一次），实得 %d", f.listCalls)
	}
	if f.metaCalls != 1 {
		t.Fatalf("缓存命中不得重复读 sidecar，实得 %d 次", f.metaCalls)
	}
	if entries[0].Checksum != "sha-a" {
		t.Fatalf("缓存快照应保留 checksum，got %q", entries[0].Checksum)
	}
}

// TestCacheFS_ListDirSkipsRemoteChecksums 远端卷（IsLocalVolume=false）不得读 sidecar（防 N 次往返）。
func TestCacheFS_ListDirSkipsRemoteChecksums(t *testing.T) {
	t.Parallel()
	f := &fakeMetaFS{
		local:   false,
		entries: []syncpkg.Entry{{Name: "a.txt", Path: "dir/a.txt"}},
		sidecar: map[string]string{"alice/user/dir/a.txt": "sha-a"},
	}
	fsys := newChecksumEnv(t, f)
	entries, err := fsys.ListDir(context.Background(), "alice/user/dir")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if f.metaCalls != 0 {
		t.Fatalf("远端卷不得读 sidecar（每文件一次往返），实得 %d 次", f.metaCalls)
	}
	if entries[0].Checksum != "" {
		t.Fatalf("远端卷 checksum 应为空（由消费方 stat 兜底），got %q", entries[0].Checksum)
	}
}

// TestCacheFS_LocalChecksumPersisted 钉住「checksum 随快照落盘」：重启（新 Cache 实例）后
// 从磁盘命中仍带 checksum，且**不再读 sidecar**。
//
// 变异：把填充移到 `cache.Get` 之外（先缓存、后填充）⇒ 落盘快照不含 checksum ⇒ 本用例红。
func TestCacheFS_LocalChecksumPersisted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	st := NewStore(root, 1<<20, nil)
	f := &fakeMetaFS{
		local:   true,
		entries: []syncpkg.Entry{{Name: "a.txt", Path: "dir/a.txt"}},
		sidecar: map[string]string{"alice/user/dir/a.txt": "sha-a"},
	}
	c1 := New(Options{TTL: func(string) time.Duration { return time.Hour }, Store: st})
	if _, err := Wrap("v1", f, c1).ListDir(context.Background(), "alice/user/dir"); err != nil {
		t.Fatalf("ListDir#1: %v", err)
	}
	firstMetaCalls := f.metaCalls
	if firstMetaCalls == 0 {
		t.Fatal("首次回源应读 sidecar")
	}
	// 新实例（模拟重启，空内存 LRU）应从磁盘快照命中。
	c2 := New(Options{TTL: func(string) time.Duration { return time.Hour }, Store: NewStore(root, 1<<20, nil)})
	entries, err := Wrap("v1", f, c2).ListDir(context.Background(), "alice/user/dir")
	if err != nil {
		t.Fatalf("ListDir#2: %v", err)
	}
	if f.listCalls != 1 {
		t.Fatalf("重启后应从磁盘快照命中，got %d 次回源", f.listCalls)
	}
	if f.metaCalls != firstMetaCalls {
		t.Fatalf("磁盘命中不得读 sidecar，got %d 次（首次后 %d）", f.metaCalls, firstMetaCalls)
	}
	if entries[0].Checksum != "sha-a" {
		t.Fatalf("落盘快照必须带 checksum（否则每次命中都要重读 sidecar），got %q", entries[0].Checksum)
	}
}
