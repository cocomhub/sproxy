// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCloudTask_JSONRoundTrip(t *testing.T) {
	task := &CloudTask{
		ID:         "test-id-123",
		URL:        "https://example.com/file.zip",
		Method:     "url",
		Filename:   "file.zip",
		Status:     "pending",
		TotalSize:  1024,
		Downloaded: 0,
		Checksum:   "abc123",
		Error:      "",
		CreatedAt:  time.Now().Truncate(time.Second),
		UpdatedAt:  time.Now().Truncate(time.Second),
		ExpiresAt:  time.Now().Add(24 * time.Hour).Truncate(time.Second),
	}

	data, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}

	var restored CloudTask
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}

	if restored.ID != task.ID {
		t.Fatalf("expected ID %q, got %q", task.ID, restored.ID)
	}
	if restored.URL != task.URL {
		t.Fatalf("expected URL %q, got %q", task.URL, restored.URL)
	}
	if restored.Status != task.Status {
		t.Fatalf("expected Status %q, got %q", task.Status, restored.Status)
	}
}

func TestCloudDownloadManager_CreateTask(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID == "" {
		t.Fatal("expected non-empty task ID")
	}
	if task.Status != "pending" {
		t.Fatalf("expected status 'pending', got %q", task.Status)
	}
	if task.Method != "url" {
		t.Fatalf("expected method 'url', got %q", task.Method)
	}
}

func TestCloudDownloadManager_CreateTaskReservesStorage(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 100, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	_, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 200)
	if err != ErrStorageFull {
		t.Fatalf("expected ErrStorageFull, got %v", err)
	}
}

func TestCloudDownloadManager_GetTask(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)

	got, ok := mgr.GetTask(task.ID)
	if !ok {
		t.Fatal("expected to find task")
	}
	if got.ID != task.ID {
		t.Fatalf("expected ID %q, got %q", task.ID, got.ID)
	}
}

func TestCloudDownloadManager_GetTaskMissing(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	_, ok := mgr.GetTask("nonexistent")
	if ok {
		t.Fatal("expected false for missing task")
	}
}

func TestCloudDownloadManager_ListTasks(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100)
	mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200)

	tasks := mgr.ListTasks("")
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestCloudDownloadManager_ListTasksFilterByStatus(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	t1, _ := mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100)
	t2, _ := mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200)
	t1.Status = "completed"
	t2.Status = "failed"

	completed := mgr.ListTasks("completed")
	if len(completed) != 1 {
		t.Fatalf("expected 1 completed task, got %d", len(completed))
	}
	failed := mgr.ListTasks("failed")
	if len(failed) != 1 {
		t.Fatalf("expected 1 failed task, got %d", len(failed))
	}
}

func TestCloudDownloadManager_CancelTask(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	task.Status = "downloading"

	err := mgr.CancelTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "cancelled" {
		t.Fatalf("expected status 'cancelled', got %q", task.Status)
	}
}

func TestCloudDownloadManager_CancelTaskInvalidStatus(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	task.Status = "completed"

	err := mgr.CancelTask(task.ID)
	if err == nil {
		t.Fatal("expected error when cancelling completed task")
	}
}

func TestCloudDownloadManager_DeleteTask(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	task.Status = "completed"

	// 创建云端文件
	cloudDir := filepath.Join(dir, ".__cloud__", task.ID)
	os.MkdirAll(cloudDir, 0755)
	os.WriteFile(filepath.Join(cloudDir, "file.zip"), []byte("data"), 0644)

	err := mgr.DeleteTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := mgr.GetTask(task.ID)
	if ok {
		t.Fatal("expected task to be deleted")
	}
}

func TestCloudDownloadManager_TaskPersistence(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)

	// 验证持久化文件存在
	taskFile := filepath.Join(dir, ".__downloads__", task.ID+".json")
	if _, err := os.Stat(taskFile); err != nil {
		t.Fatalf("expected task file %s to exist: %v", taskFile, err)
	}
}

func TestCloudDownloadManager_RecoverTasks(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr1 := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	// 创建两个任务
	mgr1.CreateTask("url", "https://example.com/a.zip", "a.zip", 100)
	mgr1.CreateTask("url", "https://example.com/b.zip", "b.zip", 200)

	// 新建一个 manager 模拟重启
	mgr2 := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())
	tasks := mgr2.ListTasks("")
	if len(tasks) != 2 {
		t.Fatalf("expected 2 recovered tasks, got %d", len(tasks))
	}
}

func defaultCloudDownloadConfig() *CloudDownloadConfig {
	return &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024, // 20 MiB
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
}
