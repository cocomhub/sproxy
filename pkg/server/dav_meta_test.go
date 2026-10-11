// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// dav_meta_test.go 覆盖 WebDAV 挂载面 sidecar 联动的**目录级**语义（davMetaFS）：
// MOVE 目录时必须整棵迁移子 sidecar 子树（`meta/<from>/**` → `meta/<to>/**`）——
// 否则新位置逐分块校验静默失效（退化为直算），且旧子树成孤儿白占 meta 桶配额。

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
		h:     &Handlers{logger: testLogger()},
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
