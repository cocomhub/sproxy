// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func setupCloudTestServer(t *testing.T) (*httptest.Server, *CloudDownloadManager) {
	t.Helper()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	h := &Handlers{cloudMgr: mgr}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download", h.cloudCreateDownload)
	mux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	mux.HandleFunc("GET /api/cloud/tasks", h.cloudListTasks)
	mux.HandleFunc("GET /api/cloud/tasks/{id}", h.cloudGetTask)
	mux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.cloudCancelTask)
	mux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.cloudDeleteTask)
	return httptest.NewServer(mux), mgr
}

func TestCloudHandler_CreateDownloadTask(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"url": "https://example.com/file.zip", "filename": "file.zip"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var task CloudTask
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatal(err)
	}
	if task.ID == "" {
		t.Fatal("expected non-empty task ID")
	}
	if task.Status != "pending" {
		t.Fatalf("expected status 'pending', got %q", task.Status)
	}
}

func TestCloudHandler_ListTasks(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100)
	mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200)

	resp, err := http.Get(ts.URL + "/api/cloud/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var tasks []*CloudTask
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(tasks))
	}
}

func TestCloudHandler_GetTask(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100)

	resp, err := http.Get(ts.URL + "/api/cloud/tasks/" + task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var got CloudTask
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.ID != task.ID {
		t.Fatalf("expected ID %q, got %q", task.ID, got.ID)
	}
}

func TestCloudHandler_GetTaskNotFound(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/cloud/tasks/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestCloudHandler_CancelTask(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100)
	task.Status = "downloading"

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+task.ID+"/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestCloudHandler_DeleteTask(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100)
	task.Status = "completed"

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/cloud/tasks/"+task.ID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestCloudHandler_ListTasksFilterByStatus(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	t1, _ := mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100)
	t2, _ := mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200)
	t1.Status = "completed"
	t2.Status = "failed"

	resp, err := http.Get(ts.URL + "/api/cloud/tasks?status=completed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var tasks []*CloudTask
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("expected 1 completed task, got %d", len(tasks))
	}
}

func TestCloudHandler_SSRFBlocked(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	tests := []struct {
		url    string
		expect int
	}{
		{"ftp://example.com/file.zip", http.StatusBadRequest},
		{"", http.StatusBadRequest},
		{"not-a-url", http.StatusBadRequest},
		{"https://example.com/file.zip", http.StatusOK},
	}
	for _, tt := range tests {
		body := strings.NewReader(`{"url": "` + tt.url + `"}`)
		resp, err := http.Post(ts.URL+"/api/cloud/download", "application/json", body)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tt.expect {
			t.Errorf("URL %q: expected %d, got %d", tt.url, tt.expect, resp.StatusCode)
		}
	}
}

func TestCloudHandler_PathTraversalBlocked(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"url": "https://example.com/file.zip", "filename": "../../../etc/passwd"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var task CloudTask
	json.NewDecoder(resp.Body).Decode(&task)
	if strings.Contains(task.Filename, "/") || strings.Contains(task.Filename, "\\") {
		t.Fatalf("filename should be sanitized, got %q", task.Filename)
	}
}

// --- 批量下载 handler 测试 ---

func TestCloudHandler_BatchCreateDownload_Success(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"urls": [{"url": "https://example.com/a.zip"}, {"url": "https://example.com/b.zip"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatal(err)
	}
	if len(batchResp.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(batchResp.Tasks))
	}
	for _, tr := range batchResp.Tasks {
		if tr.ID == "" {
			t.Fatal("expected non-empty task ID")
		}
		if tr.Status != "pending" {
			t.Fatalf("expected status 'pending', got %q", tr.Status)
		}
	}
}

