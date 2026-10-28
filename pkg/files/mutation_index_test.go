// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// mutation_index_test.go 钉住 V3 的核心承诺：**本地卷派生状态（搜索索引）可由
// `storage.Root` 结构性变异自动维护**——写路径无需记得调用 upsert/remove。
//
// 场景选择：模拟「绕过写内核」的真实形态（转存/归档/跨卷搬移落盘 = MkdirAll + 临时文件 +
// AtomicRename），这正是 #874 的事故族。修复前这类写只更新磁盘、不更新索引 ⇒ list/search
// 看不到新文件；现在由 Root 变异事件驱动。
//
// 分工：`storage` 包测试锁定「事件正确产生 + 租户前缀归一」；本文件锁定「事件 → 索引」的
// 消费语义（路径翻译 / 各操作类型 / 非 user 桶忽略）。

import (
	"net/http"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// seedMutationIndex 预热索引（空库全量构建），返回默认卷根（Mutation.Root 用它 Stat）。
func seedMutationIndex(t *testing.T, env *dirsEnv) *storage.Root {
	t.Helper()
	env.enableVolumes(t, "default")
	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	root := env.volSet.DefaultRoot()
	if root == nil {
		t.Fatal("测试环境应有默认卷根")
	}
	return root
}

// writeBypass 模拟旁路写：mkdir + 临时文件 + AtomicRename 落位（不经 files.Service 写内核）。
// 返回后由调用方喂 Mutation 事件（等价装配层的 Root 观察者回调）。
func writeBypass(t *testing.T, env *dirsEnv, rel string, body []byte) {
	t.Helper()
	tnt := env.tenants["alice"]
	if tnt == nil {
		t.Fatal("缺少 alice 租户")
	}
	rt := tnt.Root()
	dir := "user/transfer"
	if err := rt.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tmp := dir + "/.inflight-tmp.part"
	f, err := rt.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile tmp: %v", err)
	}
	if _, werr := f.Write(body); werr != nil {
		t.Fatalf("write tmp: %v", werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("close tmp: %v", cerr)
	}
	if rerr := rt.AtomicRename(tmp, rel); rerr != nil {
		t.Fatalf("AtomicRename: %v", rerr)
	}
}

// emit 把变异事件喂给消费方（模拟 Root 观察者：rel 已在卷根命名空间）。
func emit(env *dirsEnv, root *storage.Root, op storage.MutationOp, rel, rel2 string) {
	env.svc.ApplyRootMutation(storage.Mutation{Op: op, Tag: "default", Root: root, Rel: rel, Rel2: rel2})
}

// TestService_ApplyRootMutation_IndexTracksBypassWrite 钉住「旁路结构性写自动进索引」。
// 变异验证：不调 ApplyRootMutation（等价未装观察者）⇒ 搜索不命中 ⇒ 红。
func TestService_ApplyRootMutation_IndexTracksBypassWrite(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	root := seedMutationIndex(t, env)

	writeBypass(t, env, "user/transfer/readme.md", []byte("hello bypass"))
	emit(env, root, storage.MutationRename, "alice/user/transfer/.inflight-tmp.part", "alice/user/transfer/readme.md")

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=readme")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d", rr.Code)
	}
	resp := decodeList(t, rr)
	if resp.Total != 1 || len(resp.Files) != 1 {
		t.Fatalf("旁路写后应命中 1 个文件（索引未自动维护）, got total=%d files=%+v", resp.Total, resp.Files)
	}
	if f := resp.Files[0]; f.Name != "transfer/readme.md" {
		t.Fatalf("条目名 = %q，期望 transfer/readme.md", f.Name)
	}
}

// TestService_ApplyRootMutation_RemoveDropsEntry 钉住「旁路删除自动出索引」。
func TestService_ApplyRootMutation_RemoveDropsEntry(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	root := seedMutationIndex(t, env)
	writeBypass(t, env, "user/transfer/readme.md", []byte("x"))
	emit(env, root, storage.MutationRename, "alice/user/transfer/.inflight-tmp.part", "alice/user/transfer/readme.md")

	tnt := env.tenants["alice"]
	if err := tnt.Root().RemoveAll("user/transfer"); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	emit(env, root, storage.MutationRemoveAll, "alice/user/transfer", "")
	if resp := decodeList(t, env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=readme")); resp.Total != 0 {
		t.Fatalf("目录删除后不应命中，got %+v", resp.Files)
	}
}

