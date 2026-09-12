// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// read_list_search_test.go 补齐只读面两处此前覆盖不足的用户可见行为：
//   - 列表的 **subdir 参数**：进入子目录只列该项内容，以及对非法子目录（穿越 / 服务端内部
//     `.__` 前缀）与非法 owner 的 400 拒绝；
//   - 搜索的**入参校验**（q 为空 → 400）与**多卷**语义（按 owner 视图逐卷搜索，文件条目带
//     各自卷名）。
//
// 单卷 / 多卷聚合列表与单卷递归搜索已在 read_test.go 覆盖，本文件不重复。

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestService_ListFiles_SubdirListsOnlyThatDir 覆盖 subdir 进入子目录：只返回该目录条目，
// 且 checksum 按 "user/<subdir>/<name>" 键命中。
func TestService_ListFiles_SubdirListsOnlyThatDir(t *testing.T) {
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/a.txt", "A")
	writeUserFile(t, env, "alice", "user/sub/b.txt", "BB")
	cs := env.checksumStoreFor("alice")
	cs.Set("user/sub/b.txt", sha256Hex([]byte("BB")))

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?subdir=sub")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 1 || len(resp.Files) != 1 {
		t.Fatalf("子目录应只列 1 个文件, got total=%d files=%+v", resp.Total, resp.Files)
	}
	fi, ok := findEntry(resp.Files, "b.txt")
	if !ok || fi.IsDir || fi.Checksum != sha256Hex([]byte("BB")) {
		t.Fatalf("b.txt 条目=%+v want {IsDir:false Checksum:%s}", fi, sha256Hex([]byte("BB")))
	}

	// 根目录仍能看到 a.txt 与 sub 目录（列表根未被 subdir 改变）。
	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	resp = decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("根目录应有 2 个条目, got %d: %+v", resp.Total, resp.Files)
	}
}

// TestService_ListFiles_RejectsBadSubdirAndOwner 覆盖列表的三条 400：
// 穿越子目录（ValidateFilePath）、服务端内部前缀子目录（UserRel 段名拒绝）、非法 owner
// （租户不可用）。三条都必须在读取目录前拒绝。
func TestService_ListFiles_RejectsBadSubdirAndOwner(t *testing.T) {
	cases := []struct {
		name   string
		actor  string
		target string
	}{
		{"子目录穿越", "alice", "/api/files?subdir=../evil"},
		{"服务端内部前缀", "alice", "/api/files?subdir=.__internal"},
		{"owner 非法（租户不可用）", "..", "/api/files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDirsEnv(t)
			rr := env.serve(env.svc.ListFiles, tc.actor, "GET", tc.target)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("应 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if resp := decodeList(t, rr); len(resp.Files) != 0 {
				t.Fatalf("拒绝分支不应返回任何条目: %+v", resp.Files)
			}
		})
	}
}

// TestService_SearchFiles_RequiresQuery 覆盖搜索缺 q → 400（空查询不得退化为"列全部"）。
func TestService_SearchFiles_RequiresQuery(t *testing.T) {
	env := newDirsEnv(t)
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 q 应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeList(t, rr); len(resp.Files) != 0 {
		t.Fatalf("缺 q 不应返回条目: %+v", resp.Files)
	}
}

// TestService_SearchFiles_MultiVolume 覆盖多卷搜索：按 owner 视图逐卷递归，命中的文件
// 条目带各自卷名（默认卷在前）。
func TestService_SearchFiles_MultiVolume(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")

	writeUserFile(t, env, "alice", "user/m.txt", "M")
	disk2User := filepath.Join(env.volDirs["disk2"], "alice", "user")
	if err := os.MkdirAll(disk2User, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(disk2User, "d.txt"), []byte("D"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("应跨两卷命中 2 个文件, got total=%d files=%+v", resp.Total, resp.Files)
	}
	m, ok := findEntry(resp.Files, "m.txt")
	if !ok || m.Volume != "main" {
		t.Fatalf("m.txt 条目=%+v want Volume=main", m)
	}
	d, ok := findEntry(resp.Files, "d.txt")
	if !ok || d.Volume != "disk2" {
		t.Fatalf("d.txt 条目=%+v want Volume=disk2", d)
	}
}
