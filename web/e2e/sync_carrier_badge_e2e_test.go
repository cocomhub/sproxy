// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

// sync_carrier_badge_e2e_test.go —— 传输面板里**同步任务载体徽标**的真浏览器 E2E。
//
// 为什么补这条（实测教训）：此前的覆盖是「JS 单测用**合成对象**断言渲染函数」+「Go e2e 只查 mesh 状态卡」，
// 而**服务端列表端点漏了 `kind/transport/carriers` 字段**（`Manager.List` 的投影没跟上），于是
// 「徽标在真实数据下永远显示不出来」这件事两道测试都没抓到——是真浏览器 + 真数据发现的。
// 本用例因此走**真实链路**：真 sproxy（httptest 起的完整服务）→ 真 `POST /api/sync/tasks` 建任务
// → 浏览器打开 UI → 等待同步任务行出现 → 断言徽标文本由**服务端返回的字段**渲染出来。

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/mxschmitt/playwright-go"
)

// seedSyncTask 用真实 `POST /api/sync/tasks` 建一个同步任务（返回任务 id）。
func seedSyncTask(t *testing.T, baseURL, remote, direction string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"direction": direction, "remote": remote, "src": ".", "dst": ".",
		"conflict_policy": "overwrite",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(baseURL+"/api/sync/tasks", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/sync/tasks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		t.Fatalf("创建同步任务 status=%d（want 201/200）", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("解析创建响应: %v", err)
	}
	if out.ID == "" {
		t.Fatal("创建响应缺少任务 id")
	}
	return out.ID
}

// TestSyncCarrierBadge_RendersFromServerData 钉住：列表端点返回的载体字段能被 UI 渲染成徽标。
//
// 走 `direct` 载体（无需 mesh 依赖即可建任务）；徽标文案 = `syncCarrierText` 的「声明」部分（`direct`）。
// 关键断言是**字段来自服务端**：若 `GET /api/sync/tasks` 不含 `kind`，徽标为空 ⇒ 用例失败。
func TestSyncCarrierBadge_RendersFromServerData(t *testing.T) {
	baseURL, h, cfg, cleanup := testServerCfgWithHandlers(t, func(cfg *server.Config) {
		cfg.SyncRemotes = []server.SyncRemoteConfig{{
			Name: "r-direct", Kind: "direct",
			URL: "http://127.0.0.1:1", AccessKey: "k", AccessKeySecret: "s",
		}}
	})
	defer cleanup()

	// 装配 sync manager（与 cmd/sproxy 同款**导出接缝**）：不装配则 handler 返回 400 "sync not configured"，
	// 用例就退化成了「UI 渲染合成数据」——那正是本 bug 逃逸的原因，故必须走真实链路。
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	exec := syncexec.NewExecutor(h.SyncTenantResolver(), logger)
	exec.SetTenantScopeResolver(h.SyncQuotaScope())
	exec.SetScopeResolver(h.SyncScopeFor())
	remotes := make([]syncmgr.RemoteConfig, 0, len(cfg.SyncRemotes))
	for _, r := range cfg.SyncRemotes {
		remotes = append(remotes, syncmgr.RemoteConfig{
			Name: r.Name, Kind: syncmgr.RemoteKind(r.Kind), URL: r.URL,
			AccessKey: r.AccessKey, AccessKeySecret: r.AccessKeySecret, AccessKeyID: r.AccessKeyID,
			Node: r.Node, Volume: r.Volume, PeerPins: r.PeerPins, Transport: r.Transport,
		})
	}
	sm := syncmgr.NewManager(h.SyncTenantResolver(), h.SyncTenantList(), nil,
		int(capacity.CategoryUserFiles), remotes, exec, logger,
		&syncmgr.Config{MaxConcurrent: 1, TaskTTL: time.Hour})
	sm.SetQuotaResolver(h.SyncQuotaStore())
	h.SetSyncMgr(sm)
	defer sm.Stop()

	taskID := seedSyncTask(t, baseURL, "r-direct", "push")

	// 先确认服务端列表**确实**带出 carrierKind（端点契约；缺了下面的 UI 断言必然失败）。
	listResp, err := http.Get(baseURL + "/api/sync/tasks")
	if err != nil {
		t.Fatalf("GET /api/sync/tasks: %v", err)
	}
	defer listResp.Body.Close()
	var list struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("解析列表: %v", err)
	}
	found := false
	for _, task := range list.Tasks {
		if task["id"] == taskID {
			found = true
			if task["kind"] == nil || task["kind"] == "" {
				t.Fatalf("列表端点未返回 kind（Web UI 载体徽标的数据源）：%+v", task)
			}
		}
	}
	if !found {
		t.Fatalf("列表端点未返回刚创建的任务 %s", taskID)
	}

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	// 必须先切到「传输」主 tab：`#transfer-page` 初始 `display:none`，同步任务行由
	// showTransferPage → refreshSyncTasks 拉取（不切页则永远空白，断言会误判为「未接线」）。
	if err := page.Locator("#main-tab-transfer").Click(); err != nil {
		t.Fatalf("点击传输 tab: %v", err)
	}
	if err := waitLoc(page, "#transfer-page", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("传输页未显示: %v", err)
	}
	// 徽标文案 `direct` **只能**由 syncCarrierText 渲染（页面上没有别处产出这个字样）
	// ⇒ 出现即证明「服务端 kind 字段 → 列表 JSON → 归一化 → 行内徽标」整条链路通。
	// 这也正是修复前必然失败的断言：投影漏字段时徽标是空串，不会出现在 DOM 里。
	waitTextVisible(t, page, "#transfer-body", "direct", 15000)
}

