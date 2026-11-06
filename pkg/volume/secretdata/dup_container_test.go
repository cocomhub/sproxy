// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestPickCanonicalContainer 锁定重复容器的**权威选择规则**（纯函数）：
// 目录 meta 的 mtime 新者优先；平局按 DirID 字典序（确定性、与扫描顺序无关）。
func TestPickCanonicalContainer(t *testing.T) {
	t.Parallel()
	scans := []dirScan{
		{container: "old", dm: &dirMeta{DirID: "AAA", MTime: "2026-11-05T19:10:09Z"}},
		{container: "new", dm: &dirMeta{DirID: "ZZZ", MTime: "2026-11-06T11:46:59Z"}},
	}
	if got := pickCanonicalContainer(scans, []int{0, 1}); got != 1 {
		t.Fatalf("mtime 新者应胜出，got idx=%d", got)
	}
	// 平局（子容器数相同、mtime 相同）：DirID 字典序小者胜；idx0=BBB, idx1=AAA ⇒ 选 idx1。
	tie := []dirScan{
		{container: "b", dm: &dirMeta{DirID: "BBB", MTime: "2026-11-06T00:00:00Z"}},
		{container: "a", dm: &dirMeta{DirID: "AAA", MTime: "2026-11-06T00:00:00Z"}},
	}
	if got := pickCanonicalContainer(tie, []int{1, 0}); got != 1 {
		t.Fatalf("平局应按 DirID 字典序（AAA 胜 ⇒ idx 1），got idx=%d", got)
	}
	// **子容器多者胜**（最强判据；实测「写子目录不更新父 meta」⇒ mtime 常相同）：
	// idx0 无子容器、idx1 有 1 个子容器（即便 idx0 的 mtime 更新、DirID 更小）⇒ 选 idx1。
	withSub := []dirScan{
		{container: "X", dm: &dirMeta{DirID: "AAA", MTime: "2026-11-06T09:00:00Z"}},
		{container: "Y", dm: &dirMeta{DirID: "ZZZ", MTime: "2026-11-05T09:00:00Z"}},
		{container: "Y/child", dm: &dirMeta{DirID: "CCC", MTime: "2026-11-05T09:00:00Z"}},
	}
	if got := pickCanonicalContainer(withSub, []int{0, 1}); got != 1 {
		t.Fatalf("子容器多者应胜出，got idx=%d", got)
	}
}

// TestDuplicateContainer_MountsWithCanonical 钉住**重复容器可挂载**（2026-11-06 线上缺陷）。
//
// 线上形态（jav 卷）：同一逻辑目录 `X` 有两个容器（旧 `qG881kZTeIr` / 新 `4rQQof5H592J`），
// `X` 的**子容器**物理挂在旧容器下，而 `dirSegs[X]` 被**静默覆盖**成新容器 ⇒
// `validateContainerPlacement` 报「子容器未嵌套在逻辑父目录容器内」⇒ **卷拒绝挂载**。
//
// 构造（自然发生时序）：
//
//	① MakeDir("a")      ⇒ 容器 C1（meta mtime=T1）
//	② 复制 C1 的 **meta 文件**（不带子容器）⇒ C1'（同 mtime/DirID，模拟重复容器）
//	③ MakeDir("a/b")    ⇒ C1 meta 重写（mtime=T2>T1）且新增子容器 C2（物理父 = C1）
//	④ 重新挂载 ⇒ 必须成功，且 dirSegs["a"] 必须是 **C1**（mtime 新者），否则 C2 校验失败。
func TestDuplicateContainer_MountsWithCanonical(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	newFS := func() (*SecretdataFS, error) {
		return NewFS(syncpkg.NewLocalFS(root, nil), Options{
			Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
			Block: shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		})
	}
	sfs, err := newFS()
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	// ①
	if merr := sfs.MakeDir(ctx, "a"); merr != nil {
		t.Fatalf("MakeDir(a): %v", merr)
	}
	sfs.mu.RLock()
	c1 := sfs.dirSegs["a"]
	sfs.mu.RUnlock()
	if c1 == "" {
		t.Fatal("夹具异常：未找到目录 a 的容器")
	}
	// ② 只复制 meta（不带子容器）⇒ 得到「同一逻辑目录的第二个容器」
	dup := filepath.ToSlash(filepath.Join(filepath.Dir(c1), "DUPcontainerX"))
	if cerr := copyContainerMetaOnly(t, root, c1, dup); cerr != nil {
		t.Fatalf("复制容器 meta 失败: %v", cerr)
	}
	// ③ 让 C1 的 meta 变新 + 新增子容器 C2（物理父 = C1）
	if merr := sfs.MakeDir(ctx, "a/b"); merr != nil {
		t.Fatalf("MakeDir(a/b): %v", merr)
	}

	// ④ 模拟重启：必须挂载成功（修复前 fail-closed：C2 未嵌套在 dirSegs[a] 内）
	sfs2, err := newFS()
	if err != nil {
		t.Fatalf("重复容器场景必须能挂载（修复前拒绝挂载），got: %v", err)
	}
	sfs2.mu.RLock()
	chosen := sfs2.dirSegs["a"]
	sfs2.mu.RUnlock()
	if chosen != c1 {
		t.Fatalf("应选 mtime 新的权威容器 %q，got %q（选错会导致其子容器校验失败）", c1, chosen)
	}
	// 被忽略的重复容器**数据保留**（不删）
	if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(dup))); serr != nil {
		t.Fatalf("被忽略的重复容器不得被删除（数据保留）: %v", serr)
	}
}

// copyContainerMetaOnly 把 srcContainer 的**目录 meta 文件**复制到 dstContainer
// （不复制子容器）——模拟「同一逻辑目录的重复容器」。
func copyContainerMetaOnly(t *testing.T, root, srcContainer, dstContainer string) error {
	t.Helper()
	srcDir := filepath.Join(root, filepath.FromSlash(srcContainer))
	ents, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	// **必须是目录 meta**（按分类筛选；容器里还有文件 meta / 数据 blob，取第一个非目录项会取错）
	var metaName string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if shardseal.ClassifyName(e.Name()) == shardseal.KindDirMeta {
			metaName = e.Name()
			break
		}
	}
	if metaName == "" {
		return os.ErrNotExist
	}
	dstDir := filepath.Join(root, filepath.FromSlash(dstContainer))
	if merr := os.MkdirAll(dstDir, 0o755); merr != nil {
		return merr
	}
	b, rerr := os.ReadFile(filepath.Join(srcDir, metaName))
	if rerr != nil {
		return rerr
	}
	return os.WriteFile(filepath.Join(dstDir, metaName), b, 0o600)
}
