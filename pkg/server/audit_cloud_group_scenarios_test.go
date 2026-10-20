// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_cloud_group_scenarios_test.go 补设计 §9.1「云任务（cloud groups）」审计场景：
// `cloud_group_create` / `cloud_group_delete` —— 组 = 批量下载（批量落盘），创建/删除都要可追溯。
//
// 复用 `newAssemblyTestHandlers` + `cloud.NewCloudDownloadManager`（与 `setupCloudTestServer`
// 同一装配，额外返回 `*Handlers` 以便走 `/api/audit` 同一读入口断言）。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestAuditScenario_CloudGroup 创建组 + 删除组 ⇒ 两条审计（object=组 ID）。
func TestAuditScenario_CloudGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	h := newAssemblyTestHandlers(t, dir)
	h.storageMgr = sm
	h.cloudMgr = cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{
		UploadsDir:       dir,
		Storage:          cloudStorageManager{m: sm},
		TenantFor:        h.tenantFor,
		ChecksumStoreFor: h.checksumStoreFor,
		ListTenants:      h.listTenantIDs,
		Logger:           testLogger(),
		Config: &cloud.CloudDownloadConfig{
			SyncThreshold: 20 * 1024 * 1024, MaxConcurrent: 3,
			TaskTTL: 24 * time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true,
		},
	})
	attachTestAudit(t, h)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/groups", h.cloudCreateGroup)
	mux.HandleFunc("DELETE /api/cloud/groups/{id}", h.cloudDeleteGroup)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 两个内容源（组需要 ≥2 个 url）。
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer content.Close()
	body, _ := json.Marshal(map[string]any{
		"name": "audit-group",
		"urls": []map[string]string{
			{"url": content.URL + "/a.bin", "filename": "a.bin"},
			{"url": content.URL + "/b.bin", "filename": "b.bin"},
		},
	})
	resp, err := testHTTPClient(t).Post(ts.URL+"/api/cloud/groups", contentTypeJSON, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	var group cloud.CloudTaskGroup
	derr := json.NewDecoder(resp.Body).Decode(&group)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || derr != nil {
		t.Fatalf("create group status=%d err=%v", resp.StatusCode, derr)
	}
	if group.ID == "" {
		t.Fatal("组响应应含 ID")
	}

	delReq, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/cloud/groups/"+group.ID, nil)
	delResp, derr2 := testHTTPClient(t).Do(delReq)
	if derr2 != nil {
		t.Fatalf("delete group: %v", derr2)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK && delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete group status = %d", delResp.StatusCode)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	var sawCreate, sawDelete bool
	for _, ev := range entries {
		switch ev.Action {
		case "cloud_group_create":
			if ev.Object == group.ID {
				sawCreate = true
			}
		case "cloud_group_delete":
			if ev.Object == group.ID {
				sawDelete = true
			}
		}
	}
	if !sawCreate {
		t.Error("缺少 cloud_group_create 审计行（批量落盘必须可追溯 ✗）")
	}
	if !sawDelete {
		t.Error("缺少 cloud_group_delete 审计行")
	}
}
