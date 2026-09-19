// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"io"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudTask_Account_ReuseAcrossRetries 验证收敛后 downloadSinkFactory 复用
// task.account（非新建）：首轮会话创建 account 后，续传/重试的 SinkFactory 返回的
// adapter 委托同一 account（保留 committed/reserved），且 releaseTaskScope 收敛为
// account.Release（幂等归零）。
func TestCloudTask_Account_ReuseAcrossRetries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 1,
		MaxConcurrent: 1,
		TaskTTL:       time.Hour,
		FailedTaskTTL: time.Hour,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	h.setOwnerQuota("alice", 1000)
	owner := "alice"

	task, err := mgr.CreateTask("url", "https://example.com/reuse.bin", "reuse.bin", 100, owner)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// 手动装配首轮 account（模拟下载会话已立账）：从 cloud 桶 Scope 建 100 字节预留，
	// 其中 40 已 commit（reserved=60, committed=40）。
	scope := h.quotaBucketFor(owner, "cloud")
	if scope == nil {
		t.Fatal("cloud 桶 Scope 未装配（quotaFor 注入失败）")
	}
	acc, err := quota.NewTaskAccount(scope, 100)
	if err != nil {
		t.Fatalf("NewTaskAccount: %v", err)
	}
	if cuErr := acc.CommitUp(40); cuErr != nil {
		t.Fatalf("CommitUp(40): %v", cuErr)
	}
	task.account = acc

	// SinkFactory 复用同一 account：adapter 委托的 Committed 应为 40（非新建的 0）。
	factory := mgr.downloadSinkFactory(task)
	if factory == nil {
		t.Fatal("downloadSinkFactory 应非 nil（cloud 桶 Scope 已装配）")
	}
	sink, err := factory(io.Discard, 100, true) // resume=true：复用已有 account
	if err != nil {
		t.Fatalf("SinkFactory: %v", err)
	}
	adapter, ok := sink.(*quotaSinkAdapter)
	if !ok {
		t.Fatalf("sink 类型 = %T, want *quotaSinkAdapter", sink)
	}
	if got := adapter.Committed(); got != 40 {
		t.Fatalf("复用后 adapter Committed()=%d want 40（应保留首轮已 commit，非新建清零）", got)
	}
	if task.account != acc {
		t.Fatal("task.account 应为同一实例（复用而非新建）")
	}

	// releaseTaskScope 收敛：Release 幂等归零（committed 40 + reserved 60 全部回拨）。
	mgr.releaseTaskScope(task)
	if got := h.quotaBucketFor(owner, "cloud").Usage(); got != 0 {
		t.Fatalf("releaseTaskScope 后 cloud 桶 Usage()=%d want 0", got)
	}
	if got := h.quotaBucketFor(owner, "cloud").Reserved(); got != 0 {
		t.Fatalf("releaseTaskScope 后 cloud 桶 Reserved()=%d want 0", got)
	}
	if task.account != nil {
		t.Fatal("releaseTaskScope 后 task.account 应为 nil（已释放）")
	}
}
