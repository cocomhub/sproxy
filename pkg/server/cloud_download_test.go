// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestCloudDownloadManager_URLDedup(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	// 第一次创建
	task1, err := mgr.CreateTask("url", "https://example.com/same.zip", "same.zip", 100)
	if err != nil {
		t.Fatal(err)
	}

	// 第二次创建相同 URL → 应返回已有任务
	task2, err := mgr.CreateTask("url", "https://example.com/same.zip", "same.zip", 100)
	if err != nil {
		t.Fatal(err)
	}
	if task2.ID != task1.ID {
		t.Fatalf("expected same task ID for dedup, got %q vs %q", task1.ID, task2.ID)
	}

	// 不同 URL 应创建新任务
	task3, err := mgr.CreateTask("url", "https://example.com/different.zip", "different.zip", 100)
	if err != nil {
		t.Fatal(err)
	}
	if task3.ID == task1.ID {
		t.Fatal("expected different task ID for different URL")
	}
}

func TestCloudDownloadManager_URLDedupSkipFailedAndCancelled(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())

	// 创建失败任务
	task1, _ := mgr.CreateTask("url", "https://example.com/retry.zip", "retry.zip", 100)
	task1.Status = "failed"

	// 相同 URL 的失败任务应允许重新创建
	task2, err := mgr.CreateTask("url", "https://example.com/retry.zip", "retry.zip", 100)
	if err != nil {
		t.Fatal(err)
	}
	if task2.ID == task1.ID {
		t.Fatal("expected new task ID for failed task URL")
	}

	// 取消任务同理
	task2.Status = "cancelled"
	task3, err := mgr.CreateTask("url", "https://example.com/retry.zip", "retry.zip", 100)
	if err != nil {
		t.Fatal(err)
	}
	if task3.ID == task2.ID {
		t.Fatal("expected new task ID for cancelled task URL")
	}
}

func TestCloudDownloadManager_DeleteTaskCleansUpAll(t *testing.T) {
	dir := t.TempDir()
	cs := NewChecksumStore(dir, testLogger())
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, cs, testLogger(), defaultCloudDownloadConfig())

	task, _ := mgr.CreateTask("url", "https://example.com/cleanup.zip", "cleanup.zip", 100)
	task.Status = "completed"
	task.Checksum = "abc123"

	// 创建云端文件
	cloudDir := filepath.Join(dir, ".__cloud__", task.ID)
	os.MkdirAll(cloudDir, 0755)
	os.WriteFile(filepath.Join(cloudDir, "cleanup.zip"), []byte("test data"), 0644)

	// 写入 checksum
	remotePath := filepath.Join(cloudDirName, task.ID, "cleanup.zip")
	cs.Set(remotePath, "abc123")

	// 验证存储使用量 > 0
	if sm.Usage() == 0 {
		t.Fatal("expected storage usage > 0 before delete")
	}

	// 删除任务
	if err := mgr.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}

	// 验证任务已删除
	_, ok := mgr.GetTask(task.ID)
	if ok {
		t.Fatal("expected task to be deleted")
	}

	// 验证云端文件已删除
	if _, err := os.Stat(filepath.Join(cloudDir, "cleanup.zip")); !os.IsNotExist(err) {
		t.Error("cloud file should be deleted")
	}
	if _, err := os.Stat(cloudDir); !os.IsNotExist(err) {
		t.Error("cloud task dir should be deleted")
	}

	// 验证持久化文件已删除
	persistFile := filepath.Join(dir, ".__downloads__", task.ID+".json")
	if _, err := os.Stat(persistFile); !os.IsNotExist(err) {
		t.Error("persist file should be deleted")
	}

	// 验证存储空间已释放
	if sm.Usage() != 0 {
		t.Fatalf("expected storage usage=0 after delete, got %d", sm.Usage())
	}

	// 验证 checksum 已清理
	if _, ok := cs.Get(remotePath); ok {
		t.Error("checksum should be deleted")
	}
}

