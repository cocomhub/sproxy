// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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
		Path: "a.txt", Ours: []string{"o"}, Theirs: []string{"t"}, Timestamp: time.Now().UnixNano(),
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

// TestSyncConflicts_ResolveRelinksSidecar 回归（M1，第 7 轮 MAJOR）：冲突解决写回必须
// 联动 sidecar——旧 sidecar 描述的是**带冲突标记**的内容，不重建会让后续下载/转存逐分块
// 校验恒失配（fail-closed 固化不可读，幂等重传不自愈）。
// 变异：去掉 writeConflictFile 里的 relinkConflictMeta 调用 → sidecar 仍描述 marker → 红。
func TestSyncConflicts_ResolveRelinksSidecar(t *testing.T) {
	t.Parallel()
	srv := emptyRemote(t)
	h, base := newSyncTestEnv(t, srv.URL, nil)
	idx, err := syncmgr.NewConflictIndex(t.TempDir())
	if err != nil {
		t.Fatalf("NewConflictIndex: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	h.SetConflictIndex(idx)

	// 前置：写「带冲突标记」的文件（此时 sidecar 描述 marker 内容）。
	marker := []byte("<<<<<<< ours/=======/theirs >>>>>>> marker")
	if werr := h.writeConflictFile("a.txt", marker); werr != nil {
		t.Fatalf("预置冲突文件: %v", werr)
	}
	if got := metaTotalSHA(t, h, "meta/a.txt.meta"); got != sha256hex(marker) {
		t.Fatalf("前置：sidecar 应描述 marker 内容, got %s", got)
	}
	idx.Record(sync.ConflictRecord{
		Path: "a.txt", Ours: []string{"o"}, Theirs: []string{"t"}, Timestamp: time.Now().UnixNano(),
	})
	items := idx.List("")
	if len(items) != 1 {
		t.Fatalf("预置失败: %d", len(items))
	}
	resolved := "resolved-content"
	code, body := doSyncJSON(t, "POST", base+"/api/sync/conflicts/"+items[0].ID+"/resolve",
		`{"choice":"manual","content":"`+resolved+`"}`)
	if code != http.StatusOK {
		t.Fatalf("manual resolve = %d, want 200: %s", code, body)
	}
	// 断言：主文件已是解析内容 + sidecar 已重建为其证据（否则下载路径按 marker 校验必失配）。
	userRoot, _, _ := h.syncTenantRoot("")
	if raw, rerr := os.ReadFile(filepath.Join(userRoot, "a.txt")); rerr != nil || string(raw) != resolved {
		t.Fatalf("主文件应为解析内容: raw=%q err=%v", raw, rerr)
	}
	if got := metaTotalSHA(t, h, "meta/a.txt.meta"); got != sha256hex([]byte(resolved)) {
		t.Fatalf("解决写回后 sidecar 必须描述新内容（M1）：got %s want %s",
			got, sha256hex([]byte(resolved)))
	}
}

// metaTotalSHA 读租户根下指定 sidecar 的 TotalSHA256（找不到/不可解析 → Fatal）。
func metaTotalSHA(t *testing.T, h *Handlers, mrel string) string {
	t.Helper()
	userRoot, _, ok := h.syncTenantRoot("")
	if !ok {
		t.Fatal("租户不可用")
	}
	tenantRoot := filepath.Dir(userRoot)
	raw, err := os.ReadFile(filepath.Join(tenantRoot, filepath.FromSlash(mrel)))
	if err != nil {
		t.Fatalf("读 sidecar %s: %v", mrel, err)
	}
	var fm struct {
		TotalSHA256 string `json:"total_sha256"`
	}
	if uerr := json.Unmarshal(raw, &fm); uerr != nil {
		t.Fatalf("解析 sidecar: %v", uerr)
	}
	return fm.TotalSHA256
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
