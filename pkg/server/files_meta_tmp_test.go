// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

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

	metaTmpSweepLast.Store(0) // 解除节流（本用例断言清扫本体）
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

// TestSweepMetaTmp_Throttled 节流：一个窗口内只枚举一次目录（第 5 轮评审 ⑥——写路径每次
// 全目录 ReadDir 在扁平大目录下是 O(n²)）；窗口过后恢复清扫。孤儿是惰性残留，多存活一个
// 窗口属可接受降级。
func TestSweepMetaTmp_Throttled(t *testing.T) {
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if merr := root.MkdirAll("meta", 0o755); merr != nil {
		t.Fatalf("建目录: %v", merr)
	}
	mrel := "meta/x.meta"
	plant := func(rel, data string) {
		t.Helper()
		f, oerr := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if oerr != nil {
			t.Fatalf("plant %s: %v", rel, oerr)
		}
		_, _ = f.Write([]byte(data))
		_ = f.Close()
	}

	// 记录枚举次数（替换目录枚入口），并解除节流。
	var reads atomic.Int64
	oldRead := metaTmpReadDir
	metaTmpReadDir = func(r *storage.Root, d string) ([]os.DirEntry, error) {
		reads.Add(1)
		return oldRead(r, d)
	}
	oldThrottle := metaTmpSweepThrottle
	metaTmpSweepThrottle = time.Hour // 本用例内窗口不会自然过期
	t.Cleanup(func() { metaTmpReadDir = oldRead; metaTmpSweepThrottle = oldThrottle; metaTmpSweepLast.Store(0) })

	p := filesMetaPolicy{}
	metaTmpSweepLast.Store(0) // 首次：允许枚举
	plant(mrel+".tmp.1", "orphan-1")
	p.sweepMetaTmp(root, mrel)
	if got := reads.Load(); got != 1 {
		t.Fatalf("首次应枚举 1 次, got %d", got)
	}
	if _, serr := root.Stat(mrel + ".tmp.1"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("首次应清掉孤儿, err=%v", serr)
	}

	// 窗口内第二次：**不得**再枚举；孤儿保留（惰性降级）。
	plant(mrel+".tmp.2", "orphan-2")
	p.sweepMetaTmp(root, mrel)
	if got := reads.Load(); got != 1 {
		t.Fatalf("节流窗口内不应再枚举目录, reads=%d", got)
	}
	if _, serr := root.Stat(mrel + ".tmp.2"); serr != nil {
		t.Fatalf("窗口内孤儿应保留（惰性）, err=%v", serr)
	}

	// 窗口过期（模拟）：恢复清扫。
	metaTmpSweepLast.Store(0)
	p.sweepMetaTmp(root, mrel)
	if got := reads.Load(); got != 2 {
		t.Fatalf("窗口过后应再枚举, reads=%d", got)
	}
	if _, serr := root.Stat(mrel + ".tmp.2"); !errors.Is(serr, os.ErrNotExist) {
		t.Fatalf("窗口过后应清掉孤儿, err=%v", serr)
	}
}
