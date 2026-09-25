// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// tags_test.go 是文件标签系统（roadmap 11.10-④ / docs/designs/2026-09-24-file-tags.md）
// 的领域级测试：
//
//  1. 打标 → search?tag= 精确命中；未打标文件不命中；tag 与 q 可组合；
//  2. 批量打标：多文件一次性打标；
//  3. 校验：非法标签 / 超量 / 文件不存在（整批拒绝，不部分成功）；
//  4. 快照往返：saveIndexSnapshot 带 tags，重建 Service 后 tag 搜索仍命中；
//  5. 索引失效重建：从 meta/tags store 重新合并标签；
//  6. HTTP 面：查询参数形态 + JSON body 批量形态；
//  7. COW 并发：写路径 setTags 与读路径 search 并发，-race 下不 panic。
//
// 变异验证：
//  ① search 忽略 tags 分支 → tag 搜索测试红；
//  ② setTags 原地改 entry（非 COW）→ 并发测试 -race 红；
//  ③ 打标成功路径删 setTags → 打标后 tag 搜索不命中 → 红。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// postJSON 以指定 actor 发起带 JSON body 的 POST 请求（tags 批量形态用）。
func postJSON(h http.HandlerFunc, actor, target string, body any) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest("POST", target, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	h(rr, req)
	return rr
}

// TestTags_TagAndSearchByTag 打标 → search?tag= 精确命中；未打标文件不命中；tag 与 q 可组合。
func TestTags_TagAndSearchByTag(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("tag me")
	cs := sha256Hex(body)
	env.upload(t, "alice", "a.txt", body, cs, 0)
	env.upload(t, "alice", "b.txt", body, cs, 0)

	// 预热触发索引构建。
	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}

	if err := env.svc.TagFiles("alice", []string{"a.txt"}, []string{"important", "work"}); err != nil {
		t.Fatalf("TagFiles: %v", err)
	}

	// tag 精确命中 a.txt（b.txt 未打标不命中）。
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=important")
	if rr.Code != http.StatusOK {
		t.Fatalf("tag 搜索应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 1 || len(resp.Files) != 1 || resp.Files[0].Name != "a.txt" {
		t.Fatalf("tag 搜索应精确命中 a.txt, got total=%d files=%+v", resp.Total, resp.Files)
	}

	// tag 与 q 可组合：q 匹配 b.txt、tag 匹配 a.txt → 无命中。
	rr = env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=b&tag=important")
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("tag+q 组合应无命中, got %+v", resp.Files)
	}
}

// TestTags_TagOnlySearchWithoutQ tag 非空、q 为空 → 允许（按标签过滤全部，不退化 400）。
func TestTags_TagOnlySearchWithoutQ(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("only")
	env.upload(t, "alice", "only.txt", body, sha256Hex(body), 0)
	env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz")

	if err := env.svc.TagFiles("alice", []string{"only.txt"}, []string{"keep"}); err != nil {
		t.Fatalf("TagFiles: %v", err)
	}
	// q 为空 + tag 非空 → 200（按标签过滤全部文件）。
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=keep")
	if rr.Code != http.StatusOK {
		t.Fatalf("tag-only 搜索应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "only.txt" {
		t.Fatalf("tag-only 搜索应命中 only.txt, got %+v", resp.Files)
	}
	// q 与 tag 都为空 → 400（零回归）。
	rr = env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空查询应 400, got %d", rr.Code)
	}
}

// TestTags_Batch 批量打标：多文件一次性打标，各自命中。
func TestTags_Batch(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("batch")
	cs := sha256Hex(body)
	env.upload(t, "alice", "a.txt", body, cs, 0)
	env.upload(t, "alice", "b.txt", body, cs, 0)
	env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz")

	if err := env.svc.TagFiles("alice", []string{"a.txt", "b.txt"}, []string{"shared", "shared", "a"}); err != nil {
		t.Fatalf("批量打标: %v", err)
	}
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=shared")
	resp := decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("批量打标后 tag=shared 应命中 2 个, got total=%d files=%+v", resp.Total, resp.Files)
	}
	// 去重排序：shared 只记一次。
	if err := env.svc.TagFiles("alice", []string{"a.txt"}, []string{"shared", "shared"}); err != nil {
		t.Fatalf("重复打标应幂等成功: %v", err)
	}
}