func TestCloudDownloadManager_SubmitAndStart_Sync(t *testing.T) {
	content := []byte("hello sync download")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	task, err := mgr.SubmitAndStart("url", srv.URL, "sync-test.bin", int64(len(content)), t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "completed" {
		t.Fatalf("expected status 'completed' for sync download, got %q", task.Status)
	}
	if task.Checksum == "" {
		t.Fatal("expected non-empty checksum")
	}
	if task.TotalSize != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), task.TotalSize)
	}

	// 验证文件已下载
	destPath := filepath.Join(dir, ".__cloud__", task.ID, "sync-test.bin")
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("expected %q, got %q", string(content), string(got))
	}
}

func TestCloudDownloadManager_SubmitAndStart_Async(t *testing.T) {
	content := make([]byte, 30*1024*1024) // 30MB > 20MB threshold
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	task, err := mgr.SubmitAndStart("url", srv.URL, "async-test.bin", int64(len(content)), t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// 使用快照读取任务状态，避免 data race
	snapshot, _ := mgr.SnapshotTask(task.ID)
	initialStatus := snapshot.Status
	if initialStatus != "pending" && initialStatus != "downloading" {
		t.Fatalf("expected status 'pending' or 'downloading' for async download, got %q", initialStatus)
	}

	// 轮询等待完成
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for async download to complete")
		case <-ticker.C:
			cur, ok := mgr.SnapshotTask(task.ID)
			if !ok {
				t.Fatal("task not found")
			}
			if cur.Status == "completed" {
				return
			}
			if cur.Status == "failed" {
				t.Fatalf("async download failed: %s", cur.Error)
			}
		}
	}
}

