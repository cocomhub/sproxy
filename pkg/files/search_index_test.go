// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_index_test.go 是「搜索/列表走增量文件索引」的领域级测试（roadmap P0）：
//
// 目标：搜索从「每次全量 WalkDir 扫描」演进为「启动/首次使用全量构建 + 写路径增量维护」的
// 文件索引（owner 维度），搜索/列表亚秒级；索引与磁盘一致（删除/重命名/目录删除即时同步，
// 首次使用全量构建兜底「索引丢失/损坏可重建」）。
//
// 行为契约（**与旧实现逐字一致**，只换数据来源）：
//   - 子串匹配不区分大小写，只匹配「文件名/目录名」（base name），不匹配完整路径；
//   - 命中目录时返回 IsDir=true 条目（name = 相对 user 桶路径），目录条目跨卷去重、不绑卷；
//   - 命中文件时 name = 完整相对路径（如 sub/also_keep_me.txt），带 size/mtime/checksum/volume；
//   - 在途临时文件（.inflight-*.part）不参与；q 为空 → 400。

import (
	"net/http"
	"testing"
)

// TestService_SearchFiles_IndexTracksUpload 钉住「索引增量维护：上传后可见」。
// 先触发一次搜索（空库 → 索引已构建），随后上传新文件（写路径 upsert），再次搜索必须命中。
// 变异验证：删掉 WriteFile 成功路径的索引 upsert → 本用例第二次搜索不命中 → 红。
func TestService_SearchFiles_IndexTracksUpload(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	// 空库先触发索引构建。
	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}

	body := []byte("hello index")
	env.upload(t, "alice", "keep_me.txt", body, sha256Hex(body), 0)

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=keep")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 1 || len(resp.Files) != 1 {
		t.Fatalf("上传后搜索应命中 1 个文件, got total=%d files=%+v", resp.Total, resp.Files)
	}
	if f := resp.Files[0]; f.Name != "keep_me.txt" || f.Size != int64(len(body)) || f.Checksum != sha256Hex(body) {
		t.Fatalf("条目=%+v want {Name:keep_me.txt Size:%d Checksum:%s}", f, len(body), sha256Hex(body))
	}
}

// TestService_SearchFiles_IndexTracksDelete 钉住「删除同步：删后不可见」。
// 变异验证：删掉 DeleteFile 成功路径的索引 remove → 删除后搜索仍命中 → 红。
func TestService_SearchFiles_IndexTracksDelete(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	body := []byte("delete me")
	cs := sha256Hex(body)
	env.upload(t, "alice", "doomed.txt", body, cs, 0)

	if _, err := env.svc.DeleteFile(t.Context(), DeleteFileInput{
		Owner: "alice", RemotePath: "doomed.txt", ExpectedChecksum: cs, AllowMissing: false,
	}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=doom")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d", rr.Code)
	}
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("删除后搜索不应命中, got %+v", resp.Files)
	}
}

// TestService_SearchFiles_IndexTracksRename 钉住「重命名同步：新名命中、旧名不命中」。
// 变异验证：删掉 rename 成功路径的索引 rename → 搜索旧名仍命中 → 红。
func TestService_SearchFiles_IndexTracksRename(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	body := []byte("rename me")
	cs := sha256Hex(body)
	env.upload(t, "alice", "old_name.txt", body, cs, 0)

	if _, err := env.svc.RenameFile(t.Context(), RenameFileInput{
		Owner: "alice", From: "old_name.txt", To: "new_name.txt", ExpectedChecksum: cs,
	}); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}

	// 新名命中。
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=new_name")
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "new_name.txt" {
		t.Fatalf("重命名后应命中新名, got %+v", resp.Files)
	}
	// 旧名不命中。
	rr = env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=old_name")
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("重命名后旧名不应命中, got %+v", resp.Files)
	}
}

