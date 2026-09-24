// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_index_content_test.go 验证内容索引（roadmap P2 内容索引残余）：
//  1. WithContentIndex(true)：构建后搜索命中正文词元（文件名不匹配也返回）。
//  2. 默认关：正文词元不命中（零回归）。
//  3. 变异验证：contentTokens 匹配条件删 → 开状态下正文命中测试红。

import (
	"net/http"
	"testing"
)

// TestService_SearchFiles_ContentIndexHit 内容索引开启：正文词元命中。
func TestService_SearchFiles_ContentIndexHit(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	env.contentIndex = true
	env.rebuild()

	body := []byte("the quick brown fox jumps")
	env.upload(t, "alice", "doc.txt", body, sha256Hex(body), 0)

	// 预热触发全量构建（含正文抽样）。
	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	// 文件名不匹配、正文含 "brown" → 应命中 doc.txt。
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=brown")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索 = %d", rr.Code)
	}
	resp := decodeList(t, rr)
	if len(resp.Files) == 0 || resp.Files[0].Name != "doc.txt" {
		t.Fatalf("内容索引应命中 doc.txt, got %+v", resp.Files)
	}
}

// TestService_SearchFiles_ContentIndexOffByDefault 默认关：正文词元不命中（零回归）。
func TestService_SearchFiles_ContentIndexOffByDefault(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("unique-needle-in-content")
	env.upload(t, "alice", "doc.txt", body, sha256Hex(body), 0)

	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=needleincontent")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索 = %d", rr.Code)
	}
	resp := decodeList(t, rr)
	if len(resp.Files) != 0 {
		t.Fatalf("默认关：正文词元不应命中, got %+v", resp.Files)
	}
}