func TestCloudDownloadManager_SubmitAndStart_Dedup(t *testing.T) {
	content := []byte("dedup test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	// 第一次提交
	task1, _ := mgr.SubmitAndStart("url", srv.URL, "dedup.bin", int64(len(content)), t.Context())

	// 第二次提交相同 URL → 应返回已有任务
	task2, _ := mgr.SubmitAndStart("url", srv.URL, "dedup.bin", int64(len(content)), t.Context())
	if task2.ID != task1.ID {
		t.Fatalf("expected dedup ID %q, got %q", task1.ID, task2.ID)
	}
}

func TestCloudDownloadManager_CancelStopsDownload(t *testing.T) {
	// 模拟慢速下载：服务端阻塞不发送数据，等待取消
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "104857600") // 100MB
		w.WriteHeader(http.StatusOK)
		<-blockCh // 阻塞直到测试结束
	}))
	defer func() {
		close(blockCh)
		srv.Close()
	}()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	task, _ := mgr.SubmitAndStart("url", srv.URL, "cancel-test.bin", 104857600, nil) // nil context = async
	// 等待进入 downloading 状态
	time.Sleep(300 * time.Millisecond)

	// 取消任务
	if err := mgr.CancelTask(task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCloudDownloadManager_RecoverRestartsDownloading(t *testing.T) {
	content := []byte("resume test content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}

	// 创建 mgr1，创建任务，手动设置为 downloading 并持久化
	mgr1 := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	task, _ := mgr1.CreateTask("url", srv.URL, "resume.bin", int64(len(content)))
	task.Status = "downloading"
	task.UpdatedAt = time.Now()
	mgr1.saveTask(task)

	// 创建 mgr2 模拟重启，应自动恢复 downloading 任务
	mgr2 := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	// 等待恢复的任务完成
	deadline := time.After(10 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for recovered task to complete")
		case <-ticker.C:
			cur, _ := mgr2.GetTask(task.ID)
			if cur.Status == "completed" {
				return
			}
			if cur.Status == "failed" {
				t.Fatalf("recovered task failed: %s", cur.Error)
			}
		}
	}
}

func TestValidateCloudDownloadURL_Valid(t *testing.T) {
	url, filename, err := validateCloudDownloadURL("https://example.com/file.zip", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if url != "https://example.com/file.zip" {
		t.Fatalf("expected URL unchanged, got %q", url)
	}
	if filename != "file.zip" {
		t.Fatalf("expected extracted filename 'file.zip', got %q", filename)
	}
}

func TestValidateCloudDownloadURL_WithFilename(t *testing.T) {
	url, filename, err := validateCloudDownloadURL("https://example.com/data.bin", "custom.dat")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if filename != "custom.dat" {
		t.Fatalf("expected 'custom.dat', got %q", filename)
	}
	if url != "https://example.com/data.bin" {
		t.Fatalf("expected URL unchanged, got %q", url)
	}
}

func TestValidateCloudDownloadURL_EmptyURL(t *testing.T) {
	_, _, err := validateCloudDownloadURL("", "")
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestValidateCloudDownloadURL_InvalidScheme(t *testing.T) {
	_, _, err := validateCloudDownloadURL("ftp://example.com/file.zip", "")
	if err == nil {
		t.Fatal("expected error for ftp URL")
	}
}

func TestValidateCloudDownloadURL_PathTraversal(t *testing.T) {
	_, filename, err := validateCloudDownloadURL("https://example.com/file.zip", "../../../etc/passwd")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") {
		t.Fatalf("expected sanitized filename, got %q", filename)
	}
}

func TestValidateCloudDownloadURL_NoHost(t *testing.T) {
	_, _, err := validateCloudDownloadURL("not-a-url", "")
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestValidateCloudDownloadURL_QueryString(t *testing.T) {
	_, filename, err := validateCloudDownloadURL("https://example.com/download?file=test.zip&token=abc", "")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if filename != "download" {
		t.Fatalf("expected extracted filename 'download', got %q", filename)
	}
}

func TestCloudCleanupExpiredOnce_ClearsCompleted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	cfg.TaskTTL = 1 * time.Millisecond
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(mgr.StopFlush)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = "completed"
	task.UpdatedAt = time.Now().Add(-time.Hour)
	mgr.markDirty(task.ID)
	mgr.flushDirty()

	cleaned := mgr.cleanupExpiredOnce()
	if cleaned == 0 {
		t.Error("expected 1 task to be cleaned up")
	}
	mgr.mu.Lock()
	_, exists := mgr.tasks[task.ID]
	mgr.mu.Unlock()
	if exists {
		t.Error("expected completed task to be cleaned up after TTL")
	}
}

func TestCloudCleanupExpiredOnce_SkipsRunning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	cfg.TaskTTL = 1 * time.Millisecond
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(mgr.StopFlush)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	if err != nil {
		t.Fatal(err)
	}
	task.Status = "downloading"

	cleaned := mgr.cleanupExpiredOnce()
	if cleaned != 0 {
		t.Errorf("expected 0 cleanup for downloading task, got %d", cleaned)
	}
	mgr.mu.Lock()
	_, exists := mgr.tasks[task.ID]
	mgr.mu.Unlock()
	if !exists {
		t.Error("expected downloading task to persist after cleanupExpiredOnce")
	}
}

func TestCloudFlushDirty_PersistsTasks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(mgr.StopFlush)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	if err != nil {
		t.Fatal(err)
	}

	mgr.markDirty(task.ID)
	mgr.flushDirty()

	taskPath := filepath.Join(dir, ".__downloads__", task.ID+".json")
	if _, err := os.Stat(taskPath); os.IsNotExist(err) {
		t.Error("expected task persistence file after flushDirty")
	}
}

func TestCloudFlushNow_TriggersFlush(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(mgr.StopFlush)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	if err != nil {
		t.Fatal(err)
	}

	mgr.markDirty(task.ID)
	mgr.FlushNow()

	taskPath := filepath.Join(dir, ".__downloads__", task.ID+".json")
	if _, err := os.Stat(taskPath); os.IsNotExist(err) {
		t.Error("expected task persistence file after FlushNow")
	}
}
