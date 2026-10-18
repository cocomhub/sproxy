// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestLoadIndex_RootDirMetaMissing_FailsClosed 钉住「挂载期判据（精细化 C）」：
//
// 背景（2026-10-18 用户裁定）：卷**非空**（扫到了容器）却**没有任何容器解析为卷根**时，必须区分两种成因：
//   - **密钥/配置错误**（所有容器目录 meta 都解不开）⇒ **放行挂载**（读时失败）——与既有用例
//     TestWrongSecretFails 锁定的语义一致（错密钥与损坏在挂载期不可区分，放行便于运维诊断）；
//   - **损坏**（有容器目录 meta 能解开，却没有根）⇒ **fail-closed 拒绝挂载**——否则用户看到
//     **静默空卷**（`list` 空），可能误判「数据丢了」并重传 ✗。
//
// 本用例钉住第二种：删掉根容器的目录 meta（子容器完好）后，挂载必须**报错**（而不是静默空视图）。
func TestLoadIndex_RootDirMetaMissing_FailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	inner := syncpkg.NewLocalFS(dir, nil)
	fs := newFSSharedInner(t, inner)
	if err := fs.MakeDir(ctx, "sub"); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "sub/f.bin",
		bytes.NewReader(data(128)), 128, 0); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	// 找到**根容器**（卷根下唯一的容器目录）并删掉其中的目录 meta blob（模拟损坏/误删）。
	rootEntries, err := inner.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("列卷根失败: %v", err)
	}
	var rootContainer string
	for _, e := range rootEntries {
		if e.IsDir {
			rootContainer = e.Name
			break
		}
	}
	if rootContainer == "" {
		t.Fatal("未找到根容器（布局异常）")
	}
	ents, err := inner.ListDir(ctx, rootContainer)
	if err != nil {
		t.Fatalf("列根容器失败: %v", err)
	}
	removed := 0
	for _, e := range ents {
		if !e.IsDir && shardseal.ClassifyName(e.Name) == shardseal.KindDirMeta {
			if derr := inner.Delete(ctx, rootContainer+"/"+e.Name); derr != nil {
				t.Fatalf("删除根目录 meta 失败: %v", derr)
			}
			removed++
		}
	}
	if removed != 1 {
		t.Fatalf("根容器内应有恰好 1 个目录 meta blob，实际删除 %d 个", removed)
	}

	// 重新挂载：**必须失败**（拒绝静默空视图）。修复前这里会成功挂载并返回空索引 ✗。
	_, merr := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   t.TempDir(),
	})
	if merr == nil {
		t.Fatal("根目录 meta 缺失时挂载**不应成功**（会静默空视图）✗")
	}
	if !strings.Contains(merr.Error(), "无任何容器解析为卷根") &&
		!strings.Contains(merr.Error(), "根") {
		t.Fatalf("错误信息应指明「根容器/根目录 meta」问题，实际: %v", merr)
	}
}
