// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_equivalence_test.go 钉住「索引搜索结果 == 直接遍历磁盘」的语义等价
// （审计 2026-10-29 P3-5）：search 没有实时扫描兜底，索引若漏条目/残留条目，用户可见
// 结果与真实磁盘不符且不会自愈。本测试以磁盘遍历为判据独立核对。

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// diskSearchNames 直接遍历 user 桶，按与索引相同的语义收集命中项（子串匹配 base 名、
// 跳过在途临时文件、目录条目只记一次）。
func diskSearchNames(t *testing.T, userAbs, q string) []string {
	t.Helper()
	dirs := map[string]bool{}
	var files []string
	ql := strings.ToLower(q)
	err := filepath.WalkDir(userAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if p != userAbs && IsInflightTempName(name) {
			return nil
		}
		rel, rerr := filepath.Rel(userAbs, p)
		if rerr != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !strings.Contains(strings.ToLower(name), ql) {
			return nil
		}
		if d.IsDir() {
			dirs[rel] = true
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	out := files
	for d := range dirs {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// TestSearch_IndexMatchesDiskWalk：多层级 + 目录命中 + 在途临时文件场景下，索引结果与磁盘
// 遍历逐条一致。
//
// 变异：索引漏目录条目（ensureParentsLocked 删掉）⇒ 目录命中缺失 ⇒ 红。
func TestSearch_IndexMatchesDiskWalk(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	layout := []string{
		"a/keep1.txt",
		"a/b/keep2.txt",
		"a/b/c/other.bin",
		"keepdir/x.txt",
		".inflight-AABBCC-upload-1.part",
	}
	for _, rel := range layout {
		abs := filepath.Join(userAbs, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	env.rebuild()

	for _, q := range []string{"keep", "keepdir", "other"} {
		res, err := env.svc.Search(SearchQuery{Owner: "alice", Query: q})
		if err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
		var got []string
		for _, f := range res.Files {
			got = append(got, f.Name)
		}
		sort.Strings(got)
		want := diskSearchNames(t, userAbs, q)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("q=%q 索引结果与磁盘遍历不一致:\n got=%v\nwant=%v", q, got, want)
		}
	}
}
