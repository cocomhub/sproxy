// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudTasksAdapter_SubmitReturnsSnapshot 锁定「提交返回快照」：
// HTTP 层解锁后序列化响应，若返回活对象会与下载 goroutine 的字段改写并发（DATA RACE）。
func TestCloudTasksAdapter_SubmitReturnsSnapshot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<20, nil, testLogger())
	cfg := &cloud.CloudDownloadConfig{
		SyncThreshold: 20 << 20, MaxConcurrent: 1,
		TaskTTL: 24 * time.Hour, FailedTaskTTL: time.Hour,
	}
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)
	adapter := &cloudTasksAdapter{h: &Handlers{cloudMgr: mgr}}

	task, err := adapter.SubmitAndStart("url", "https://example.com/s/EXAMPLE-ADAPTER-1", "a.bin", 1024,
		context.Background(), "", cloud.TaskParams{Save: true})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}
	// 返回的对象必须是快照：修改它不应影响管理器内的任务。
	task.Status = "mutated-by-test"
	live, ok := mgr.SnapshotTask(task.ID, "")
	if !ok {
		t.Fatal("任务应存在")
	}
	if live.Status == "mutated-by-test" {
		t.Fatal("返回的应是快照（修改不应影响管理器内任务）")
	}
}
