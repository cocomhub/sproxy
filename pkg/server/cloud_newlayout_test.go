// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestCloud_NewLayout 验证云任务文件与状态落租户桶（P3 任务 13 新布局）：
//   - 任务文件 <root>/alice/cloud/<taskID>/<file>
//   - 任务状态 <root>/alice/meta/cloud/<taskID>.json
//   - kind=cloud_task 下载 filename=<taskID>/<file> 仍可用（按任务 owner 租户 FeatureRel 解析）
//   - 跨租户（bob）下载同一任务文件 → 404（SnapshotTask owner 过滤保持）
func TestCloud_NewLayout(t *testing.T) {
	env := newOwnerEnv(t)
	sm := NewStorageManager(env.root, 10*1024*1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(env.root, sm, env.h.tenantFor, env.h.checksumStoreFor, env.h.listTenantIDs, testLogger(), defaultCloudDownloadConfig())
	env.h.cloudMgr = mgr
	t.Cleanup(func() { mgr.Close() })

	// 创建任务（owner=alice）→ 状态文件应落 <root>/alice/meta/cloud/<taskID>.json
	content := []byte("new-layout-content")
	task, err := mgr.CreateTask("url", "https://example.com/new-layout.bin", "new-layout.bin", int64(len(content)), "alice")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	persistFile := filepath.Join(env.root, "alice", "meta", "cloud", task.ID+".json")
	if _, err := os.Stat(persistFile); err != nil {
		t.Fatalf("任务状态文件应落 <root>/alice/meta/cloud/<taskID>.json: %v", err)
	}

	// 置 completed 并落盘任务文件（模拟下载完成）：<root>/alice/cloud/<taskID>/<file>
	task.Status = "completed"
	if err := mgr.saveTask(task); err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(env.root, "alice", "cloud", task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "new-layout.bin"), content, 0o644); err != nil {
		t.Fatal(err)
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
