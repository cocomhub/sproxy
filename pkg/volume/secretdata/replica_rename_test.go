// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// replica_rename_test.go 钉住「Rename 必须在**全部 target** 上执行」（独立审核 P1，2026-10-18）。
//
// 缺陷：`replicaFS.Rename` 只改 `targets[0]`，而 `WriteFile`/`Delete` 都是**全 target** ⇒ mv 后
// 副本仍留在**源路径**；随后 `deleteFileBlobs`（经 `replicaFS.Delete` 全 target）会把副本的
// **源**副本删掉 ⇒ 副本**既无源也无目标** ⇒ 主 target 故障时回退读副本失败 = **静默数据丢失**。
//
// 用例锁定该场景：双 target 写一份文件 ⇒ 改名 ⇒ 断言**两个 target 都只有新路径**（未修必红：
// 副本仍只有旧路径），且新旧路径的分布一致（无分叉）。

import (
	"bytes"
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestReplicaFS_RenameAppliesToAllTargets 改名必须在主 target 与副本上同时生效（无分叉）。
func TestReplicaFS_RenameAppliesToAllTargets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	primary := syncpkg.NewLocalFS(t.TempDir(), nil)
	replica := syncpkg.NewLocalFS(t.TempDir(), nil)
	inner := newReplicaFS([]syncpkg.FS{primary, replica})

	fs, err := NewFS(inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if err := fs.MakeDir(ctx, "a"); err != nil {
		t.Fatalf("MakeDir(a): %v", err)
	}
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir(b): %v", err)
	}
	data := bytes.Repeat([]byte("replica-rename"), 128)
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/f.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 前置：写是**全 target** ⇒ 两个 target 都能 Stat 到该 meta（用 meta 文件是否存在判定，
	// 避免依赖上层索引）。
	e := fs.index["a/f.bin"]
	if e == nil {
		t.Fatal("源条目缺失")
	}
	metaRel := e.dirSeg + "/" + e.metaName
	for name, tgt := range map[string]syncpkg.FS{"primary": primary, "replica": replica} {
		if ent, serr := tgt.Stat(ctx, metaRel); serr != nil || ent == nil {
			t.Fatalf("前置失败：%s 未持有源 meta（err=%v）", name, serr)
		}
	}

	// 改名（同容器 ⇒ 只重写 meta；跨容器 ⇒ 搬/拷分块）——此处用跨容器以覆盖 blob 搬移路径。
	if err := fs.Rename(ctx, "a/f.bin", "b/f.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	moved := fs.index["b/f.bin"]
	if moved == nil {
		t.Fatal("改名后目标条目缺失")
	}
	newMetaRel := moved.dirSeg + "/" + moved.metaName
	for name, tgt := range map[string]syncpkg.FS{"primary": primary, "replica": replica} {
		if ent, serr := tgt.Stat(ctx, newMetaRel); serr != nil || ent == nil {
			t.Fatalf("%s 未持有**新路径**的 meta（未修必红：Rename 只改主 target ⇒ 副本留在源路径，"+
				"随后全 target 删除会把副本的源副本删掉 ⇒ 副本既无源也无目标 = 静默数据丢失）（err=%v）", name, serr)
		}
		if ent, serr := tgt.Stat(ctx, metaRel); serr == nil && ent != nil {
			t.Fatalf("%s 仍持有**旧路径**的 meta（分叉：改名未在该 target 生效）", name)
		}
	}
}
