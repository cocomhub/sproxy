// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
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
	if restored.Method != task.Method {
		t.Fatalf("expected Method %q, got %q", task.Method, restored.Method)
	}
	if restored.Filename != task.Filename {
		t.Fatalf("expected Filename %q, got %q", task.Filename, restored.Filename)
	}
	if restored.Status != task.Status {
		t.Fatalf("expected Status %q, got %q", task.Status, restored.Status)
	}
	if restored.TotalSize != task.TotalSize {
		t.Fatalf("expected TotalSize %d, got %d", task.TotalSize, restored.TotalSize)
	}
	if restored.Downloaded != task.Downloaded {
		t.Fatalf("expected Downloaded %d, got %d", task.Downloaded, restored.Downloaded)
	}
	if restored.Checksum != task.Checksum {
		t.Fatalf("expected Checksum %q, got %q", task.Checksum, restored.Checksum)
	}
	if restored.Error != task.Error {
		t.Fatalf("expected Error %q, got %q", task.Error, restored.Error)
	}
	if !restored.CreatedAt.Equal(task.CreatedAt) {
		t.Fatalf("expected CreatedAt %v, got %v", task.CreatedAt, restored.CreatedAt)
	}
	if !restored.UpdatedAt.Equal(task.UpdatedAt) {
		t.Fatalf("expected UpdatedAt %v, got %v", task.UpdatedAt, restored.UpdatedAt)
	}
	if !restored.ExpiresAt.Equal(task.ExpiresAt) {
		t.Fatalf("expected ExpiresAt %v, got %v", task.ExpiresAt, restored.ExpiresAt)
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
	mgr.mu.Lock()
	t1.Status = "completed"
	t2.Status = "failed"
	mgr.mu.Unlock()

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
	mgr.mu.Lock()
	task.Status = "downloading"
	mgr.mu.Unlock()

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
	mgr.mu.Lock()
	task.Status = "completed"
	mgr.mu.Unlock()

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
	mgr.mu.Lock()
	task.Status = "completed"
	mgr.mu.Unlock()

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

	// 创建两个任务并置为 completed（避免恢复时触发 pending 任务重启下载）
	t1, _ := mgr1.CreateTask("url", "https://example.com/a.zip", "a.zip", 100)
	t2, _ := mgr1.CreateTask("url", "https://example.com/b.zip", "b.zip", 200)
	mgr1.mu.Lock()
	t1.Status = "completed"
	t2.Status = "completed"
	mgr1.mu.Unlock()
	mgr1.saveTask(t1)
	mgr1.saveTask(t2)
	mgr1.Close() // 关闭 mgr1 的 flushLoop/cleanupExpired 后台 goroutine，避免与新 mgr 的清理逻辑交叉

	// 新建一个 manager 模拟重启
	mgr2 := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())
	mgr2.Close()
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
		AllowPrivate:  true,
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
	mgr.mu.Lock()
	task1.Status = "failed"
	mgr.mu.Unlock()

	// 相同 URL 的失败任务应允许重新创建
	task2, err := mgr.CreateTask("url", "https://example.com/retry.zip", "retry.zip", 100)
	if err != nil {
		t.Fatal(err)
	}
	if task2.ID == task1.ID {
		t.Fatal("expected new task ID for failed task URL")
	}

	// 取消任务同理
	mgr.mu.Lock()
	task2.Status = "cancelled"
	mgr.mu.Unlock()
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
	mgr.mu.Lock()
	task.Status = "completed"
	task.Checksum = "abc123"
	mgr.mu.Unlock()

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
		if _, err := w.Write(content); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
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
		if _, err := w.Write(content); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		AllowPrivate:  true,
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
	// 使用阻塞服务器，让第一个任务停留在 downloading 状态
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "104857600") // 100MB
		w.WriteHeader(http.StatusOK)
		<-blockCh // 阻塞直到测试结束
	}))
	t.Cleanup(func() {
		close(blockCh)
		srv.Close()
	})

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger()) // 1 GiB 上限
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		AllowPrivate:  true,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	// 第一次提交（异步，让任务停留在 downloading 状态）
	task1, err := mgr.SubmitAndStart("url", srv.URL, "dedup.bin", 104857600, nil)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	// 等待任务进入 downloading 状态
	for range 30 {
		cur, found := mgr.SnapshotTask(task1.ID)
		if !found {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if cur.Status == "downloading" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 第二次提交相同 URL → 应返回已有任务（pending/downloading 去重）
	task2, err := mgr.SubmitAndStart("url", srv.URL, "dedup.bin", 104857600, nil)
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if task2.ID != task1.ID {
		t.Fatalf("expected dedup ID %q, got %q", task1.ID, task2.ID)
	}
}

// TestCloudDownloadManager_SubmitAndStart_DedupPendingUsesRealObject 回归测试：
// CreateGroup 创建的任务停在 pending（尚无 goroutine），随后 SubmitAndStart 去重命中
// 该 pending 任务时必须用 m.tasks 中的真实对象启动。旧代码用 findByURL 的快照副本
// 启动，executeDownload 只写副本、真实对象永远停在 pending（findByURL 持续命中使
// 同 URL 无法再下载、任务卡死，直到进程重启自愈）。
func TestCloudDownloadManager_SubmitAndStart_DedupPendingUsesRealObject(t *testing.T) {
	content := []byte("dedup pending real object content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		_, _ = w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	// CreateTask 对未知大小任务预留 cloudReservePlaceholder（1 GiB），上限需大于该值
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		AllowPrivate:  true,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(mgr.Close)

	// 仅创建组（不启动），子任务停在 pending
	group, err := mgr.CreateGroup("g", []cloudfilename.Entry{{URL: srv.URL, Filename: "real.bin"}})
	if err != nil {
		t.Fatal(err)
	}
	taskID := group.TaskIDs[0]

	// 对组内同一 URL 提交下载：去重命中 pending 任务，应启动真实对象
	task, err := mgr.SubmitAndStart("url", srv.URL, "real.bin", -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != taskID {
		t.Fatalf("expected dedup to same task %q, got %q", taskID, task.ID)
	}

	// 等待真实对象离开 pending（旧 bug：真实对象永远停在 pending）。
	// 用 SnapshotTask 取快照副本读取，避免锁外读共享对象与 executeDownload 写状态竞争。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		real, ok := mgr.SnapshotTask(taskID)
		if ok && real.Status != "pending" {
			if real.Status == "completed" {
				return
			}
			t.Fatalf("real task %s reached %q instead of completed: %s", taskID, real.Status, real.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("real task %s never left pending (dedup goroutine ran on a snapshot copy)", taskID)
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
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		AllowPrivate:  true,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	task, _ := mgr.SubmitAndStart("url", srv.URL, "cancel-test.bin", 104857600, nil) // nil context = async
	// 等待进入 downloading 状态
	for range 30 {
		task, _ = mgr.SnapshotTask(task.ID)
		if task.Status == "downloading" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if task.Status != "downloading" {
		t.Fatalf("expected task to be downloading, got %s", task.Status)
	}

	// 取消任务
	if err := mgr.CancelTask(task.ID); err != nil {
		t.Fatal(err)
	}
}

// TestCloudDownloadManager_CancelCleansUpTaskDir 回归测试：取消下载后任务目录
// （含 .partial）必须被清理。旧代码只删最终文件，.partial 残留但存储账本已释放
// 归零，磁盘占用不被记账，可累计突破 max_storage_bytes 配额。
func TestCloudDownloadManager_CancelCleansUpTaskDir(t *testing.T) {
	// 服务端写入少量数据（使 .partial 落盘）后阻塞，等待取消
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "104857600") // 100MB
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some partial data"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-blockCh
	}))
	t.Cleanup(func() {
		close(blockCh)
		srv.Close()
	})

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		AllowPrivate:  true,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(mgr.Close)

	task, submitErr := mgr.SubmitAndStart("url", srv.URL, "cancel.bin", 104857600, nil)
	if submitErr != nil {
		t.Fatal(submitErr)
	}

	// 等待 .partial 文件出现（确认下载已开始写盘）
	taskDir := filepath.Join(dir, ".__cloud__", task.ID)
	deadline := time.Now().Add(5 * time.Second)
	partialWritten := false
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(filepath.Join(taskDir, "cancel.bin.partial")); statErr == nil {
			partialWritten = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !partialWritten {
		t.Fatal("expected partial file to be written before cancel")
	}

	// 取消任务，等待下载 goroutine 完全退出（旧 goroutine 停止写盘）
	if cancelErr := mgr.CancelTask(task.ID); cancelErr != nil {
		t.Fatal(cancelErr)
	}
	if !mgr.waitTaskStopped(task.ID, 2*time.Second) {
		t.Fatal("download goroutine did not stop after cancel")
	}

	// 任务目录应被清理（无 .partial/.partial.etag 残留；目录不存在或为空均可）
	entries, readErr := os.ReadDir(taskDir)
	if readErr == nil && len(entries) > 0 {
		t.Fatalf("expected task dir cleaned after cancel, found %d entries", len(entries))
	}
}

func TestCloudDownloadManager_RecoverRestartsDownloading(t *testing.T) {
	content := []byte("resume test content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		if _, err := w.Write(content); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		AllowPrivate:  true,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}

	// 创建 mgr1，创建任务，手动设置为 downloading 并持久化
	mgr1 := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	task, _ := mgr1.CreateTask("url", srv.URL, "resume.bin", int64(len(content)))
	mgr1.mu.Lock()
	task.Status = "downloading"
	task.UpdatedAt = time.Now()
	mgr1.mu.Unlock()
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
			cur, ok := mgr2.SnapshotTask(task.ID)
			if !ok {
				t.Fatal("task not found")
			}
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
	url, filename, err := validateCloudDownloadURL("https://example.com/file.zip", "", false)
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
	url, filename, err := validateCloudDownloadURL("https://example.com/data.bin", "custom.dat", false)
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
	_, _, err := validateCloudDownloadURL("", "", false)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
}

func TestValidateCloudDownloadURL_InvalidScheme(t *testing.T) {
	_, _, err := validateCloudDownloadURL("ftp://example.com/file.zip", "", false)
	if err == nil {
		t.Fatal("expected error for ftp URL")
	}
}

func TestValidateCloudDownloadURL_PathTraversal(t *testing.T) {
	_, _, err := validateCloudDownloadURL("https://example.com/file.zip", "../../../etc/passwd", false)
	if err == nil {
		t.Fatal("expected error for unsafe filename")
	}
}

func TestValidateCloudDownloadURL_NoHost(t *testing.T) {
	_, _, err := validateCloudDownloadURL("not-a-url", "", false)
	if err == nil {
		t.Fatal("expected error for malformed URL")
	}
}

func TestValidateCloudDownloadURL_QueryString(t *testing.T) {
	_, filename, err := validateCloudDownloadURL("https://example.com/download?file=test.zip&token=abc", "", false)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	// 新行为：查询参数附加在文件名后，经过 filepathSafe 后 ? 和 = 被替换为 _
	// 查询参数中的 = 和 & 在文件名中合法（多数系统允许），filepathSafe 保留它们
	if filename != "download_file=test.zip&token=abc" {
		t.Fatalf("expected extracted filename 'download_file=test.zip&token=abc', got %q", filename)
	}
}

func TestCloudCleanupExpiredOnce_ClearsCompleted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	cfg.TaskTTL = 1 * time.Millisecond
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(mgr.Close)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	task.Status = "completed"
	task.UpdatedAt = time.Now().Add(-time.Hour)
	mgr.mu.Unlock()
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
	t.Cleanup(mgr.Close)

	task, err := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 1024)
	if err != nil {
		t.Fatal(err)
	}
	mgr.mu.Lock()
	task.Status = "downloading"
	mgr.mu.Unlock()

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
	t.Cleanup(mgr.Close)

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
	t.Cleanup(mgr.Close)

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

func TestCloudDownloadManager_DeleteTaskCleansAndReleases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	task := &CloudTask{
		ID:           "cloud-test-1",
		URL:          "https://example.com/file.txt",
		Status:       "completed",
		Filename:     "file.txt",
		TotalSize:    1000,
		ReservedSize: 1000, // ReservedSize 是存储账本的权威字段，删除时按它释放
		Checksum:     "abc123",
	}
	mgr.mu.Lock()
	mgr.tasks[task.ID] = task
	mgr.mu.Unlock()

	taskDir := filepath.Join(mgr.cloudDir, task.ID)
	os.MkdirAll(taskDir, 0755)
	os.WriteFile(filepath.Join(taskDir, task.Filename), []byte("test"), 0644)
	mgr.storage.TryReserve(1000, CategoryCloud)

	if err := mgr.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Error("task dir should be deleted")
	}

	usage := mgr.storage.UsageByCategory()
	if usage[CategoryCloud] != 0 {
		t.Errorf("expected cloud size 0, got %d", usage[CategoryCloud])
	}
}

func TestCloudDownloadManager_ClientDisconnectDownloadContinues(t *testing.T) {
	content := []byte("client disconnect async retry test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		w.WriteHeader(http.StatusOK)
		for i := range content {
			if _, err := w.Write(content[i : i+1]); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	ctx, cancel := context.WithCancel(t.Context())

	task, _ := mgr.SubmitAndStart("url", srv.URL, "disconnect.bin", int64(len(content)), ctx)

	cancel()

	deadline := time.After(15 * time.Second)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			cur, _ := mgr.SnapshotTask(task.ID)
			t.Fatalf("timeout waiting for async retry: status=%s, error=%s", cur.Status, cur.Error)
		case <-ticker.C:
			cur, _ := mgr.SnapshotTask(task.ID)
			if cur.Status == "completed" {
				return
			}
			if cur.Status == "failed" {
				t.Fatalf("async retry failed: %s", cur.Error)
			}
		}
	}
}

func TestCloudDownloadManager_ConcurrentSemaphoreLimit(t *testing.T) {
	t.Parallel()
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "104857600")
		w.WriteHeader(http.StatusOK)
		<-blockCh
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 2,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() {
		mgr.Close()
		os.RemoveAll(filepath.Join(dir, ".__cloud__"))
		os.RemoveAll(filepath.Join(dir, ".__downloads__"))
	})

	task1, _ := mgr.SubmitAndStart("url", srv.URL+"?1", "block1.bin", 104857600, nil)
	task2, _ := mgr.SubmitAndStart("url", srv.URL+"?2", "block2.bin", 104857600, nil)
	task3, _ := mgr.SubmitAndStart("url", srv.URL+"?3", "block3.bin", 104857600, nil)

	allTasks := []*CloudTask{task1, task2, task3}

	// 等待任意 2 个任务进入 downloading 状态
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for 2 tasks to start downloading")
		default:
			downloading := 0
			for _, tk := range allTasks {
				s, _ := mgr.SnapshotTask(tk.ID)
				if s.Status == "downloading" {
					downloading++
				}
			}
			if downloading >= 2 {
				// 继续检查 10 轮，确认并发数始终不超过 2
				stableDeadline := time.After(2 * time.Second)
				stable := true
				for range 10 {
					select {
					case <-stableDeadline:
						break
					default:
						time.Sleep(20 * time.Millisecond)
						downloading = 0
						for _, tk := range allTasks {
							s, _ := mgr.SnapshotTask(tk.ID)
							if s.Status == "downloading" {
								downloading++
							}
						}
						if downloading > 2 {
							stable = false
							t.Errorf("并发数超过限制: %d > 2", downloading)
						}
					}
				}
				if stable {
					t.Logf("并发限制正常: 始终不超过 2 个 downloading")
				}
				close(blockCh)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestCloudDownloadManager_MetricsTracking(t *testing.T) {
	content := []byte("metrics test")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
		if _, err := w.Write(content); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	task, err := mgr.SubmitAndStart("url", srv.URL, "metrics.bin", int64(len(content)), t.Context())
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for download")
		default:
			cur, _ := mgr.SnapshotTask(task.ID)
			if cur.Status == "completed" {
				if mgr.metrics.TasksCreated.Load() < 1 {
					t.Errorf("TasksCreated should be >= 1, got %d", mgr.metrics.TasksCreated.Load())
				}
				if mgr.metrics.TasksCompleted.Load() < 1 {
					t.Errorf("TasksCompleted should be >= 1, got %d", mgr.metrics.TasksCompleted.Load())
				}
				if mgr.metrics.BytesDownloaded.Load() < 1 {
					t.Errorf("BytesDownloaded should be >= 1, got %d", mgr.metrics.BytesDownloaded.Load())
				}
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// --- 可靠性：重试 / 超时 / 排队取消 / 存储账本 / 续传 ---

func TestCloudDownloadManager_RetryOnTransientFailure(t *testing.T) {
	var attempts atomic.Int32
	content := []byte("retry success content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 1,
		MaxConcurrent: 3,
		TaskTTL:       time.Hour,
		FailedTaskTTL: time.Hour,
		AllowPrivate:  true,
		MaxRetries:    5,
		RetryDelay:    10 * time.Millisecond,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	task, err := mgr.SubmitAndStart("url", srv.URL, "retry.bin", -1, nil)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for retry download to complete")
		default:
			cur, ok := mgr.SnapshotTask(task.ID)
			if !ok {
				t.Fatal("task not found")
			}
			if cur.Status == "completed" {
				if attempts.Load() < 3 {
					t.Fatalf("expected >=3 attempts, got %d", attempts.Load())
				}
				if mgr.metrics.TasksRetried.Load() < 2 {
					t.Fatalf("expected TasksRetried >= 2, got %d", mgr.metrics.TasksRetried.Load())
				}
				return
			}
			if cur.Status == "failed" {
				t.Fatalf("task failed: %s", cur.Error)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestCloudDownloadManager_TimeoutThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	content := []byte("slow then fast content")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			time.Sleep(500 * time.Millisecond) // 超过 DownloadTimeout
		}
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold:   1,
		MaxConcurrent:   3,
		TaskTTL:         time.Hour,
		FailedTaskTTL:   time.Hour,
		AllowPrivate:    true,
		DownloadTimeout: 100 * time.Millisecond,
		MaxRetries:      3,
		RetryDelay:      10 * time.Millisecond,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	task, err := mgr.SubmitAndStart("url", srv.URL, "timeout-retry.bin", -1, nil)
	if err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for timeout-retry download to complete")
		default:
			cur, _ := mgr.SnapshotTask(task.ID)
			if cur.Status == "completed" {
				if attempts.Load() < 2 {
					t.Fatalf("expected >=2 attempts, got %d", attempts.Load())
				}
				return
			}
			if cur.Status == "failed" {
				t.Fatalf("task failed: %s", cur.Error)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestCloudDownloadManager_QueuedTaskCancellable(t *testing.T) {
	blockCh := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "104857600")
		w.WriteHeader(http.StatusOK)
		<-blockCh // 阻塞第一个任务，占满唯一并发槽
	}))
	t.Cleanup(func() {
		close(blockCh)
		srv.Close()
	})

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 1,
		TaskTTL:       time.Hour,
		FailedTaskTTL: time.Hour,
		AllowPrivate:  true,
		MaxRetries:    1,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	task1, err := mgr.SubmitAndStart("url", srv.URL, "block.bin", 104857600, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 等待 task1 进入 downloading
	waitStatus(t, mgr, task1.ID, "downloading")

	// 第二个任务排队（唯一并发槽被占）
	task2, err := mgr.SubmitAndStart("url", srv.URL+"/queued", "queued.bin", -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if cur, _ := mgr.SnapshotTask(task2.ID); cur.Status != "pending" {
		t.Fatalf("expected queued task to be pending, got %q", cur.Status)
	}

	// 排队任务应可取消
	if err := mgr.CancelTask(task2.ID); err != nil {
		t.Fatal(err)
	}
	if cur, _ := mgr.SnapshotTask(task2.ID); cur.Status != "cancelled" {
		t.Fatalf("expected queued task to be cancelled, got %q", cur.Status)
	}

	// Close 应能及时返回（排队任务已注册 cancelFuncs）
	done := make(chan struct{})
	go func() {
		mgr.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close() hung: queued task not cancellable")
	}
}

func waitStatus(t *testing.T, mgr *CloudDownloadManager, id, want string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			cur, _ := mgr.SnapshotTask(id)
			t.Fatalf("timeout waiting for %s status %q, got %q", id, want, cur.Status)
		default:
			if cur, ok := mgr.SnapshotTask(id); ok && cur.Status == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func waitTaskDone(t *testing.T, mgr *CloudDownloadManager, id string) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			cur, _ := mgr.SnapshotTask(id)
			t.Fatalf("timeout waiting for task %s terminal status, got %q (%s)", id, cur.Status, cur.Error)
		default:
			cur, ok := mgr.SnapshotTask(id)
			if !ok {
				t.Fatal("task not found")
			}
			if cur.Status == "completed" || cur.Status == "failed" || cur.Status == "cancelled" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
}

func TestCloudDownloadManager_StorageAccountingNoLeak(t *testing.T) {
	content := []byte("small unknown-size file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(content)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 1,
		MaxConcurrent: 3,
		TaskTTL:       time.Hour,
		FailedTaskTTL: time.Hour,
		AllowPrivate:  true,
	})
	t.Cleanup(func() { mgr.Close() })

	task, err := mgr.SubmitAndStart("url", srv.URL, "a.bin", -1, nil) // 未知大小 → 1 GiB 占位
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, task.ID)
	if cur, _ := mgr.SnapshotTask(task.ID); cur.Status != "completed" {
		t.Fatalf("expected completed, got %q (%s)", cur.Status, cur.Error)
	}
	// 完成后账本应对齐实际大小，而不是泄漏 1 GiB 占位
	if usage := sm.UsageByCategory()[CategoryCloud]; usage != int64(len(content)) {
		t.Fatalf("expected cloud usage %d after completion, got %d", len(content), usage)
	}

	// 取消一个未知大小任务：占位立即回收
	t2, err := mgr.CreateTask("url", "https://example.com/cancel-unknown.bin", "c.bin", -1)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.CancelTask(t2.ID); err != nil {
		t.Fatal(err)
	}
	if usage := sm.UsageByCategory()[CategoryCloud]; usage != int64(len(content)) {
		t.Fatalf("expected cloud usage %d after cancel, got %d", len(content), usage)
	}

	// 删除完成任务：占用归零
	if err := mgr.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if usage := sm.UsageByCategory()[CategoryCloud]; usage != 0 {
		t.Fatalf("expected cloud usage 0 after delete, got %d", usage)
	}
}

func TestCloudDownloadManager_FailedTaskKeepsPartialAndResumes(t *testing.T) {
	full := make([]byte, 1000)
	for i := range full {
		full[i] = byte(i % 251)
	}
	var sawRange atomic.Bool
	var firstAttempt atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			if r.Header.Get("Range") != "bytes=10-" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			sawRange.Store(true)
			w.Header().Set("Content-Range", "bytes 10-999/1000")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(full[10:])
			return
		}
		if firstAttempt.CompareAndSwap(false, true) {
			// 首次：只发送 10 字节后停流，触发超时
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(http.StatusOK)
			w.Write(full[:10])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			time.Sleep(2 * time.Second)
			return
		}
		w.Write(full)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold:   1,
		MaxConcurrent:   1,
		TaskTTL:         time.Hour,
		FailedTaskTTL:   time.Hour,
		AllowPrivate:    true,
		DownloadTimeout: 300 * time.Millisecond,
		MaxRetries:      1, // 首次失败后不再自动重试，等待手动 resume
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	task, err := mgr.SubmitAndStart("url", srv.URL, "resume.bin", -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, task.ID)
	if cur, _ := mgr.SnapshotTask(task.ID); cur.Status != "failed" {
		t.Fatalf("expected failed after first timeout, got %q (%s)", cur.Status, cur.Error)
	}

	// 失败后应保留 .partial（10 字节）供续传
	partialPath := filepath.Join(mgr.cloudDir, task.ID, "resume.bin.partial")
	fi, err := os.Stat(partialPath)
	if err != nil {
		t.Fatalf("expected partial file to be kept after failure: %v", err)
	}
	if fi.Size() != 10 {
		t.Fatalf("expected partial size 10, got %d", fi.Size())
	}

	// 手动续传：force=false 走 Range
	if err2 := mgr.ResumeTask(task.ID, false); err2 != nil {
		t.Fatal(err2)
	}
	waitTaskDone(t, mgr, task.ID)
	if cur, _ := mgr.SnapshotTask(task.ID); cur.Status != "completed" {
		t.Fatalf("expected completed after resume, got %q (%s)", cur.Status, cur.Error)
	}
	if !sawRange.Load() {
		t.Fatal("expected Range header on resume request")
	}
	dest := filepath.Join(mgr.cloudDir, task.ID, "resume.bin")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(full) {
		t.Fatal("resumed file content mismatch")
	}
	if usage := sm.UsageByCategory()[CategoryCloud]; usage != int64(len(full)) {
		t.Fatalf("expected cloud usage %d after resume, got %d", len(full), usage)
	}
}

func TestCloudDownloadManager_ResumeTaskForceTrueFullRedownload(t *testing.T) {
	full := []byte("force full redownload content")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Errorf("force resume must not send Range header")
		}
		requests.Add(1)
		w.Write(full)
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 1, MaxConcurrent: 1, TaskTTL: time.Hour, FailedTaskTTL: time.Hour,
		AllowPrivate: true, MaxRetries: 1,
	})
	t.Cleanup(func() { mgr.Close() })

	task, err := mgr.SubmitAndStart("url", srv.URL, "force.bin", -1, nil)
	if err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, task.ID)
	// 置 failed 准备 resume。通过 failTask 走正常释放路径，确保 StorageManager 账本一致
	// （不手动置 ReservedSize=0 绕过释放，否则云分类计数会虚高）。
	mgr.mu.Lock()
	initialStatus := task.Status
	mgr.mu.Unlock()
	if initialStatus == "completed" {
		// 已完成，直接置 failed 即可（ReservedSize 已由 downloadDone 对齐到实际大小）
		mgr.mu.Lock()
		task.Status = "failed"
		task.ReservedSize = 0
		mgr.mu.Unlock()
	} else {
		mgr.failTask(task, "test force resume")
	}
	if err := mgr.ResumeTask(task.ID, true); err != nil {
		t.Fatal(err)
	}
	waitTaskDone(t, mgr, task.ID)
	if cur, _ := mgr.SnapshotTask(task.ID); cur.Status != "completed" {
		t.Fatalf("expected completed, got %q (%s)", cur.Status, cur.Error)
	}
	if requests.Load() < 2 {
		t.Fatalf("expected >=2 full requests, got %d", requests.Load())
	}
}

