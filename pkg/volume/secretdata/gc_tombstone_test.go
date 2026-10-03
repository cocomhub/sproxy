// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestGC_OptionalTool_CleansOrphanChunkPreservesLive（方案 A：GC 降级为**可选维护工具**，
// 默认禁用、正确性不依赖）。注入一个无任何 meta 引用的孤儿分块（模拟崩溃残留 / 远程卷 /
// 多进程共享卷孤儿兜底）→ 显式 fs.GC() 清理它；同容器存活文件分块不被误删。
func TestGC_OptionalTool_CleansOrphanChunkPreservesLive(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := data(500)
	if err := fs.WriteFile(ctx, "live.bin", bytes.NewReader(c), int64(len(c)), 0); err != nil {
		t.Fatal(err)
	}
	entry := fs.index["live.bin"]
	// 注入孤儿分块（hex 名 = KindChunk，无 meta 引用）到同容器。
	orphan := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" // 40 hex，无 -/_/@
	if werr := fs.inner.WriteFile(ctx, path.Join(entry.dirSeg, orphan), bytes.NewReader(data(64)), 64, 0); werr != nil {
		t.Fatal(werr)
	}
	if ent, _ := fs.inner.Stat(ctx, path.Join(entry.dirSeg, orphan)); ent == nil {
		t.Fatal("孤儿分块应已注入")
	}
	// 显式 fs.GC()（可选工具，非默认后台）：清孤儿、保存活。
	if _, gerr := fs.GC(ctx); gerr != nil {
		t.Fatalf("GC: %v", gerr)
	}
	if ent, _ := fs.inner.Stat(ctx, path.Join(entry.dirSeg, orphan)); ent != nil {
		t.Error("GC（可选工具）未清理孤儿分块")
	}
	// 存活文件仍可读回、内容一致（GC 不得误删存活分块）。
	rc, rerr := fs.OpenRead(ctx, "live.bin")
	if rerr != nil {
		t.Fatalf("GC 后 OpenRead(live.bin): %v", rerr)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, c) {
		t.Error("GC 误删存活文件分块，内容不一致")
	}
}
