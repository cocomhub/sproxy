// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloud_NewLayout 验证云任务文件与状态落租户桶（P3 任务 13 新布局）：
//   - 任务文件 <root>/alice/cloud/<taskID>/<file>
//   - 任务状态 <root>/alice/meta/cloud/<taskID>.json
//   - kind=cloud_task 下载 filename=<taskID>/<file> 仍可用（按任务 owner 租户 FeatureRel 解析）
//   - 跨租户（bob）下载同一任务文件 → 404（SnapshotTask owner 过滤保持）
func TestCloud_NewLayout(t *testing.T) {
	env := newOwnerEnv(t)
	sm := capacity.NewStorageManager(env.root, 10*1024*1024*1024, nil, testLogger())
	mgr := cloud.NewCloudDownloadManager(env.root, cloudStorageManager{m: sm}, env.h.tenantFor, env.h.checksumStoreFor, env.h.listTenantIDs, testLogger(), defaultCloudDownloadConfig())
	env.h.cloudMgr = mgr
	t.Cleanup(func() { mgr.Close() })

	// 走**真实链路**完成一个任务（httptest 源 + SubmitAndStart）：管理器迁入 pkg/cloud 后，
	// 装配层测试不再注入领域内部（原来的 task.Status=completed + mgr.saveTask 注入已移除，
	// 改为由领域自己把状态与文件落到租户桶——这同时强化了用例：断言的是真实产物）。
	content := []byte("new-layout-content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()
	task, err := mgr.SubmitAndStart("url", srv.URL, "new-layout.bin", int64(len(content)), nil, "alice")
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	waitTaskDone(t, mgr, task.ID)

	// 状态文件应落 <root>/alice/meta/cloud/<taskID>.json
	persistFile := filepath.Join(env.root, "alice", "meta", "cloud", task.ID+".json")
	if _, err := os.Stat(persistFile); err != nil {
		t.Fatalf("任务状态文件应落 <root>/alice/meta/cloud/<taskID>.json: %v", err)
	}
	// 任务文件应落 <root>/alice/cloud/<taskID>/<file>
	taskDir := filepath.Join(env.root, "alice", "cloud", task.ID)
	if _, err := os.Stat(filepath.Join(taskDir, "new-layout.bin")); err != nil {
		t.Fatalf("任务文件应落 <root>/alice/cloud/<taskID>/<file>: %v", err)
	}

	// alice 下载 kind=cloud_task filename=<taskID>/<file> → 200
	dlPath := "/download?filename=" + url.QueryEscape(task.ID+"/new-layout.bin") + "&kind=cloud_task"
	if code := env.doGet(t, "alice", dlPath); code != http.StatusOK {
		t.Fatalf("alice 下载 cloud_task 应 200, got %d", code)
	}

	// bob（跨租户）下载 alice 的任务 → 404（SnapshotTask owner 过滤保持，不泄露存在性）
	bobMux := actorDownloadMux(env.h, "bob")
	req := httptest.NewRequest("GET", dlPath, nil)
	rr := httptest.NewRecorder()
	bobMux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("bob 下载 alice 的任务应 404, got %d", rr.Code)
	}
}
