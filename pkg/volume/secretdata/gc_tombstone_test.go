// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestGCTombstone_CleansDeletedMetaAndChunks（Imp-3 回归）：删除写墓碑 meta + 保留分块
// 交 GC 上收。GC 复用内存索引（gcMarkIndex）后，墓碑 meta 与孤儿分块仍须按「不在索引
// 引用集」判删——本测试断言删除+GC 后容器内不再残留删除前出现的文件 meta/分块（目录
// meta 恒保留），且已删文件不可读。
func TestGCTombstone_CleansDeletedMetaAndChunks(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := data(500)
	if err := fs.WriteFile(ctx, "del.bin", bytes.NewReader(c), int64(len(c)), 0); err != nil {
		t.Fatal(err)
	}
	entry := fs.index["del.bin"]
	names := map[string]struct{}{}
	before, _ := fs.inner.ListDir(ctx, entry.dirSeg)
	for _, f := range before {
		names[f.Name] = struct{}{}
	}
	if len(names) < 2 {
		t.Fatalf("删除前容器应含 meta+分块（至少 2 个非目录文件），got %d", len(names))
	}
	if err := fs.Delete(ctx, "del.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GC(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := fs.inner.ListDir(ctx, entry.dirSeg)
	for _, f := range after {
		if f.IsDir || shardseal.ClassifyName(f.Name) == shardseal.KindDirMeta {
			continue // 目录条目/目录 meta 恒保留（GC 不清目录结构）
		}
		if _, was := names[f.Name]; was {
			t.Errorf("GC 后残留删除前文件 %s（墓碑 meta 或分块应被清）", f.Name)
		}
	}
	if rc, err := fs.OpenRead(ctx, "del.bin"); err == nil {
		rc.Close()
		t.Error("已删文件应不可读")
	}
}
