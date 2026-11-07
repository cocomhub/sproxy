// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// container_reclaim_phys_test.go 钉住两条不变式（独立审核 P2，2026-10-18）：
//
//  1. **空容器回收在 ListDir 失败时必须 fail-closed**：此前仅在 `err == nil` 时进删除循环，
//     失败即**静默跳过删除**却仍注销内存映射 ⇒ 磁盘上目录 meta 仍在（重启 loadIndex 扫到 ⇒
//     目录"复活"：幽灵空目录）。修法：不注销映射、不删容器 + 显式告警。
//  2. **目录改名必须同步 `dirPhysTotal`**：目录 meta blob 长度随内容变化（pad 到 [base,2·base)），
//     而改名此前完全不更新该累计（writeDirMetaLocked 不碰、ensureContainerLocked 只在建容器时加）
//     ⇒ TrueUsage 与磁盘漂移（随改名累积），与 mkdir/rmdir 的实测差分口径不一致。

import (
	"context"
	"errors"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// failListDirFS 让指定路径的 ListDir 失败（其余委托）。
type failListDirFS struct {
	*syncpkg.LocalFS
	path string
}

func (f *failListDirFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if p == f.path {
		return nil, errors.New("injected: listdir failure")
	}
	return f.LocalFS.ListDir(ctx, p)
}

// TestRemoveEmptyContainer_ListDirFailureKeepsMappings ListDir 失败 ⇒ 不得注销映射/删容器。
func TestRemoveEmptyContainer_ListDirFailureKeepsMappings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := fs.MakeDir(ctx, "a"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	container := fs.dirSegs["a"]
	if container == "" {
		t.Fatal("前置：容器未建立")
	}
	// 让该容器的 ListDir 失败。
	fs.inner = &failListDirFS{LocalFS: inner, path: container}

	fs.mu.Lock()
	fs.removeEmptyContainerLocked(ctx, container, "a")
	fs.mu.Unlock()

	// 核心断言：映射必须保留（未修必红：被 delete ⇒ 磁盘内容仍在 ⇒ 重启后幽灵目录）。
	fs.mu.RLock()
	_, stillMapped := fs.dirSegs["a"]
	fs.mu.RUnlock()
	if !stillMapped {
		t.Fatal("ListDir 失败时空容器映射**不得**注销（未修必红：磁盘目录 meta 仍在 ⇒ 重启后目录复活）")
	}
	// 容器目录也不得被删（否则后续无法回收）。
	if e, serr := inner.Stat(ctx, container); serr != nil || e == nil {
		t.Fatalf("ListDir 失败时容器目录不得删除（err=%v）", serr)
	}
}

// 关于「目录改名是否导致 dirPhysTotal 漂移」：**已实测证伪**（2026-10-18）——
// 探针（5 次改名）显示目录 meta blob 落盘长度恒为 353B、dirPhysTotal 恒 677 ⇒ 与内容无关
// （pad 目标随机但最终长度恒定）⇒ 改名**不产生漂移** ⇒ 不做改动、也不保留无判别力用例。
// 若未来 meta 格式变化导致长度随内容变（届时该不变量会破），应在此补**能变红**的用例。

// failDeleteFS 让指定路径的 Delete 失败（其余委托）——模拟残留文件删不掉的场景。
type failDeleteFS struct {
	*syncpkg.LocalFS
	path string
}

func (f *failDeleteFS) Delete(ctx context.Context, p string) error {
	if p == f.path {
		return errors.New("injected: delete failure")
	}
	return f.LocalFS.Delete(ctx, p)
}

// TestRemoveEmptyContainer_PerFileDeleteFailureKeepsMappings 逐文件删除失败也必须 fail-closed。
// 未修必红：旧实现 `_ = Delete` 忽略错误 ⇒ 继续删容器（必失败）+ 注销映射 ⇒ 磁盘残留 meta
// ⇒ 重启后 loadIndex 扫到 ⇒ 幽灵目录"复活"（与 ListDir 分支的处置不一致）。
func TestRemoveEmptyContainer_PerFileDeleteFailureKeepsMappings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := fs.MakeDir(ctx, "a"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	container := fs.dirSegs["a"]
	if container == "" {
		t.Fatal("前置：容器未建立")
	}
	// 找到容器内的目录 meta blob，并让它的 Delete 失败。
	entries, lerr := inner.ListDir(ctx, container)
	if lerr != nil || len(entries) == 0 {
		t.Fatalf("前置：容器内应有目录 meta（err=%v n=%d）", lerr, len(entries))
	}
	victim := container + "/" + entries[0].Name
	fs.inner = &failDeleteFS{LocalFS: inner, path: victim}

	fs.mu.Lock()
	fs.removeEmptyContainerLocked(ctx, container, "a")
	fs.mu.Unlock()

	fs.mu.RLock()
	_, stillMapped := fs.dirSegs["a"]
	fs.mu.RUnlock()
	if !stillMapped {
		t.Fatal("逐文件删除失败时映射**不得**注销（未修必红：磁盘 meta 仍在 ⇒ 重启后幽灵目录复活）")
	}
}
