// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestCleanupEmptyDuplicateContainers_RemovesEmptyKeepsAuthority 钉住**空落选容器清理**：
//   - 空落选容器（只有目录 meta / 空子容器）⇒ 被删除；
//   - **权威容器绝不动**（其文件仍可读）；
//   - 留证 JSON 落盘（可追溯删了什么）。
func TestCleanupEmptyDuplicateContainers_RemovesEmptyKeepsAuthority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	evidence := t.TempDir()
	sfs := newNormalizeFS(t, root)
	payload := bytes.Repeat([]byte("cleanup-authority-"), 512)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/keep.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	sfs.mu.RLock()
	cA := sfs.dirSegs["a"]
	sfs.mu.RUnlock()
	// 制造重复：复制权威容器**的目录 meta**（不带文件）到同层 ⇒ 一个空落选容器
	dupEmpty := path.Join(path.Dir(cA), "EMPTYDUPCONT1")
	if cerr := copyContainerMetaOnly(t, root, cA, dupEmpty); cerr != nil {
		t.Fatalf("复制目录 meta: %v", cerr)
	}

	sfs2 := newNormalizeFS(t, root)
	groups := sfs2.dupGroupsSnapshot()
	if len(groups) == 0 {
		t.Fatal("应发现重复容器组")
	}
	// 确认落选里包含我们造的空容器
	var hasEmpty bool
	for _, g := range groups {
		for _, o := range g.others {
			if o == dupEmpty {
				hasEmpty = true
			}
		}
	}
	if !hasEmpty {
		t.Fatalf("夹具异常：空落选容器未被登记为 others，groups=%+v", groups)
	}

	removed, skipped, err := sfs2.CleanupEmptyDuplicateContainers(ctx, evidence, false)
	if err != nil {
		t.Fatalf("CleanupEmptyDuplicateContainers: %v", err)
	}
	if removed == 0 {
		t.Fatal("空落选容器应被清理")
	}
	_ = skipped
	// 空落选容器物理已删
	if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(dupEmpty))); !os.IsNotExist(serr) {
		t.Fatalf("空落选容器应已删除，stat err=%v", serr)
	}
	// **权威容器未动**，文件仍可读
	rc, rerr := sfs2.OpenRead(ctx, "a/keep.bin")
	if rerr != nil {
		t.Fatalf("权威容器文件必须仍可读: %v", rerr)
	}
	got := make([]byte, len(payload))
	n, _ := rc.Read(got)
	_ = rc.Close()
	if n == 0 || !bytes.Equal(got[:n], payload[:n]) {
		t.Fatal("权威容器内容发生变化")
	}
	// 留证 JSON 已落盘
	found := false
	_ = filepath.Walk(evidence, func(p string, info os.FileInfo, werr error) error {
		if werr == nil && info != nil && !info.IsDir() && strings.HasSuffix(p, ".json") {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("清理留证 JSON 必须落盘（可追溯删除内容）")
	}
}

// TestCleanupEmptyDuplicateContainers_SkipsWhenFilesPresent 钉住**安全约束**：
// 落选容器含**文件 meta** ⇒ 不得删除（跳过 + 计数），数据必须保留。
//
// 变异：去掉 containerTreeIsEmpty 校验 ⇒ 本用例红（数据被误删）。
func TestCleanupEmptyDuplicateContainers_SkipsWhenFilesPresent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	pA := bytes.Repeat([]byte("A-"), 512)
	pB := bytes.Repeat([]byte("B-"), 512)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/one.bin", bytes.NewReader(pA), int64(len(pA)), 0); werr != nil {
		t.Fatalf("WriteFile(a): %v", werr)
	}
	// 造一个**含文件**的重复容器：复制整个权威容器（含文件 meta）到同层
	sfs.mu.RLock()
	cA := sfs.dirSegs["a"]
	sfs.mu.RUnlock()
	dupWithFiles := path.Join(path.Dir(cA), "DUPWITHFILES1")
	if cerr := copyTreeLocal(t, filepath.Join(root, filepath.FromSlash(cA)), filepath.Join(root, filepath.FromSlash(dupWithFiles))); cerr != nil {
		t.Fatalf("复制容器: %v", cerr)
	}
	// 让权威容器多一个文件 ⇒ 两者条目不同 ⇒ 审计标争议（落选进入 others）
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/two.bin", bytes.NewReader(pB), int64(len(pB)), 0); werr != nil {
		t.Fatalf("WriteFile(b): %v", werr)
	}

	sfs2 := newNormalizeFS(t, root)
	// 逐个校验：含文件的落选容器必须被识别为「非空」
	ok, reason, cerr := sfs2.containerTreeIsEmpty(ctx, dupWithFiles)
	if cerr != nil {
		t.Fatalf("containerTreeIsEmpty: %v", cerr)
	}
	if ok {
		t.Fatal("含文件 meta 的容器不得被判为空")
	}
	if reason == "" {
		t.Fatal("应给出「含文件」原因")
	}

	removed, skipped, err := sfs2.CleanupEmptyDuplicateContainers(ctx, "", false)
	if err != nil {
		t.Fatalf("CleanupEmptyDuplicateContainers: %v", err)
	}
	_ = removed
	if skipped == 0 {
		t.Fatal("含文件的落选容器必须计入 skipped（不得删除）")
	}
	// **数据仍在**（物理目录未删）
	if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(dupWithFiles))); serr != nil {
		t.Fatalf("含文件的落选容器不得被删除: %v", serr)
	}
}

// TestDeleteContainerTree_BottomUp 锁定删除顺序：叶子先（避免非空目录删除失败）。
func TestDeleteContainerTree_BottomUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	if merr := sfs.MakeDir(ctx, "x/y/z"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	c := sfs.dirSegs["x"]
	sfs.mu.RUnlock()
	nodes, err := sfs.collectContainerTree(ctx, c)
	if err != nil {
		t.Fatalf("collectContainerTree: %v", err)
	}
	if len(nodes) < 3 {
		t.Fatalf("应至少 3 层容器，got %d", len(nodes))
	}
	// 逆序删除（实现内部如此）；这里验证「按逆序删可行」：深度必须递减
	depths := make([]int, 0, len(nodes))
	for _, n := range nodes {
		depths = append(depths, strings.Count(n.container, "/"))
	}
	sorted := append([]int{}, depths...)
	sort.Ints(sorted)
	if depths[0] != sorted[0] {
		t.Fatalf("collectContainerTree 应为前序（父先），got %v", depths)
	}
	if derr := sfs.deleteContainerTree(ctx, c); derr != nil {
		t.Fatalf("deleteContainerTree: %v", derr)
	}
	if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(c))); !os.IsNotExist(serr) {
		t.Fatalf("整棵树应已删除，stat err=%v", serr)
	}
}
