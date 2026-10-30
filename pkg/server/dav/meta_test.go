// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dav

// dav_meta_test.go 覆盖 WebDAV 挂载面 sidecar 联动的**目录级**语义（davMetaFS）：
// MOVE 目录时必须整棵迁移子 sidecar 子树（`meta/<from>/**` → `meta/<to>/**`）——
// 否则新位置逐分块校验静默失效（读侧退化为不校验），且旧子树成孤儿白占 meta 桶配额。

import (
	"context"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestDavMetaFS_RenameDirMigratesMetaSubtree 回归（本轮 B 面）：目录级 MOVE 迁移子 sidecar。
// 变异：去掉 Rename 里的 migrateMetaSubtree 调用 → 子 sidecar 仍在 meta/docs/** → 红。
func TestDavMetaFS_RenameDirMigratesMetaSubtree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })

	// 预置子 sidecar（内容无关紧要——本用例只验证**位置迁移**）与 user 桶目录（inner Rename 前置）。
	for _, p := range []string{
		filepath.Join(dir, "meta", "docs", "a.txt.meta"),
		filepath.Join(dir, "meta", "docs", "sub", "b.txt.meta"),
		filepath.Join(dir, "user", "docs", "sub"),
	} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", filepath.Dir(p), err)
		}
	}
	for _, p := range []string{
		filepath.Join(dir, "meta", "docs", "a.txt.meta"),
		filepath.Join(dir, "meta", "docs", "sub", "b.txt.meta"),
	} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", p, err)
		}
	}

	userRoot := filepath.Join(dir, "user")
	d := &davMetaFS{
		FS:    syncpkg.NewLocalFS(filepath.ToSlash(userRoot), testLogger()),
		deps:  Deps{Logger: testLogger()},
		owner: "",
		root:  root,
	}
	if err := d.Rename(context.Background(), "docs", "docs2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	for _, p := range []string{
		filepath.Join(dir, "meta", "docs2", "a.txt.meta"),
		filepath.Join(dir, "meta", "docs2", "sub", "b.txt.meta"),
	} {
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("子 sidecar 应迁移到新位置（变异：不迁移 → 红）: %s: %v", p, serr)
		}
	}
	for _, p := range []string{
		filepath.Join(dir, "meta", "docs", "a.txt.meta"),
		filepath.Join(dir, "meta", "docs", "sub", "b.txt.meta"),
	} {
		if _, serr := os.Stat(p); serr == nil {
			t.Fatalf("旧位置 sidecar 应已随迁不残留（孤儿占配额）: %s", p)
		}
	}
}

// TestDavMetaFS_CapabilityForwardingAndMetaLink 回归（M8，第 7 轮 MAJOR）：davMetaFS 必须
// 透传可选能力（Provider/Mover/Copier/Linker/RangeReader/DirectURL/WriteIfAbsent/
// LocalVolume/StagingQuota*），且**写面能力带 meta 重链**——此前 Move/Copy/Link 被嵌入
// FS 直通（目标无 sidecar、源成孤儿），Provider 等断言恒 false 静默降级。
// 变异：去掉 Move 里的 relinkAfterMove → 目标无 sidecar → 红。
func TestDavMetaFS_CapabilityForwardingAndMetaLink(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	userRoot := filepath.Join(dir, "user")
	base := syncpkg.NewLocalFS(filepath.ToSlash(userRoot), testLogger())
	provider := &capProviderFS{LocalFS: base, fm: &meta.FileMeta{
		Version: 1, Size: 1, TotalSHA256: strings.Repeat("a", 64), ChunkSize: 1,
	}}
	d := &davMetaFS{FS: provider, deps: Deps{Logger: testLogger(), Meta: testMetaPolicy{}}, owner: "", root: root}
	ctx := context.Background()

	// 能力断言（编译期已断言；运行期再核一层，防「实现了但语义落空」）。
	if _, ok := any(d).(syncpkg.WriteIfAbsent); !ok {
		t.Fatal("davMetaFS 应实现 WriteIfAbsent")
	}
	if _, ok := any(d).(syncpkg.RangeReader); !ok {
		t.Fatal("davMetaFS 应实现 RangeReader")
	}
	if _, ok := any(d).(syncpkg.DirectURLProvider); !ok {
		t.Fatal("davMetaFS 应实现 DirectURLProvider")
	}
	if pv, ok := any(d).(meta.Provider); !ok {
		t.Fatal("davMetaFS 应实现 meta.Provider")
	} else if fm, ferr := pv.FileMeta(ctx, "x.bin"); ferr != nil || fm == nil {
		t.Fatalf("FileMeta 应透传内层: fm=%v err=%v", fm, ferr)
	}
	if !d.IsLocalVolume() {
		t.Fatal("IsLocalVolume 应透传内层（capProviderFS 自述本地）")
	}

	// 写面 + Move 重链：先写（生成 sidecar），再 Move → 目标必须有 sidecar、源不得残留。
	if werr := d.WriteFile(ctx, syncpkg.LogicalKey{}, "a.bin", strings.NewReader("hello"), 5, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if _, oerr := root.Stat(meta.MetaPath("user/a.bin")); oerr != nil {
		t.Fatalf("写后应有 sidecar: %v", oerr)
	}
	if merr := d.Move(ctx, "a.bin", "b.bin"); merr != nil {
		t.Fatalf("Move: %v", merr)
	}
	if _, oerr := root.Stat(meta.MetaPath("user/b.bin")); oerr != nil {
		t.Fatalf("Move 后目标应有 sidecar（M8：写面能力须带 meta 重链）: %v", oerr)
	}
	if _, oerr := root.Stat(meta.MetaPath("user/a.bin")); oerr == nil {
		t.Fatal("Move 后源 sidecar 应随迁不残留")
	}
}

// capProviderFS 是「实现 meta.Provider + LocalVolume + Mover/Copier/Linker」的替身
// （LocalFS 已实现 Mover/Copier/Linker），用于验证 davMetaFS 的透传与重链。
type capProviderFS struct {
	*syncpkg.LocalFS // 具体类型嵌入：连带提升 Mover/Copier/Linker/RangeReader 等可选能力
	fm               *meta.FileMeta
}

func (c *capProviderFS) FileMeta(context.Context, string) (*meta.FileMeta, error) { return c.fm, nil }

func (c *capProviderFS) IsLocalVolume() bool { return true }

// testLogger 返回丢弃输出的 slog.Logger（本包测试用）。
func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// testMetaPolicy 是 MetaPolicy 的测试替身：只把 sidecar 落到 meta/<rel>.meta，
// 用于验证 davMetaFS 的「写/移动后联动策略」接线（真实策略由 pkg/server 覆盖）。
type testMetaPolicy struct{}

func (testMetaPolicy) WriteMeta(_ context.Context, _ string, root *storage.Root, rel string, _ map[string]any) error {
	p := meta.MetaPath(rel)
	if err := root.MkdirAll(path.Dir(p), 0o755); err != nil {
		return err
	}
	w, err := root.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	_, err = w.Write([]byte("{}"))
	return err
}

func (testMetaPolicy) SweepMetaTmp(*storage.Root, string) {}
