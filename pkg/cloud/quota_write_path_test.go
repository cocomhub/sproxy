// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// 本用例原在 pkg/server/quota_write_path_test.go：它驱动的是**领域内**的配额结算路径
// （`failTask` 把已写入的 partial 计入配额 → resume 全量重下时增长超限被拒），
// 云任务管理器迁入本包后随之迁入（装配层测试不再触碰领域内部方法）。
//
// 唯一改动：装配层测试基座（Handlers.cfgPtr/h.quotaFor/newCloudTestManager）换成本包
// 的等价环境（cloudTestEnv.setOwnerQuota/quotaFor）；其余逐字不变。

func TestQuota_CloudResumeGrowthRejected(t *testing.T) {
	content := make([]byte, 120)
	for i := range content {
		content[i] = byte(i % 251)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	h.setOwnerQuota("alice", 100)

	// 创建任务（预留 90）+ 模拟失败落 90 字节 partial → failTask Commit(90)（reservation 消费）
	task, err := mgr.CreateTask("url", srv.URL, "resume.bin", 90, "alice")
	if err != nil {
		t.Fatal(err)
	}
	taskDir := filepath.Join(mgr.CloudDirFor("alice"), task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, 90)
	for i := range partial {
		partial[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "resume.bin.partial"), partial, 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.failTask(task, "simulated failure")
	if got := h.quotaFor("alice").Usage(); got != 90 {
		t.Fatalf("failTask 后 Usage()=%d want 90", got)
	}

	// resume（force 全量重下）→ 增长到 120 超过租户上限 100 → 应失败且 Scope 归零
	if err := mgr.ResumeTask(task.ID, true, "alice"); err != nil {
		t.Fatalf("ResumeTask: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, ok := mgr.SnapshotTask(task.ID, "alice")
		if !ok {
			t.Fatal("task disappeared")
		}
		if snap.Status == "failed" || snap.Status == "completed" || snap.Status == "cancelled" {
			if snap.Status != "failed" {
				t.Fatalf("resume 增长超限应 failed, got %q", snap.Status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resume 下载超时未进入终态")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.quotaFor("alice").Usage(); got != 0 {
		t.Fatalf("resume 失败清理后 Usage()=%d want 0（旧 committed 已释放）", got)
	}
}
