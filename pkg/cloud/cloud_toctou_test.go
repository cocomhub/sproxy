// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// cloud_toctou_test.go 是云任务删除/取消路径 TOCTOU 窗口的钉住测试
// （2026-09-17 安全加固，计划 2026-09-17-security-toctou.md 任务 2）。
//
// 现状结论（先读代码再断言）：DeleteTask/CancelTask 的文件删除与终态发布均已闭合：
//   - DeleteTask：m.mu 锁内先 delete(m.tasks, id)（终态先行），锁外删文件——文件删除
//     只作用于本任务目录（taskDir = <cloud>/<taskID>/），路径由任务 ID 派生，不存在
//     「基于过期文件发布」的路径（终态只有存在性，无内容/大小判据）；
//   - releaseTaskScope 幂等（QuotaCommitted/ReservedSize 归零），#290/#315 已把
//     「先删文件后发布终态」「重试不重复回拨」钉住（见 lifecycle_invariant_test.go）。
//
// 故本文件补**钉住测试**（无生产改动）：删除已完成任务时，任务文件被并发替换为
// 更大/更小的其它内容，删除仍成功且 storageMgr 按 ReservedSize（非盘上大小）释放
// ——账本与「被删除对象」无关，不会因替换产生超额释放。

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// TestCloudDeleteTask_FileReplacedBeforeDelete 验证：删除已完成任务时，任务文件被
// 并发替换为不同大小的内容，删除成功、storageMgr 释放量按创建期 ReservedSize 精确
// 收敛（不受替换内容大小影响——不存在「按过期文件大小回拨」路径）。
func TestCloudDeleteTask_FileReplacedBeforeDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024, "")
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	task.Status = "completed"
	mgr.mu.Unlock()

	// 创建云端文件（大小 = 预留 1024）。
	cloudDir := filepath.Join(mgr.CloudDirFor(""), task.ID)
	if err := os.MkdirAll(cloudDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloudDir, "file.zip"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	// 删除前把任务文件替换为更大内容（模拟并发写者/残留产物膨胀）。
	if err := os.WriteFile(filepath.Join(cloudDir, "file.zip"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	before := sm.Usage()
	if err := mgr.DeleteTask(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	// storageMgr 按 ReservedSize 释放：创建期预留 1024，删除后 Usage 归零（回到初始）。
	if got := sm.Usage(); got != before-1024 {
		t.Fatalf("删除后 storageMgr Usage=%d want %d（按 ReservedSize 释放，不受替换大小影响）", got, before-1024)
	}
	if _, ok := mgr.GetTask(task.ID, ""); ok {
		t.Fatal("任务应已删除")
	}
	// 任务文件目录应已清理。
	if _, err := os.Stat(cloudDir); !os.IsNotExist(err) {
		t.Fatalf("任务目录应已删除, stat err=%v", err)
	}
}