// TestService_SearchFiles_IndexTracksRmdir 钉住「目录删除同步：子树不可见」。
// 变异验证：删掉 RemoveDir 成功路径的索引 removePrefix → 目录删除后子文件仍命中 → 红。
func TestService_SearchFiles_IndexTracksRmdir(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	body := []byte("nested")
	env.upload(t, "alice", "subdir/inner.txt", body, sha256Hex(body), 0)

	if _, err := env.svc.RemoveDir("alice", "subdir", true); err != nil {
		t.Fatalf("RemoveDir: %v", err)
	}

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=inner")
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("目录删除后子文件不应命中, got %+v", resp.Files)
	}
}

// TestService_SearchFiles_IndexRebuildOnFirstUse 钉住「索引丢失/损坏可重建」：
// 磁盘上已有文件但索引从未构建（新进程/索引被 Invalidate）→ 首次搜索全量构建后命中。
func TestService_SearchFiles_IndexRebuildOnFirstUse(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	// 直接落盘（不经写路径，模拟「索引未构建」的存量数据）。
	writeUserFile(t, env, "alice", "user/preexisting.txt", "pre")

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=preexisting")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d", rr.Code)
	}
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "preexisting.txt" {
		t.Fatalf("首次使用应全量构建并命中存量文件, got %+v", resp.Files)
	}
}

// TestService_SearchFiles_IndexInvalidateRebuilds 钉住「显式失效后重建」：
// InvalidateIndex(owner) 后（装配层版本恢复等旁路写会调用），搜索重建并命中旁路写入的文件。
func TestService_SearchFiles_IndexInvalidateRebuilds(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	// 旁路写：直接落盘（模拟装配层版本恢复等不经领域写路径的写入）。
	writeUserFile(t, env, "alice", "user/restored.txt", "r")

	// 索引失效 → 下次搜索重建。
	env.svc.InvalidateIndex("alice")
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=restored")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d", rr.Code)
	}
	if resp := decodeList(t, rr); resp.Total != 1 || resp.Files[0].Name != "restored.txt" {
		t.Fatalf("失效后应重建并命中旁路写入, got %+v", resp.Files)
	}
}

// TestService_SearchFiles_IndexPreservesSubstringSemantics 钉住「索引化后子串匹配语义不变」：
// 命中目录（q 匹配目录名）返回 IsDir 目录条目；命中文件返回完整相对路径；大小写不敏感。
func TestService_SearchFiles_IndexPreservesSubstringSemantics(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	env.upload(t, "alice", "VideoDir/clip_01.mp4", []byte("v"), sha256Hex([]byte("v")), 0)
	env.upload(t, "alice", "video_02.mp4", []byte("w"), sha256Hex([]byte("w")), 0)

	// q=video（目录名/文件名都命中，大小写不敏感）。
	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=video")
	resp := decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("应命中目录 VideoDir + 文件 video_02.mp4, got total=%d files=%+v", resp.Total, resp.Files)
	}
	dirFound := false
	fileFound := false
	for _, f := range resp.Files {
		if f.Name == "VideoDir" && f.IsDir {
			dirFound = true
		}
		if f.Name == "video_02.mp4" && !f.IsDir {
			fileFound = true
		}
	}
	if !dirFound || !fileFound {
		t.Fatalf("应同时命中目录条目与文件条目, got %+v", resp.Files)
	}
}

// TestService_SearchFiles_IndexHidesInflightTemp 钉住「索引化后在途临时文件仍不可见」。
func TestService_SearchFiles_IndexHidesInflightTemp(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	if rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=seed"); rr.Code != http.StatusOK {
		t.Fatalf("预热搜索应 200, got %d", rr.Code)
	}
	writeUserFile(t, env, "alice", "user/"+inflightName, "PARTIAL")

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=inflight")
	if resp := decodeList(t, rr); resp.Total != 0 {
		t.Fatalf("在途临时文件不应命中, got %+v", resp.Files)
	}
}
