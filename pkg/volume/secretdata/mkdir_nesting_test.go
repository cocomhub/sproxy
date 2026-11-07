// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestMakeDir_PhysicalNesting：**物理嵌套**（2026-10-14 用户口径）——底层目录树必须与逻辑目录树
// 同形：子容器落在父容器**内**（此前所有容器都是卷根下的随机名兄弟目录 ⇒ 存储上看不出父子关系）。
func TestMakeDir_PhysicalNesting(t *testing.T) {
	t.Parallel()
	rootDir := t.TempDir()
	opts := Options{Secret: []byte("nest-key-0123456789ab"), MetaPadBytes: 64 << 10, TempDir: t.TempDir()}
	fs, err := NewFS(syncpkg.NewLocalFS(rootDir, nil), opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "a/b/c"); err != nil {
		t.Fatalf("MakeDir a/b/c: %v", err)
	}

	// ① 顶层只有**一个**容器目录（逻辑树根的容器）——不再是 a/b/c 各一个扁平兄弟目录。
	top, lerr := fs.inner.ListDir(ctx, "")
	if lerr != nil {
		t.Fatalf("ListDir(卷根): %v", lerr)
	}
	var topDirs []string
	for _, e := range top {
		if e.IsDir {
			topDirs = append(topDirs, e.Name)
		}
	}
	if len(topDirs) != 1 {
		t.Fatalf("顶层容器数 = %d（%v），应为 1（扁平布局下会是多个兄弟目录）", len(topDirs), topDirs)
	}

	// ② 物理嵌套深度 ≥ 3（a、b、c 各占一层，且都在父容器内）。
	if d := containerNestingDepth(ctx, t, fs.inner, topDirs[0]); d < 3 {
		t.Fatalf("物理嵌套深度 = %d，应 ≥ 3（存储上必须能看出父子层级）", d)
	}

	// ③ 逻辑仍逐级可见（层级语义不变）。
	for _, p := range []string{"a", "a/b", "a/b/c"} {
		e, serr := fs.Stat(ctx, p)
		if serr != nil || e == nil || !e.IsDir {
			t.Fatalf("Stat(%s) 应为目录: e=%+v err=%v", p, e, serr)
		}
	}

	// ④ 嵌套目录内写读一致（容器链完整）。
	payload := bytes.Repeat([]byte("n"), 4096)
	if werr := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/b/c/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("嵌套写: %v", werr)
	}
	rc, oerr := fs.OpenRead(ctx, "a/b/c/f.bin")
	if oerr != nil {
		t.Fatalf("嵌套读: %v", oerr)
	}
	got := new(bytes.Buffer)
	if _, cerr := got.ReadFrom(rc); cerr != nil {
		t.Fatalf("读回: %v", cerr)
	}
	_ = rc.Close()
	if !bytes.Equal(got.Bytes(), payload) {
		t.Fatalf("嵌套读回不一致: len=%d want=%d", got.Len(), len(payload))
	}

	// ⑤ 重挂载（递归加载容器树）后逻辑树与文件都在。
	fs2, rerr := NewFS(syncpkg.NewLocalFS(rootDir, nil), opts)
	if rerr != nil {
		t.Fatalf("重挂载: %v", rerr)
	}
	for _, p := range []string{"a", "a/b", "a/b/c", "a/b/c/f.bin"} {
		if e, serr := fs2.Stat(ctx, p); serr != nil || e == nil {
			t.Fatalf("重挂载后 Stat(%s) 失败: e=%+v err=%v（容器树未递归加载？）", p, e, serr)
		}
	}
}

// containerNestingDepth 返回容器树的物理嵌套深度（仅统计「含子目录」的层级链）。
func containerNestingDepth(ctx context.Context, t *testing.T, inner syncpkg.FS, container string) int {
	t.Helper()
	depth := 0
	cur := container
	for {
		entries, err := inner.ListDir(ctx, cur)
		if err != nil {
			t.Fatalf("ListDir(%s): %v", cur, err)
		}
		next := ""
		for _, e := range entries {
			if e.IsDir {
				next = e.Name
				break
			}
		}
		if next == "" {
			return depth
		}
		depth++
		cur = cur + "/" + next
	}
}
