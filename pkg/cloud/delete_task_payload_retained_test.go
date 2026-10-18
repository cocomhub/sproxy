// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// delete_task_payload_retained_test.go 钉住「删除未确认 ⇒ 不得归还配额」（独立审核 P2，2026-10-18）。
//
// 缺陷：DeleteTask 此前**先释放后删除**（`ReleaseCloud` + `releaseTaskScope` 在 `deleteTaskFiles`
// 之前），而删除失败（Windows 共享违规 / 杀软持句柄，重试耗尽）只 Warn ⇒ 文件仍在盘、账本已归还
// ⇒ 租户在下次扫描（≤30min）前可越过 `max_storage_bytes` / owner 配额（**fail-open**），且对用户
// 仍返回成功。
//
// 修法：改为「删除确认 ⇒ 才释放」——`deleteTaskFiles` 走可注入接缝 + 有界重试，之后用
// `taskPayloadLeft` 确认产物目录是否真消失；未消失 ⇒ **保留**预留（偏高是 fail-closed 方向，
// 由周期扫描收敛）并显式告警。
//
// 本用例用注入的 `RemoveFile` 让删除**确定性失败**（跨平台，不依赖 Windows 文件锁），断言账本
// 保留预留；对照：既有 TestCloudDownloadManager_DeleteTaskCleansAndReleases 覆盖删除成功 ⇒ 归还。

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudDownloadManager_DeleteTaskKeepsReservationWhenPayloadRemains 产物未确认删除时，
// 账本必须**保留**预留（未修必红：此前先释放后删除 ⇒ fail-open）。
func TestCloudDownloadManager_DeleteTaskKeepsReservationWhenPayloadRemains(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1024*1024, nil, testLogger())
	failRemove := func(string) error { return errors.New("injected: file in use") }
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig(),
		CloudManagerOptions{RemoveFile: failRemove})
	t.Cleanup(mgr.Close)

	task := &CloudTask{
		ID:           "cloud-keep-1",
		URL:          "https://example.com/f.txt",
		Status:       "completed",
		Filename:     "f.txt",
		TotalSize:    1000,
		ReservedSize: 1000, // 存储账本的权威字段（删除时按它释放）
	}
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	taskDir := filepath.Join(mgr.CloudDirFor(""), task.ID)
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, task.Filename), []byte("payload"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := mgr.storage.TryReserveCloud(1000); err != nil {
		t.Fatalf("TryReserveCloud: %v", err)
	}

	if err := mgr.DeleteTask(task.ID, ""); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	// 前置：产物确实仍在盘（注入的删除失败 ⇒ 这正是生产 fail-open 的触发条件）。
	if _, err := os.Stat(taskDir); err != nil {
		t.Fatalf("前置失败：产物目录应仍在（注入删除失败），实际 %v", err)
	}
	// 核心断言：账本必须保留预留（未修必红：0）。
	if got := sm.UsageByCategory()[capacity.CategoryCloud]; got != 1000 {
		t.Fatalf("产物未确认删除时账本必须保留预留（未修必红：先释放后删除 ⇒ fail-open ⇒ 可越过配额）；"+
			"实际 %d, want 1000", got)
	}
}