func TestCloudHandler_BatchCreateDownload_Empty(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"urls": []}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCloudHandler_BatchCreateDownload_InvalidJSON(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`not json`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestCloudHandler_BatchCreateDownload_MixedResults(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"urls": [{"url": "https://example.com/valid.zip"}, {"url": "ftp://example.com/bad.zip"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatal(err)
	}
	if len(batchResp.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(batchResp.Tasks))
	}
	// 第一个有效 URL 应成功
	if batchResp.Tasks[0].Status != "pending" {
		t.Fatalf("expected first task status 'pending', got %q", batchResp.Tasks[0].Status)
	}
	if batchResp.Tasks[0].ID == "" {
		t.Fatal("expected non-empty ID for valid URL")
	}
	// 第二个无效 URL 应失败
	if batchResp.Tasks[1].Status != "failed" {
		t.Fatalf("expected second task status 'failed', got %q", batchResp.Tasks[1].Status)
	}
	if batchResp.Tasks[1].Error == "" {
		t.Fatal("expected error message for invalid URL")
	}
}

func TestCloudHandler_BatchCreateDownload_EmptyURL(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"urls": [{"url": ""}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatal(err)
	}
	if batchResp.Tasks[0].Status != "failed" {
		t.Fatalf("expected 'failed' status for empty URL, got %q", batchResp.Tasks[0].Status)
	}
}

func TestCloudHandler_BatchCreateDownload_PathTraversal(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"urls": [{"url": "https://example.com/file.zip", "filename": "../../../etc/passwd"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatal(err)
	}
	if batchResp.Tasks[0].Status != "pending" && batchResp.Tasks[0].Status != "downloading" {
		t.Fatalf("expected 'pending' or 'downloading', got %q", batchResp.Tasks[0].Status)
	}
	if strings.Contains(batchResp.Tasks[0].Filename, "/") || strings.Contains(batchResp.Tasks[0].Filename, "\\") {
		t.Fatalf("filename should be sanitized, got %q", batchResp.Tasks[0].Filename)
	}
}

func TestCloudHandler_BatchCreateDownload_Dedup(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	// 提交相同 URL 两次
	body := strings.NewReader(`{"urls": [{"url": "https://example.com/same.zip"}, {"url": "https://example.com/same.zip"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatal(err)
	}
	if batchResp.Tasks[0].ID != batchResp.Tasks[1].ID {
		t.Fatalf("expected same task ID for dedup, got %q and %q",
			batchResp.Tasks[0].ID, batchResp.Tasks[1].ID)
	}
}

func TestCloudHandler_BatchCreateDownload_AlwaysAsync(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	// 小文件（< 20 MiB）在批量模式下也应返回 pending（异步）
	body := strings.NewReader(`{"urls": [{"url": "https://example.com/small.zip"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	json.NewDecoder(resp.Body).Decode(&batchResp)
	if batchResp.Tasks[0].Status != "pending" {
		t.Fatalf("expected batch mode to always be async, got status %q", batchResp.Tasks[0].Status)
	}
}

func TestCloudHandler_BatchCreateDownload_StorageFull(t *testing.T) {
	dir := t.TempDir()
	// 创建存储空间仅 50 字节的 manager
	sm := NewStorageManager(dir, 50, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)

	h := &Handlers{cloudMgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 请求 100 字节，超过 50 字节上限
	body := strings.NewReader(`{"urls": [{"url": "https://example.com/big.zip"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	json.NewDecoder(resp.Body).Decode(&batchResp)
	if batchResp.Tasks[0].Status != "failed" {
		t.Fatalf("expected 'failed' for storage full, got %q", batchResp.Tasks[0].Status)
	}
}

func TestCloudHandler_BatchCreateDownload_MaxLimit(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	// 101 URLs should be rejected
	urls := make([]string, 101)
	for i := range urls {
		urls[i] = `{"url": "https://example.com/file` + fmt.Sprintf("%d", i) + `.zip"}`
	}
	body := strings.NewReader(`{"urls": [` + strings.Join(urls, ",") + `]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for 101 URLs, got %d", resp.StatusCode)
	}
}

func TestCloudHandler_BatchCreateDownload_ExactlyMax(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	// 100 URLs should be accepted
	urls := make([]string, 100)
	for i := range urls {
		urls[i] = `{"url": "https://example.com/file` + fmt.Sprintf("%d", i) + `.zip"}`
	}
	body := strings.NewReader(`{"urls": [` + strings.Join(urls, ",") + `]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for 100 URLs, got %d", resp.StatusCode)
	}

	var batchResp struct {
		Tasks []CloudBatchTaskResult `json:"tasks"`
	}
	json.NewDecoder(resp.Body).Decode(&batchResp)
	if len(batchResp.Tasks) != 100 {
		t.Fatalf("expected 100 tasks, got %d", len(batchResp.Tasks))
	}
}
