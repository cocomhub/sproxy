// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_all_volumes_test.go 钉住「全卷搜索」（设计 2026-10-30 D4 定案）：
// 本地卷走内存索引、外部卷经 `VolumeViewLister`（装配层视图 = 卷状态缓存）递归列举，
// 结果聚合；深度/条目预算受限；单卷失败隔离；tag 过滤只作用于本地卷。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// searchEnv 构造「本地默认卷 + 外部卷 v-ext」的搜索环境（viewList 提供外部卷列举）。
func searchEnv(t *testing.T) (*dirsEnv, *viewListSet, *fakeExternalVolume) {
	t.Helper()
	env := viewEnv(t, false) // 多卷 + vls
	ext := &fakeExternalVolume{tree: map[string][]ExternalEntry{}}
	env.enableExternalVolume(t, volume.Volume{Name: "v-ext", Type: "fakeext"}, ext)
	vls, ok := env.viewList.(*viewListSet)
	if !ok {
		t.Fatal("env.viewList 未设置为 *viewListSet")
	}
	return env, vls, ext
}

// TestSearch_AggregatesLocalAndExternalVolumes：本地索引结果 + 外部卷递归结果聚合，
// 外部文件带 Volume，目录条目跨卷只出现一次。
//
// 变异：去掉 Search 的外部卷分支 ⇒ 外部命中缺失 ⇒ 红。
func TestSearch_AggregatesLocalAndExternalVolumes(t *testing.T) {
	t.Parallel()
	env, _, ext := searchEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "keep-local.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ext.tree[""] = []ExternalEntry{
		{Name: "ext-a.txt", Size: 7, MTime: 11},
		{Name: "extdir", IsDir: true},
	}
	ext.tree["extdir"] = []ExternalEntry{{Name: "ext-b.txt", Size: 9}}

	// 本地：索引命中。
	res, err := env.svc.Search(t.Context(), SearchQuery{Owner: "alice", Query: "keep-local"})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("本地搜索: n=%d err=%v", len(res.Files), err)
	}
	if res.Files[0].Volume != "default" {
		t.Fatalf("本地命中 Volume=%q, want default", res.Files[0].Volume)
	}
	// 外部：顶层 + 子目录递归命中，带卷名。
	res, err = env.svc.Search(t.Context(), SearchQuery{Owner: "alice", Query: "ext-"})
	if err != nil {
		t.Fatalf("外部搜索: %v", err)
	}
	if len(res.Files) != 2 {
		t.Fatalf("外部卷递归命中 = %d, want 2: %+v", len(res.Files), res.Files)
	}
	names := map[string]string{}
	for _, f := range res.Files {
		names[f.Name] = f.Volume
	}
	if names["ext-a.txt"] != "v-ext" || names["extdir/ext-b.txt"] != "v-ext" {
		t.Fatalf("外部命中路径/卷名错误: %+v", names)
	}
	// 目录条目：IsDir、Volume 空。
	res, err = env.svc.Search(t.Context(), SearchQuery{Owner: "alice", Query: "extdir"})
	if err != nil || len(res.Files) != 1 {
		t.Fatalf("目录搜索: n=%d err=%v", len(res.Files), err)
	}
	if !res.Files[0].IsDir || res.Files[0].Name != "extdir" || res.Files[0].Volume != "" {
		t.Fatalf("目录条目应 {IsDir:true, Name:extdir, Volume:\"\"}, got %+v", res.Files[0])
	}
}

// TestSearch_ExternalFailureIsolated：外部卷列举失败不影响本地结果，搜索本身不报错。
func TestSearch_ExternalFailureIsolated(t *testing.T) {
	t.Parallel()
	env, _, ext := searchEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ext.listErr = errors.New("remote boom")
	res, err := env.svc.Search(t.Context(), SearchQuery{Owner: "alice", Query: "keep"})
	if err != nil {
		t.Fatalf("外部卷失败不应使搜索报错: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "keep.txt" {
		t.Fatalf("本地结果应保留, got %+v", res.Files)
	}
}

// TestSearch_ExternalDepthBounded：自嵌套目录不得无限递归（深度上限生效）。
//
// 变异：去掉 depth 上限 ⇒ 递归不终止（测试超时/栈溢出）。
func TestSearch_ExternalDepthBounded(t *testing.T) {
	t.Parallel()
	env, vls, ext := searchEnv(t)
	ext.tree[""] = []ExternalEntry{{Name: "loop", IsDir: true}}
	ext.tree["loop"] = []ExternalEntry{{Name: "loop", IsDir: true}}

	res, err := env.svc.Search(t.Context(), SearchQuery{Owner: "alice", Query: "loop"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res.Files) > searchExternalMaxDepth+2 {
		t.Fatalf("递归深度未受限：命中 %d 条（max depth %d）", len(res.Files), searchExternalMaxDepth)
	}
	if vls.calls > searchExternalMaxDepth+2 {
		t.Fatalf("ListDirView 调用 %d 次，超过深度上限 %d", vls.calls, searchExternalMaxDepth)
	}
}

// TestSearch_TagSkipsExternalVolumes：标签是本地卷元数据 ⇒ tag 过滤时不遍历外部卷。
func TestSearch_TagSkipsExternalVolumes(t *testing.T) {
	t.Parallel()
	env, vls, ext := searchEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "tagged.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	ext.tree[""] = []ExternalEntry{{Name: "tagged-ext.txt"}}
	before := vls.calls
	if _, err := env.svc.Search(t.Context(), SearchQuery{Owner: "alice", Tag: "no-such-tag"}); err != nil {
		t.Fatalf("search: %v", err)
	}
	if vls.calls != before {
		t.Fatalf("tag 过滤不应遍历外部卷（calls %d → %d）", before, vls.calls)
	}
}
