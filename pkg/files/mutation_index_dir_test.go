// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// mutation_index_dir_test.go 钉住目录变异的投影语义（设计 2026-10-30 D1/D2）：
// `storage.Mutation` 的 `IsDir` + 目录 rename 的「删源前缀 + 重建目标子树」——
// 目录 rename 后索引既不能残留源子树（搜到已不存在文件），也不能丢失目标子树（搜不到新文件）。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestApplyRootMutation_DirRenameRebuildsTargetSubtree 端到端（真实 Root rename + 事件观察者）：
// `user/a/**` → `user/z/**` 后，源前缀条目全部消失，目标子树逐条可搜。
//
// 变异：applyDirRename 改回单条（remove + upsert）⇒ 目标子树缺失 ⇒「重命名后应命中」红。
func TestApplyRootMutation_DirRenameRebuildsTargetSubtree(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	for _, rel := range []string{"a/c.txt", "a/b/keep.txt"} {
		abs := filepath.Join(userAbs, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	env.rebuild()
	svc := env.svc
	// 预热：构建索引（含 a/、a/b/、a/c.txt、a/b/keep.txt）。
	if res, err := svc.Search(SearchQuery{Owner: "alice", Query: "keep"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("预热搜索: n=%d err=%v", len(res.Files), err)
	}

	tnt := env.tenantFor("alice")
	if err := tnt.Root().Rename("user/a", "user/z"); err != nil {
		t.Fatalf("rename dir: %v", err)
	}
	// 显式投递**生产口径**事件（卷根命名空间含 owner 前缀；dirsEnv 用 OpenRoot，前缀需显式给）。
	svc.ApplyRootMutation(storage.Mutation{
		Op: storage.MutationRename, Tag: "main", Root: tnt.Root(),
		Rel: "alice/user/a", Rel2: "alice/user/z", IsDir: true,
	})

	// 目标子树必须完整可见，且**路径已在新位置**（只查数量会被「源残留」骗过）。
	for q, want := range map[string]string{
		"keep":  "z/b/keep.txt",
		"c.txt": "z/c.txt",
	} {
		res, err := svc.Search(SearchQuery{Owner: "alice", Query: q})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		if len(res.Files) != 1 || res.Files[0].Name != want {
			t.Fatalf("重命名后 %q 应命中 %q，got %+v（目标子树未重建/源残留）", q, want, res.Files)
		}
	}
	// 源路径不得残留。
	res, err := svc.Search(SearchQuery{Owner: "alice", Query: "a"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, f := range res.Files {
		if f.Name == "a" || len(f.Name) > 2 && f.Name[:2] == "a/" {
			t.Fatalf("源路径残留: %q（目录 rename 未按前缀清理）", f.Name)
		}
	}
}

// TestApplyRootMutation_EmptyDirRemoveUsesPrefix：空目录 `Root.Remove`（事件 MutationRemove +
// IsDir）必须按前缀清理（此前只删单条 ⇒ 目录条目残留）。
func TestApplyRootMutation_EmptyDirRemoveUsesPrefix(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(filepath.Join(userAbs, "gone", "deep"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	env.rebuild()
	svc := env.svc
	if res, err := svc.Search(SearchQuery{Owner: "alice", Query: "deep"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("预热: n=%d err=%v", len(res.Files), err)
	}
	tnt := env.tenantFor("alice")
	if err := tnt.Root().RemoveAll("user/gone/deep"); err != nil {
		t.Fatalf("removeall: %v", err)
	}
	svc.ApplyRootMutation(storage.Mutation{
		Op: storage.MutationRemoveAll, Tag: "main", Root: tnt.Root(), Rel: "alice/user/gone/deep", IsDir: true,
	})
	if err := tnt.Root().Remove("user/gone"); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	svc.ApplyRootMutation(storage.Mutation{
		Op: storage.MutationRemove, Tag: "main", Root: tnt.Root(), Rel: "alice/user/gone", IsDir: true,
	})
	res, err := svc.Search(SearchQuery{Owner: "alice", Query: "gone"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Files) != 0 {
		t.Fatalf("删除后不应命中 gone，got %+v", res.Files)
	}
}