// TestTags_Validation 校验：非法标签 / 超量 / 文件不存在（整批拒绝，不部分成功）。
func TestTags_Validation(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("v")
	cs := sha256Hex(body)
	env.upload(t, "alice", "v.txt", body, cs, 0)
	env.upload(t, "alice", "w.txt", body, cs, 0)

	// 非法标签（含空格/标点）→ 400。
	if err := env.svc.TagFiles("alice", []string{"v.txt"}, []string{"bad tag!"}); err == nil {
		t.Fatal("非法标签应报错")
	} else if he := asHTTPError(err); he == nil || he.Status != http.StatusBadRequest {
		t.Fatalf("非法标签应 400, got %v", err)
	}
	// 超量（21 个）→ 400。
	many := make([]string, 21)
	for i := range many {
		many[i] = fmt.Sprintf("t%d", i)
	}
	if err := env.svc.TagFiles("alice", []string{"v.txt"}, many); err == nil {
		t.Fatal("超量标签应报错")
	} else if he := asHTTPError(err); he == nil || he.Status != http.StatusBadRequest {
		t.Fatalf("超量应 400, got %v", err)
	}
	// 文件不存在 → 404。
	if err := env.svc.TagFiles("alice", []string{"missing.txt"}, []string{"t"}); err == nil {
		t.Fatal("文件不存在应报错")
	} else if he := asHTTPError(err); he == nil || he.Status != http.StatusNotFound {
		t.Fatalf("文件不存在应 404, got %v", err)
	}
	// 整批拒绝：batch 中一个不存在 → 全部拒绝（合法文件不被打标）。
	if err := env.svc.TagFiles("alice", []string{"v.txt", "missing.txt"}, []string{"t"}); err == nil {
		t.Fatal("批量含不存在文件应报错")
	}
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=t")
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("整批拒绝后不应有任何命中, got %+v", resp.Files)
	}
}

// TestTags_IndexSnapshotRoundTripWithTags 快照含 tags 往返：打标 → 保存快照 →
// 重建 Service（同存储根）→ tag 搜索仍命中。
func TestTags_IndexSnapshotRoundTripWithTags(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("snap")
	env.upload(t, "alice", "s.txt", body, sha256Hex(body), 0)
	env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz") // 构建

	if err := env.svc.TagFiles("alice", []string{"s.txt"}, []string{"keep"}); err != nil {
		t.Fatalf("TagFiles: %v", err)
	}
	if n := env.svc.SaveIndexSnapshots(); n != 1 {
		t.Fatalf("保存快照数 = %d, want 1", n)
	}
	s2 := env.newService()
	if n, err := s2.Search(SearchQuery{Owner: "alice", Query: "s.txt", Tag: "keep"}); err != nil || len(n.Files) != 1 {
		t.Fatalf("重建后 tag 搜索命中数 = %d err=%v, want 1（快照应带 tags）", len(n.Files), err)
	}
}

// TestTags_RebuildMergesFromStore 索引失效重建：从 meta/tags store 重新合并标签。
func TestTags_RebuildMergesFromStore(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("rebuild")
	env.upload(t, "alice", "r.txt", body, sha256Hex(body), 0)
	env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz") // 构建

	if err := env.svc.TagFiles("alice", []string{"r.txt"}, []string{"keep"}); err != nil {
		t.Fatalf("TagFiles: %v", err)
	}
	// 失效索引 → 下次搜索全量重建（walkUserRoot + store 合并）。
	env.svc.InvalidateIndex("alice")
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=keep")
	if rr.Code != http.StatusOK {
		t.Fatalf("重建后 tag 搜索应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "r.txt" {
		t.Fatalf("重建后 tag 搜索应命中 r.txt, got %+v", resp.Files)
	}
}

// TestService_TagsHandler_QueryAndBody HTTP 面：查询参数形态 + JSON body 批量形态。
func TestService_TagsHandler_QueryAndBody(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("h")
	cs := sha256Hex(body)
	env.upload(t, "alice", "h.txt", body, cs, 0)
	env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz")

	// 查询参数形态：POST /api/tags?filename=h.txt&tags=one,two。
	rr := env.serve(env.svc.Tags, "alice", "POST", "/api/tags?filename=h.txt&tags=one,two")
	if rr.Code != http.StatusOK {
		t.Fatalf("查询参数打标应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); !resp.Success {
		t.Fatalf("打标响应应 success, got %+v", resp)
	}
	rr = env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=one")
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "h.txt" {
		t.Fatalf("查询参数打标后 tag=one 应命中 h.txt, got %+v", resp.Files)
	}

	// JSON body 批量形态。
	rr = postJSON(env.svc.Tags, "alice", "/api/tags", TagsRequest{Files: []string{"h.txt"}, Tags: []string{"batch", "tag"}})
	if rr.Code != http.StatusOK {
		t.Fatalf("body 批量打标应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=batch")
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "h.txt" {
		t.Fatalf("body 打标后 tag=batch 应命中 h.txt, got %+v", resp.Files)
	}

	// 缺参 → 400。
	if rr := env.serve(env.svc.Tags, "alice", "POST", "/api/tags"); rr.Code != http.StatusBadRequest {
		t.Fatalf("缺参应 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestTags_SetTagsConcurrentWithSearch_NoRace COW 并发：写路径 setTags 与读路径 search
// 并发，-race 下不 panic。变异②：setTags 原地改 entry（非 COW）→ 本用例红。
func TestTags_SetTagsConcurrentWithSearch_NoRace(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("x")
	env.upload(t, "alice", "f.txt", body, sha256Hex(body), 0)
	env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=zzz") // 构建

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 写 goroutine：反复打标（TagFiles = store 落盘 + setTags COW 替换指针）。
	wg.Go(func() {
		i := 0
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = env.svc.TagFiles("alice", []string{"f.txt"}, []string{"t1", "t2"})
			i++
		}
	})
	// 读 goroutine：反复 search（无锁遍历当前 map）。
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=t1")
		}
	})
	for range 200 {
		env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?tag=t1")
	}
	close(stop)
	wg.Wait()
}
