// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_audit_cleanup_test.go 钉住审计文件卫生（对抗性评审 R1）：审计 sink 文件
// （<TMPDIR>/sproxy-audit/<taskID>.audit.log）随任务删除/TTL 过期清理——防每任务一文件
// 无限累积（删除/过期路径此前不动审计文件）。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// writeAuditFileFor 在审计 sink 目录写入任务对应的审计文件（模拟 newTaskAuditScope 落盘）。
func writeAuditFileFor(t *testing.T, mgr *CloudDownloadManager, taskID string) string {
	t.Helper()
	auditFile := filepath.Join(mgr.auditSinkDir(), taskID+".audit.log")
	if err := os.MkdirAll(filepath.Dir(auditFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(auditFile, []byte("{\"type\":\"download\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return auditFile
}

// TestDeleteTask_RemovesAuditFile（R1）：DeleteTask 后审计文件不存在。
func TestDeleteTask_RemovesAuditFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 4*1024*1024, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatal(err)
	}
	auditFile := writeAuditFileFor(t, mgr, task.ID)

	if err := mgr.DeleteTask(task.ID, ""); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	if _, serr := os.Stat(auditFile); !os.IsNotExist(serr) {
		t.Fatalf("删除任务后审计文件仍存在（stat err=%v）", serr)
	}
}

// TestCleanupExpired_RemovesAuditFile（R1）：TTL 过期清理后审计文件不存在。
func TestCleanupExpired_RemovesAuditFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 4*1024*1024, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	cfg.TaskTTL = 1 * time.Millisecond
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatal(err)
	}
	auditFile := writeAuditFileFor(t, mgr, task.ID)

	mgr.mu.Lock()
	task.Status = "completed"
	task.UpdatedAt = time.Now().Add(-time.Hour)
	mgr.mu.Unlock()
	mgr.markDirty(task.ID)
	mgr.flushDirty()

	if cleaned := mgr.cleanupExpiredOnce(); cleaned == 0 {
		t.Fatal("expected 1 task to be cleaned up")
	}
	if _, serr := os.Stat(auditFile); !os.IsNotExist(serr) {
		t.Fatalf("过期清理后审计文件仍存在（stat err=%v）", serr)
	}
}
