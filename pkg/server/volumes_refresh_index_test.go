// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// TestVolumesAPI_RefreshClearsSearchIndex 钉住 refresh 与搜索索引的联动（审计 2026-10-29）：
// 进程外改动（运维直写盘）后执行 `POST /api/volumes/refresh`，**列表与搜索都必须看见新文件**。
//
// 此前 refresh 只清卷状态缓存（list），不碰 searchIndex ⇒ 用户「强制刷新」后列表正确、
// 搜索仍返回陈旧结果，且没有任何入口能纠正（只能重启）。
//
// 变异：去掉 refresh 成功路径的 `h.invalidateSearchIndexes()` ⇒ b.txt 搜不到 ⇒ 红。
func TestVolumesAPI_RefreshClearsSearchIndex(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	userDir := filepath.Join(root, "alice", "user")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write a: %v", err)
	}

	baseURL, h, _ := newVolumesAPIServer(t, "alice", []VolumeConfig{{Name: "main", Root: root}}, nil)
	h.stateCache = volumestate.New(volumestate.Options{})
	t.Cleanup(func() { _ = h.stateCache.RefreshAll(context.Background()) })
	svc := h.fileService()
	if svc == nil {
		t.Fatal("files.Service 未装配（测试无效）")
	}
	// 先构建索引（a.txt）。
	if res, err := svc.Search(t.Context(), files.SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("构建索引后应命中 a.txt: n=%d err=%v", len(res.Files), err)
	}
	// 进程外新增（不经服务写路径 ⇒ 没有 Root 事件、索引不会自动增量）。
	if err := os.WriteFile(filepath.Join(userDir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatalf("write b: %v", err)
	}

	resp, err := testHTTPClient(t).Post(baseURL+"/api/volumes/refresh", "application/json", nil)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh status=%d", resp.StatusCode)
	}

	res, serr := svc.Search(t.Context(), files.SearchQuery{Owner: "alice", Query: "b.txt"})
	if serr != nil {
		t.Fatalf("search: %v", serr)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "b.txt" {
		t.Fatalf("refresh 后搜索应看到进程外新增文件，got %+v（搜索索引未随 refresh 失效）", res.Files)
	}
}
