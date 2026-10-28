// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// collectMutations 收集 Root 变异事件（并发安全：写路径可能在多个 goroutine）。
type collectMutations struct {
	mu   sync.Mutex
	seen []Mutation
}

func (c *collectMutations) fn(m Mutation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seen = append(c.seen, m)
}

func (c *collectMutations) list() []Mutation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Mutation(nil), c.seen...)
}

// TestRootMutationObserver_StructuralOps 锁定「结构性变异方法各发一次事件、Rel 正确」。
// 这是 V3（本地卷派生状态自动维护）与 R45 门禁的基础契约。
func TestRootMutationObserver_StructuralOps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	col := &collectMutations{}
	rt.SetMutationObserver("vol1", col.fn)

	if err := rt.MkdirAll("u/owner/user/docs", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	f, cerr := rt.OpenFile("u/owner/user/docs/a.txt", os.O_CREATE|os.O_WRONLY, 0o600)
	if cerr != nil {
		t.Fatalf("OpenFile: %v", cerr)
	}
	if _, werr := f.WriteString("x"); werr != nil {
		t.Fatalf("write: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("close: %v", cerr)
	}
	if err := rt.Rename("u/owner/user/docs/a.txt", "u/owner/user/docs/b.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	now := time.Now()
	if err := rt.Chtimes("u/owner/user/docs/b.txt", now, now); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := rt.Link("u/owner/user/docs/b.txt", "u/owner/user/docs/c.txt"); err != nil {
		t.Skipf("硬链接不可用（文件系统不支持）: %v", err)
	}
	if err := rt.Remove("u/owner/user/docs/c.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := rt.RemoveAll("u/owner/user/docs"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}

	got := col.list()
	want := []struct {
		op        MutationOp
		rel, rel2 string
	}{
		{MutationMkdir, "u/owner/user/docs", ""},
		{MutationRename, "u/owner/user/docs/a.txt", "u/owner/user/docs/b.txt"},
		{MutationChtimes, "u/owner/user/docs/b.txt", ""},
		{MutationLink, "u/owner/user/docs/b.txt", "u/owner/user/docs/c.txt"},
		{MutationRemove, "u/owner/user/docs/c.txt", ""},
		{MutationRemoveAll, "u/owner/user/docs", ""},
	}
	if len(got) != len(want) {
		t.Fatalf("事件数 %d != 期望 %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].Op != w.op || got[i].Rel != w.rel || got[i].Rel2 != w.rel2 {
			t.Errorf("事件[%d] = {%s %q %q}，期望 {%s %q %q}", i, got[i].Op, got[i].Rel, got[i].Rel2, w.op, w.rel, w.rel2)
		}
		if got[i].Tag != "vol1" {
			t.Errorf("事件[%d] Tag = %q，期望 vol1", i, got[i].Tag)
		}
		if got[i].Root != rt {
			t.Errorf("事件[%d] Root 应为发事件的根", i)
		}
	}
}

// TestRootMutationObserver_FailedOpDoesNotNotify 锁定「写失败不发事件」（否则消费方会
// 为不存在的对象建条目/删不掉幽灵）。
func TestRootMutationObserver_FailedOpDoesNotNotify(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	col := &collectMutations{}
	rt.SetMutationObserver("vol1", col.fn)

	if rerr := rt.Remove("missing.txt"); rerr == nil {
		t.Fatal("删除不存在的文件应失败")
	}
	if rerr := rt.Rename("missing.txt", "x.txt"); rerr == nil {
		t.Fatal("重命名不存在的文件应失败")
	}
	if got := col.list(); len(got) != 0 {
		t.Fatalf("失败操作不应发事件: %+v", got)
	}
}

// TestRootMutationObserver_NilObserverNoop 锁定「未装观察者 = 零回归」。
func TestRootMutationObserver_NilObserverNoop(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if err := rt.MkdirAll("a/b", 0o755); err != nil {
		t.Fatalf("MkdirAll（无观察者）: %v", err)
	}
	if err := rt.RemoveAll("a"); err != nil {
		t.Fatalf("RemoveAll（无观察者）: %v", err)
	}
}

// TestRootMutationObserver_TenantInheritsPrefix 锁定租户子根**继承观察者并归一前缀**：
// 消费方只认「卷根命名空间」的 rel（`<owner>/user/<rel>`），不需要知道是哪个子根写的。
func TestRootMutationObserver_TenantInheritsPrefix(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	parent, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = parent.Close() })
	col := &collectMutations{}
	parent.SetMutationObserver("vol1", col.fn)

	tenant, err := OpenTenant(parent, "owner1")
	if err != nil {
		t.Fatalf("OpenTenant: %v", err)
	}
	t.Cleanup(func() { _ = tenant.Root().Close() })

	if err := tenant.Root().MkdirAll("user/docs", 0o755); err != nil {
		t.Fatalf("tenant MkdirAll: %v", err)
	}
	got := col.list()
	if len(got) != 1 {
		t.Fatalf("期望 1 个事件，got %+v", got)
	}
	if got[0].Rel != filepath.ToSlash("owner1/user/docs") {
		t.Errorf("Rel = %q，期望 owner1/user/docs（前缀归一）", got[0].Rel)
	}
	if got[0].Root != parent {
		t.Errorf("Root 应为卷根（消费方用它在卷根命名空间 Stat）")
	}
	if got[0].Tag != "vol1" {
		t.Errorf("Tag = %q，期望 vol1（租户继承）", got[0].Tag)
	}
}

// TestRootMutationObserver_AtomicRename 锁定临时文件提交（AtomicRename）也发 rename 事件
// ——它是本地卷最常见的「新对象出现」路径（写临时文件后改名落位）。
func TestRootMutationObserver_AtomicRename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	col := &collectMutations{}
	rt.SetMutationObserver("vol1", col.fn)

	if err := rt.MkdirAll(".tmp", 0o755); err != nil {
		t.Fatalf("MkdirAll tmp: %v", err)
	}
	tf, terr := rt.OpenFile(".tmp/a", os.O_CREATE|os.O_WRONLY, 0o600)
	if terr != nil {
		t.Fatalf("OpenFile tmp: %v", terr)
	}
	if cerr := tf.Close(); cerr != nil {
		t.Fatalf("close tmp: %v", cerr)
	}
	if err := rt.MkdirAll("user", 0o755); err != nil {
		t.Fatalf("MkdirAll user: %v", err)
	}
	if err := rt.AtomicRename(".tmp/a", "user/a"); err != nil {
		t.Fatalf("AtomicRename: %v", err)
	}
	got := col.list()
	if len(got) != 3 {
		t.Fatalf("期望 3 个事件（mkdir tmp + mkdir user + rename；内容写入不发结构事件），got %+v", got)
	}
	last := got[len(got)-1]
	if last.Op != MutationRename || last.Rel != ".tmp/a" || last.Rel2 != "user/a" {
		t.Fatalf("AtomicRename 事件 = %+v", last)
	}
	if _, serr := os.Stat(filepath.Join(dir, "user", "a")); serr != nil {
		t.Fatalf("落位文件应存在: %v", serr)
	}
}

// TestRootMutationObserver_MarkWritten 锁定「内容写显式声明」入口（V3 内容写事件）：
// 结构性方法自动发事件，内容写由写面适配器调用 MarkWritten。
func TestRootMutationObserver_MarkWritten(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	col := &collectMutations{}
	rt.SetMutationObserver("vol1", col.fn)

	if err := rt.MkdirAll("owner/user/d", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	rt.MarkWritten("owner/user/d/a.txt")

	got := col.list()
	if len(got) != 2 {
		t.Fatalf("期望 2 个事件（mkdir + write），got %+v", got)
	}
	if last := got[1]; last.Op != MutationWrite || last.Rel != "owner/user/d/a.txt" || last.Tag != "vol1" {
		t.Fatalf("write 事件 = %+v", last)
	}
}

// TestRootMutationObserver_MarkWrittenWithoutObserver 零回归：未装观察者时安全无操作。
func TestRootMutationObserver_MarkWrittenWithoutObserver(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	rt.MarkWritten("owner/user/a.txt") // 不 panic 即为通过
}
