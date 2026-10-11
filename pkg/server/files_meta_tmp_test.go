// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestFilesMetaPolicy_SweepMetaTmp（m9）：meta tmp 孤儿清扫覆盖两种命名（本文件的
// `.tmp.<nano>` 与 LocalFS.writeFileAtomic 的 `.tmp-*`），且**不**误删同目录其他文件。
// 变异：去掉 `.tmp-` 分支 → 连字符孤儿残留 → 红。
func TestFilesMetaPolicy_SweepMetaTmp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, rerr := storage.OpenRoot(dir)
	if rerr != nil {
		t.Fatalf("OpenRoot: %v", rerr)
	}
	// Root.Close 释放内部句柄（Windows：不关会让 t.TempDir 严格清理报 "being used by
	// another process" → 假红）。cleanup 为 LIFO，先于 TempDir 清理执行。
	t.Cleanup(func() { _ = root.Close() })
	const mrel = "sub/x.meta"
	plant := func(name, data string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 真实命名（由生成端决定）：`.tmp.<nano>`（无前导点）与 LocalFS 的 `.<base>.tmp-*`（**带前导点**）。
	// 首版 fixture 用了 `x.meta.tmp-abc`（无前导点）⇒ 与生成端不符 ⇒ 假绿（我的 m9 回归）。
	orphans := []string{mrel + ".tmp.123", path.Dir(mrel) + "/." + path.Base(mrel) + ".tmp-abc"}
	for _, o := range orphans {
		plant(o, "orphan")
	}
	keep := []string{mrel, "sub/other.meta", "sub/x.meta.keep", "sub/x.meta.tmp-abc.meta"}
	for _, k := range keep {
		plant(k, "keep")
	}

	(filesMetaPolicy{}).sweepMetaTmp(root, mrel)

	for _, o := range orphans {
		if _, err := root.Stat(o); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("孤儿 %s 应被清扫（err=%v）", o, err)
		}
	}
	for _, k := range keep {
		if _, err := root.Stat(k); err != nil {
			t.Errorf("非孤儿 %s 不应被删: %v", k, err)
		}
	}
}