// --- 任务组 ---

func TestCloudDownloadManager_GroupLifecycleAndPersistence(t *testing.T) {
	contentA := []byte("group file A content")
	contentB := []byte("group file B content")
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(contentA) }))
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(contentB) }))
	defer srvA.Close()
	defer srvB.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{SyncThreshold: 1, MaxConcurrent: 3, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true}
	mgr1 := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr1.Close() })

	group, err := mgr1.SubmitAndStartGroup("persist-group", []cloudfilename.Entry{
		{URL: srvA.URL, Filename: "a.bin"},
		{URL: srvB.URL, Filename: "b.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tid := range group.TaskIDs {
		waitTaskDone(t, mgr1, tid)
		if cur, _ := mgr1.SnapshotTask(tid); cur.Status != "completed" {
			t.Fatalf("expected completed task %s, got %q", tid, cur.Status)
		}
		// 子任务应带 GroupID
		mgr1.mu.RLock()
		stored := mgr1.tasks[tid]
		gid := stored.GroupID
		mgr1.mu.RUnlock()
		if gid != group.ID {
			t.Fatalf("expected task GroupID %q, got %q", group.ID, gid)
		}
	}

	// 组状态 completed
	mgr1.UpdateGroupStatus(group.ID)
	g, ok := mgr1.GetGroup(group.ID)
	if !ok {
		t.Fatal("group not found")
	}
	if g.Status != "completed" || g.Completed != 2 {
		t.Fatalf("expected group completed(2/2), got %s (%d/%d)", g.Status, g.Completed, g.TotalTasks)
	}

	// 组持久化文件存在
	if _, err := os.Stat(filepath.Join(dir, ".__downloads__", "groups", group.ID+".json")); err != nil {
		t.Fatalf("expected group persist file: %v", err)
	}

	// ArchiveFile 落库
	mgr1.SetGroupArchiveFile(group.ID, ".__cloud_archives__/g1.tar.gz")
	if g, _ := mgr1.GetGroup(group.ID); g.ArchiveFile != ".__cloud_archives__/g1.tar.gz" {
		t.Fatalf("expected archive_file set, got %q", g.ArchiveFile)
	}

	// 模拟重启：新 manager 恢复组
	mgr1.Close()
	mgr2 := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr2.Close() })
	g2, ok := mgr2.GetGroup(group.ID)
	if !ok {
		t.Fatal("group not recovered after restart")
	}
	if g2.Status != "completed" || len(g2.TaskIDs) != 2 || g2.ArchiveFile != ".__cloud_archives__/g1.tar.gz" {
		t.Fatalf("unexpected recovered group: %+v", g2)
	}
	if tasks := mgr2.ListTasks(""); len(tasks) != 2 {
		t.Fatalf("expected 2 recovered tasks, got %d", len(tasks))
	}

	// 删除组：任务、文件、组记录全部清理
	if err := mgr2.DeleteGroup(group.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr2.GetGroup(group.ID); ok {
		t.Fatal("group should be deleted")
	}
	if tasks := mgr2.ListTasks(""); len(tasks) != 0 {
		t.Fatalf("expected 0 tasks after group delete, got %d", len(tasks))
	}
	if usage := sm.UsageByCategory()[CategoryCloud]; usage != 0 {
		t.Fatalf("expected cloud usage 0 after group delete, got %d", usage)
	}
}

