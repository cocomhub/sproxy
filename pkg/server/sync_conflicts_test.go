// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// TestSyncConflicts_ListGetResolve API 全链路：登记 → 列出 → 单条 → resolve ours → 文件写回 + 不再列出。
func TestSyncConflicts_ListGetResolve(t *testing.T) {
	t.Parallel()
	srv := emptyRemote(t)
	h, base := newSyncTestEnv(t, srv.URL, nil)

	// 装配冲突索引（测试直接注入）。
	idx, err := syncmgr.NewConflictIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewConflictIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	h.SetConflictIndex(idx)

	// 预置冲突（模拟 merge3 登记）。
	idx.Record(sync.ConflictRecord{
		Path: "user/a.txt", HunkCount: 1, BaseSHA: "b", OursSHA: "o", TheirsSHA: "t",
		Ours: []string{"ours-content"}, Theirs: []string{"theirs-content"}, Timestamp: time.Now().UnixNano(),
	})

	// 列出。
	code, body := doSyncJSON(t, "GET", base+"/api/sync/conflicts", "")
	if code != http.StatusOK {
		t.Fatalf("list = %d, want 200: %s", code, body)
	}
	var listResp struct {
		Conflicts []*syncmgr.ConflictItem `json:"conflicts"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("解析列表: %v, body=%s", err, body)
	}
	if len(listResp.Conflicts) != 1 {
		t.Fatalf("列表应 1 条, got %d", len(listResp.Conflicts))
	}
	id := listResp.Conflicts[0].ID

	// 单条。
	if c2, b2 := doSyncJSON(t, "GET", base+"/api/sync/conflicts/"+id, ""); c2 != http.StatusOK {
		t.Fatalf("get = %d, want 200: %s", c2, b2)
	}

	// resolve ours → 文件写回 + 不再列出。
	code, body = doSyncJSON(t, "POST", base+"/api/sync/conflicts/"+id+"/resolve", `{"choice":"ours"}`)
	if code != http.StatusOK {
		t.Fatalf("resolve = %d, want 200: %s", code, body)
	}
	code, body = doSyncJSON(t, "GET", base+"/api/sync/conflicts", "")
	if code != http.StatusOK {
		t.Fatalf("list-after = %d: %s", code, body)
	}
	var after struct {
		Conflicts []*syncmgr.ConflictItem `json:"conflicts"`
	}
	if err := json.Unmarshal(body, &after); err != nil {
		t.Fatalf("解析列表-after: %v", err)
	}
	if len(after.Conflicts) != 0 {
		t.Fatalf("resolve 后应 0 条, got %d", len(after.Conflicts))
	}
}

// TestSyncConflicts_ResolveManual 手动解决（带 content）。
func TestSyncConflicts_ResolveManual(t *testing.T) {
	t.Parallel()
	srv := emptyRemote(t)
	h, base := newSyncTestEnv(t, srv.URL, nil)
	idx, err := syncmgr.NewConflictIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewConflictIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	h.SetConflictIndex(idx)
	idx.Record(sync.ConflictRecord{
		Path: "user/a.txt", Ours: []string{"o"}, Theirs: []string{"t"}, Timestamp: time.Now().UnixNano(),
	})
	items := idx.List("")
	if len(items) != 1 {
		t.Fatalf("预置失败: %d", len(items))
	}
	id := items[0].ID
	code, body := doSyncJSON(t, "POST", base+"/api/sync/conflicts/"+id+"/resolve",
		`{"choice":"manual","content":"manual-content"}`)
	if code != http.StatusOK {
		t.Fatalf("manual resolve = %d, want 200: %s", code, body)
	}
}

// TestSyncConflicts_NotConfigured 未装配索引 → 400。
func TestSyncConflicts_NotConfigured(t *testing.T) {
	t.Parallel()
	srv := emptyRemote(t)
	h, base := newSyncTestEnv(t, srv.URL, nil)
	_ = h
	code, body := doSyncJSON(t, "GET", base+"/api/sync/conflicts", "")
	if code != http.StatusBadRequest {
		t.Fatalf("未装配应 400, got %d: %s", code, body)
	}
}