// TestSyncCarrierBadge_MeshRemoteShowsDeclaredTransport 用 **mesh** 载体再钉一层：徽标为
// `<载体类型>/<transport>`（如 `mesh/relay`）——该字样在 UI 里唯一来源就是这条渲染路径，
// 比 `direct` 更不可能偶然命中。mesh 任务在未装配 mesh 工厂时**运行期**会失败，但任务本身
// 与列表字段仍然真实存在（本用例只关心「声明可见」）。
func TestSyncCarrierBadge_MeshRemoteShowsDeclaredTransport(t *testing.T) {
	baseURL, h, cfg, cleanup := testServerCfgWithHandlers(t, func(cfg *server.Config) {
		cfg.SyncRemotes = []server.SyncRemoteConfig{{
			Name: "r-mesh", Kind: "mesh", Node: "nodeB", Volume: "main",
			PeerPins: []string{e2eMeshPin}, Transport: "relay",
		}}
	})
	defer cleanup()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	exec := syncexec.NewExecutor(h.SyncTenantResolver(), logger)
	exec.SetTenantScopeResolver(h.SyncQuotaScope())
	exec.SetScopeResolver(h.SyncScopeFor())
	remotes := make([]syncmgr.RemoteConfig, 0, len(cfg.SyncRemotes))
	for _, r := range cfg.SyncRemotes {
		remotes = append(remotes, syncmgr.RemoteConfig{
			Name: r.Name, Kind: syncmgr.RemoteKind(r.Kind), URL: r.URL,
			AccessKey: r.AccessKey, AccessKeySecret: r.AccessKeySecret, AccessKeyID: r.AccessKeyID,
			Node: r.Node, Volume: r.Volume, PeerPins: r.PeerPins, Transport: r.Transport,
		})
	}
	sm := syncmgr.NewManager(h.SyncTenantResolver(), h.SyncTenantList(), nil,
		int(capacity.CategoryUserFiles), remotes, exec, logger,
		&syncmgr.Config{MaxConcurrent: 1, TaskTTL: time.Hour})
	sm.SetQuotaResolver(h.SyncQuotaStore())
	h.SetSyncMgr(sm)
	defer sm.Stop()

	seedSyncTask(t, baseURL, "r-mesh", "push")

	page, stop := pageFixture(t)
	defer stop()

	page.Goto(baseURL + "/ui/")
	if err := page.Locator("#main-tab-transfer").Click(); err != nil {
		t.Fatalf("点击传输 tab: %v", err)
	}
	if err := waitLoc(page, "#transfer-page", playwright.WaitForSelectorStateVisible, 8000); err != nil {
		t.Fatalf("传输页未显示: %v", err)
	}
	waitTextVisible(t, page, "#transfer-body", "mesh/relay", 15000)
}