// TestService_ApplyRootMutation_RenameMovesEntry 钉住「旁路重命名搬迁索引条目」。
func TestService_ApplyRootMutation_RenameMovesEntry(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	root := seedMutationIndex(t, env)
	writeBypass(t, env, "user/transfer/old.txt", []byte("x"))
	emit(env, root, storage.MutationRename, "alice/user/transfer/.inflight-tmp.part", "alice/user/transfer/old.txt")

	tnt := env.tenants["alice"]
	if err := tnt.Root().Rename("user/transfer/old.txt", "user/transfer/new.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	emit(env, root, storage.MutationRename, "alice/user/transfer/old.txt", "alice/user/transfer/new.txt")

	if resp := decodeList(t, env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=old")); resp.Total != 0 {
		t.Fatalf("旧名不应命中，got %+v", resp.Files)
	}
	resp := decodeList(t, env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=new"))
	if resp.Total != 1 || resp.Files[0].Name != "transfer/new.txt" {
		t.Fatalf("新名应命中 transfer/new.txt，got %+v", resp.Files)
	}
}

// TestService_ApplyRootMutation_IgnoresNonUserBuckets 钉住「meta/version/chunk 等桶的
// 高频写不进入用户可见索引」（否则每次 sidecar 写都会污染搜索视图）。
func TestService_ApplyRootMutation_IgnoresNonUserBuckets(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	root := seedMutationIndex(t, env)

	tnt := env.tenants["alice"]
	if err := tnt.Root().MkdirAll("meta/state", 0o755); err != nil {
		t.Fatalf("MkdirAll meta: %v", err)
	}
	emit(env, root, storage.MutationMkdir, "alice/meta/state", "")
	if resp := decodeList(t, env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=state")); resp.Total != 0 {
		t.Fatalf("meta 桶不应出现在用户索引中，got %+v", resp.Files)
	}
}

// TestSplitUserBucketPath 锁定路径翻译的边界（非法 owner / 非 user 桶 / 结案子路径）。
func TestSplitUserBucketPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		owner     string
		rel       string
		wantValid bool
	}{
		{"alice/user/a.txt", "alice", "a.txt", true},
		{"alice/user/dir/b.txt", "alice", "dir/b.txt", true},
		{"alice\\user\\dir\\b.txt", "alice", "dir/b.txt", true},
		{"alice/meta/x.json", "", "", false},
		{"alice/user", "", "", false},
		{"alice/user/", "", "", false},
		{"../alice/user/a.txt", "", "", false},
		{"alice/version/v1", "", "", false},
	}
	for _, c := range cases {
		owner, rel, ok := splitUserBucketPath(c.in)
		if ok != c.wantValid || owner != c.owner || rel != c.rel {
			t.Errorf("splitUserBucketPath(%q) = (%q,%q,%v)，期望 (%q,%q,%v)", c.in, owner, rel, ok, c.owner, c.rel, c.wantValid)
		}
	}
}

// TestService_ApplyRootMutation_ContentWriteUpserts 钉住「内容写事件」（MutationWrite）同样
// 落到索引：这是 `pkg/rootfs.WriteFile`（卷的 sync.FS 写面）在写成功后显式声明的路径，
// 覆盖转存/归档/镜像/备份等不经写内核的写入。
func TestService_ApplyRootMutation_ContentWriteUpserts(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	root := seedMutationIndex(t, env)

	// 目录先建好（结构性事件），再直接写内容（不经 files.Service 写内核）。
	writeBypass(t, env, "user/transfer/content.bin", []byte("0123456789"))
	emit(env, root, storage.MutationWrite, "alice/user/transfer/content.bin", "")

	resp := decodeList(t, env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=content"))
	if resp.Total != 1 || len(resp.Files) != 1 {
		t.Fatalf("内容写事件后应命中 1 个文件，got total=%d files=%+v", resp.Total, resp.Files)
	}
	if got := resp.Files[0]; got.Name != "transfer/content.bin" || got.Size != 10 {
		t.Fatalf("条目 = %+v，期望 transfer/content.bin size=10", got)
	}
}