func TestCloudDownloadManager_GroupDuplicateURLRejected(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 1, MaxConcurrent: 3, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true,
	})
	t.Cleanup(func() { mgr.Close() })

	// 同一 URL 使用不同文件名 → 触发组内去重冲突（而非文件名冲突）
	_, err := mgr.SubmitAndStartGroup("dup", []cloudfilename.Entry{
		{URL: "https://example.com/a.zip", Filename: "one.zip"},
		{URL: "https://example.com/a.zip", Filename: "two.zip"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate URL error, got %v", err)
	}
	// 组创建失败后不应泄漏已创建的子任务（Critical #2 回归）
	if tasks := mgr.ListTasks(""); len(tasks) != 0 {
		t.Fatalf("expected 0 tasks after failed group creation (rollback), got %d", len(tasks))
	}
	if usage := sm.UsageByCategory()[CategoryCloud]; usage != 0 {
		t.Fatalf("expected 0 cloud usage after failed group creation (rollback), got %d", usage)
	}
}

// TestCloudDownloadManager_GroupStatusAutoUpdatedOnCompletion 验证任务完成后
// 组状态自动刷新（无需显式调用 UpdateGroupStatus）——Important #6 回归。
func TestCloudDownloadManager_GroupStatusAutoUpdatedOnCompletion(t *testing.T) {
	content := []byte("auto group status")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	defer srv.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{SyncThreshold: 1, MaxConcurrent: 3, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	group, err := mgr.SubmitAndStartGroup("auto", []cloudfilename.Entry{
		{URL: srv.URL + "/a.bin", Filename: "a.bin"},
		{URL: srv.URL + "/b.bin", Filename: "b.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tid := range group.TaskIDs {
		waitTaskDone(t, mgr, tid)
	}

	// 不调用 UpdateGroupStatus，直接读取组状态，应已由 refreshTaskGroup 自动刷新为 completed
	deadline := time.Now().Add(5 * time.Second)
	for {
		g, _ := mgr.GetGroup(group.ID)
		if g.Status == "completed" && g.Completed == 2 && g.TotalTasks == 2 {
			break
		}
		if time.Now().After(deadline) {
			g, _ := mgr.GetGroup(group.ID)
			t.Fatalf("group status not auto-updated, got %s (%d/%d)", g.Status, g.Completed, g.TotalTasks)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCloudDownloadManager_GroupStatusPartialAndCancel(t *testing.T) {
	content := []byte("ok file")
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(content) }))
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srvOK.Close()
	defer srv404.Close()

	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{SyncThreshold: 1, MaxConcurrent: 3, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })

	group, err := mgr.SubmitAndStartGroup("partial", []cloudfilename.Entry{
		{URL: srvOK.URL, Filename: "ok.bin"},
		{URL: srv404.URL, Filename: "bad.bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tid := range group.TaskIDs {
		waitTaskDone(t, mgr, tid)
	}
	mgr.UpdateGroupStatus(group.ID)
	g, _ := mgr.GetGroup(group.ID)
	if g.Status != "partial" || g.Completed != 1 || g.Failed != 1 {
		t.Fatalf("expected partial(1 completed, 1 failed), got %s (%d/%d)", g.Status, g.Completed, g.Failed)
	}

	// 取消已完成/已失败任务：组状态不应被强制改为 cancelled
	if err := mgr.CancelGroup(group.ID); err != nil {
		t.Fatal(err)
	}
	mgr.UpdateGroupStatus(group.ID)
	g, _ = mgr.GetGroup(group.ID)
	if g.Status != "partial" {
		t.Fatalf("expected partial after cancelling terminal tasks, got %q", g.Status)
	}
}

// TestCloudDownloadManager_GroupFilenameConflict 验证组创建前自动文件名冲突被拦截。
// 两个不同 URL 都推导出 index.html → 409 文件名冲突；指定不同保存文件名后可创建。
func TestCloudDownloadManager_GroupFilenameConflict(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), &CloudDownloadConfig{
		SyncThreshold: 1, MaxConcurrent: 3, TaskTTL: time.Hour, FailedTaskTTL: time.Hour, AllowPrivate: true,
	})
	t.Cleanup(func() { mgr.Close() })

	// 两个目录结尾 URL → 自动文件名都是 index.html → 冲突
	_, err := mgr.CreateGroup("conflict", []cloudfilename.Entry{
		{URL: "https://example.com/a/"},
		{URL: "https://example.com/b/"},
	})
	if err == nil || !strings.Contains(err.Error(), "filename conflict") {
		t.Fatalf("expected filename conflict error, got %v", err)
	}
	// 创建失败不应泄漏已创建的子任务与存储预留
	if tasks := mgr.ListTasks(""); len(tasks) != 0 {
		t.Fatalf("expected 0 tasks after conflict, got %d", len(tasks))
	}
	if usage := sm.UsageByCategory()[CategoryCloud]; usage != 0 {
		t.Fatalf("expected 0 cloud usage after conflict, got %d", usage)
	}

	// 显式指定不同保存文件名可消除冲突
	group, err := mgr.CreateGroup("ok", []cloudfilename.Entry{
		{URL: "https://example.com/a/", Filename: "a-index.html"},
		{URL: "https://example.com/b/", Filename: "b-index.html"},
	})
	if err != nil {
		t.Fatalf("expected group creation after specifying filenames, got %v", err)
	}
	if group.TotalTasks != 2 {
		t.Fatalf("expected 2 tasks, got %d", group.TotalTasks)
	}

	// 显式 Filename 含路径分隔符 → ResolveFilename 返回不安全文件名错误（只校验不修改）
	_, err = mgr.CreateGroup("still-conflict", []cloudfilename.Entry{
		{URL: "https://example.com/c/", Filename: "a/b.zip"},
		{URL: "https://example.com/d/", Filename: "a_b.zip"},
	})
	if err == nil || !strings.Contains(err.Error(), "unsafe characters") {
		t.Fatalf("expected unsafe filename error, got %v", err)
	}

	// 清理后真正唯一的文件名可创建成功（保证清理规则一致生效）
	g2, err := mgr.CreateGroup("ok2", []cloudfilename.Entry{
		{URL: "https://example.com/e/", Filename: "c_d.zip"},
		{URL: "https://example.com/f/", Filename: "cd.zip"},
	})
	if err != nil {
		t.Fatalf("expected group creation after sanitize makes names unique, got %v", err)
	}

	// 清理已创建组（避免影响测试隔离）
	if err := mgr.DeleteGroup(group.ID); err != nil {
		t.Fatalf("DeleteGroup %s: %v", group.ID, err)
	}
	if err := mgr.DeleteGroup(g2.ID); err != nil {
		t.Fatalf("DeleteGroup %s: %v", g2.ID, err)
	}
}
