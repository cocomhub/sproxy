// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func setupCloudTestServerWithSSRF(t *testing.T, allowPrivate bool) (*httptest.Server, *CloudDownloadManager) {
	t.Helper()
	t.Helper()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  allowPrivate,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() {
		mgr.Close()
		os.RemoveAll(filepath.Join(dir, ".__cloud__"))
		os.RemoveAll(filepath.Join(dir, ".__downloads__"))
	})

	h := &Handlers{cloudMgr: mgr, logger: testLogger(), storageMgr: sm, cfgPtr: newTestCfgPtr(dir), auditLogger: testLogger()}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download", h.cloudCreateDownload)
	mux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	mux.HandleFunc("GET /api/cloud/tasks", h.cloudListTasks)
	mux.HandleFunc("GET /api/cloud/tasks/{id}", h.cloudGetTask)
	mux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.cloudCancelTask)
	mux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.cloudDeleteTask)
	mux.HandleFunc("POST /api/cloud/tasks/{id}/resume", h.cloudResumeTask)
	mux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.cloudArchiveTask)
	mux.HandleFunc("POST /api/cloud/groups", h.cloudCreateGroup)
	mux.HandleFunc("GET /api/cloud/groups", h.cloudListGroups)
	mux.HandleFunc("GET /api/cloud/groups/{id}", h.cloudGetGroup)
	mux.HandleFunc("POST /api/cloud/groups/{id}/cancel", h.cloudCancelGroup)
	mux.HandleFunc("DELETE /api/cloud/groups/{id}", h.cloudDeleteGroup)
	mux.HandleFunc("POST /api/cloud/groups/{id}/resume", h.cloudResumeGroup)
	mux.HandleFunc("POST /api/cloud/groups/{id}/archive", h.cloudArchiveGroup)
	return httptest.NewServer(mux), mgr
}

func setupCloudTestServer(t *testing.T) (*httptest.Server, *CloudDownloadManager) {
	t.Helper()
	return setupCloudTestServerWithSSRF(t, true)
}

func setupCloudTestServerWithSSRFEnforced(t *testing.T) (*httptest.Server, *CloudDownloadManager) {
	t.Helper()
	return setupCloudTestServerWithSSRF(t, false)
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
	if task.Status != "pending" && task.Status != "downloading" {
		t.Fatalf("expected status 'pending' or 'downloading', got %q", task.Status)
	}
}

func TestCloudHandler_ListTasks(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100, "")
	mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200, "")

	resp, err := http.Get(ts.URL + "/api/cloud/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var listResp struct {
		Tasks []*CloudTask `json:"tasks"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(listResp.Tasks))
	}
}

func TestCloudHandler_GetTask(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100, "")

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

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100, "")
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

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100, "")
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

	t1, _ := mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100, "")
	t2, _ := mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200, "")
	t1.Status = "completed"
	t2.Status = "failed"

	resp, err := http.Get(ts.URL + "/api/cloud/tasks?status=completed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var listResp struct {
		Tasks []*CloudTask `json:"tasks"`
		Total int          `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Tasks) != 1 {
		t.Fatalf("expected 1 completed task, got %d", len(listResp.Tasks))
	}
}

func TestCloudHandler_SSRFBlocked(t *testing.T) {
	ts, _ := setupCloudTestServerWithSSRFEnforced(t)
	defer ts.Close()

	tests := []struct {
		name   string
		url    string
		expect int
	}{
		{"ftp scheme", "ftp://example.com/file.zip", http.StatusBadRequest},
		{"empty url", "", http.StatusBadRequest},
		{"invalid url", "not-a-url", http.StatusBadRequest},
		{"valid https", "https://example.com/file.zip", http.StatusOK},
		{"loopback 127.0.0.1", "http://127.0.0.1:8080/file.zip", http.StatusBadRequest},
		{"localhost hostname", "http://localhost:8080/file.zip", http.StatusBadRequest},
		{"private 10.x", "http://10.0.0.1/file.zip", http.StatusBadRequest},
		{"private 192.168.x", "http://192.168.1.1/file.zip", http.StatusBadRequest},
		{"private 172.16.x", "http://172.16.0.1/file.zip", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.NewReader(`{"url": "` + tt.url + `"}`)
			resp, err := http.Post(ts.URL+"/api/cloud/download", "application/json", body)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.expect {
				t.Errorf("URL %q: expected %d, got %d", tt.url, tt.expect, resp.StatusCode)
			}
		})
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

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsafe filename, got %d", resp.StatusCode)
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
		if tr.Status != "pending" && tr.Status != "downloading" {
			t.Fatalf("expected status 'pending' or 'downloading', got %q", tr.Status)
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
	if batchResp.Tasks[0].Status != "pending" && batchResp.Tasks[0].Status != "downloading" {
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
	if batchResp.Tasks[0].Status != "failed" {
		t.Fatalf("expected 'failed' for unsafe filename, got %q", batchResp.Tasks[0].Status)
	}
	if batchResp.Tasks[0].Error == "" {
		t.Fatal("expected error message for unsafe filename")
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
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if batchResp.Tasks[0].Status != "pending" && batchResp.Tasks[0].Status != "downloading" {
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
	t.Cleanup(func() { mgr.Close() })

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
	if err := json.NewDecoder(resp.Body).Decode(&batchResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
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
		urls[i] = `{"url": "https://example.com/file` + strconv.Itoa(i) + `.zip"}`
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

func TestCloudHandler_CancelNonexistent(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/nonexistent/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent task, got %d", resp.StatusCode)
	}
}

func TestCloudHandler_DeleteNonexistent(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/cloud/tasks/nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent task, got %d", resp.StatusCode)
	}
}

// --- 组路由与 resume 路由 ---

func TestCloudHandler_GroupCreateGetListArchive(t *testing.T) {
	contentA := []byte("handler group A content")
	contentB := []byte("handler group B content")
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(contentA) }))
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(contentB) }))
	defer srvA.Close()
	defer srvB.Close()

	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	// 创建组
	body, _ := json.Marshal(map[string]any{
		"name": "handler-group",
		"urls": []map[string]string{
			{"url": srvA.URL, "filename": "a.bin"},
			{"url": srvB.URL, "filename": "b.bin"},
		},
	})
	resp, err := http.Post(ts.URL+"/api/cloud/groups", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating group, got %d", resp.StatusCode)
	}
	var group CloudTaskGroup
	if err2 := json.NewDecoder(resp.Body).Decode(&group); err2 != nil {
		t.Fatal(err2)
	}
	resp.Body.Close()
	if len(group.TaskIDs) != 2 {
		t.Fatalf("expected 2 tasks in group, got %d", len(group.TaskIDs))
	}

	// 等待子任务完成
	for _, tid := range group.TaskIDs {
		waitTaskDone(t, mgr, tid)
	}

	// 组详情
	resp, err = http.Get(ts.URL + "/api/cloud/groups/" + group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 getting group, got %d", resp.StatusCode)
	}
	var detail struct {
		Group *CloudTaskGroup `json:"group"`
		Tasks []*CloudTask    `json:"tasks"`
	}
	if err2 := json.NewDecoder(resp.Body).Decode(&detail); err2 != nil {
		t.Fatal(err2)
	}
	resp.Body.Close()
	if detail.Group == nil || detail.Group.Status != "completed" {
		t.Fatalf("expected completed group, got %+v", detail.Group)
	}
	if len(detail.Tasks) != 2 {
		t.Fatalf("expected 2 tasks in detail, got %d", len(detail.Tasks))
	}

	// 组列表
	resp, err = http.Get(ts.URL + "/api/cloud/groups")
	if err != nil {
		t.Fatal(err)
	}
	var listResp struct {
		Groups []CloudTaskGroup `json:"groups"`
		Total  int              `json:"total"`
	}
	if err2 := json.NewDecoder(resp.Body).Decode(&listResp); err2 != nil {
		t.Fatal(err2)
	}
	resp.Body.Close()
	if len(listResp.Groups) != 1 {
		t.Fatalf("expected 1 group in list, got %d", len(listResp.Groups))
	}

	// 组归档（按子任务目录收集已完成文件）
	archiveBody := `{"archive_name": "handler-group.tar.gz"}`
	resp, err = http.Post(ts.URL+"/api/cloud/groups/"+group.ID+"/archive", "application/json", strings.NewReader(archiveBody))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 archiving group, got %d", resp.StatusCode)
	}
	var arch CloudArchiveResult
	if err2 := json.NewDecoder(resp.Body).Decode(&arch); err2 != nil {
		t.Fatal(err2)
	}
	resp.Body.Close()
	if !arch.Success || arch.File == "" || arch.TaskCount != 2 {
		t.Fatalf("unexpected archive result: %+v", arch)
	}
	// 归档文件真实存在
	archivePath := filepath.Join(mgr.uploadsDir, filepath.FromSlash(arch.File))
	if _, err2 := os.Stat(archivePath); err2 != nil {
		t.Fatalf("expected archive file on disk: %v", err2)
	}

	// archive_file 已落库到真实组对象
	resp, err = http.Get(ts.URL + "/api/cloud/groups/" + group.ID)
	if err != nil {
		t.Fatal(err)
	}
	var detail2 struct {
		Group *CloudTaskGroup `json:"group"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&detail2)
	resp.Body.Close()
	if detail2.Group == nil || detail2.Group.ArchiveFile != arch.File {
		t.Fatalf("expected archive_file %q persisted, got %q", arch.File, detail2.Group.ArchiveFile)
	}
}

func TestCloudHandler_ResumeTaskEndpoint(t *testing.T) {
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv404.Close()

	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	// 提交一个必然失败的异步任务
	body := strings.NewReader(`{"url": "` + srv404.URL + `"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	var task CloudTask
	if err2 := json.NewDecoder(resp.Body).Decode(&task); err2 != nil {
		t.Fatal(err2)
	}
	resp.Body.Close()

	// 等待失败
	waitTaskDone(t, mgr, task.ID)
	if cur, _ := mgr.SnapshotTask(task.ID, ""); cur.Status != "failed" {
		t.Fatalf("expected failed task, got %q", cur.Status)
	}

	// resume 失败任务 → 200
	resp, err = http.Post(ts.URL+"/api/cloud/tasks/"+task.ID+"/resume", "application/json", strings.NewReader(`{"force": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 resuming failed task, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// resume 不存在任务 → 404
	resp, err = http.Post(ts.URL+"/api/cloud/tasks/nonexistent/resume", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 resuming nonexistent task, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestCloudHandler_BatchAndGroup_ConfigurableMaxLimit 验证批量/组上限来自服务端配置
// MaxBatchURLs（而非硬编码 100）：配置为 2 时 3 个 URL 被 400 拒绝，2 个 URL 正常通过。
func TestCloudHandler_BatchAndGroup_ConfigurableMaxLimit(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		MaxBatchURLs:  2,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr := NewCloudDownloadManager(dir, sm, nil, testLogger(), cfg)
	t.Cleanup(func() { mgr.Close() })
	h := &Handlers{cloudMgr: mgr, logger: testLogger()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	mux.HandleFunc("POST /api/cloud/groups", h.cloudCreateGroup)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	urlsArrayJSON := func(n int) string {
		var parts []string
		for i := range n {
			parts = append(parts, fmt.Sprintf(`{"url": "https://example.com/f%d.zip"}`, i))
		}
		return `[` + strings.Join(parts, ",") + `]`
	}

	// 批量：3 个 URL 超过配置上限 2 → 400
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", strings.NewReader(`{"urls": `+urlsArrayJSON(3)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("batch 3 URLs: expected 400, got %d (%s)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "maximum 2 URLs per batch") {
		t.Fatalf("expected error mentioning limit 2, got: %s", body)
	}

	// 批量：2 个 URL 未超限 → 200
	resp2, err := http.Post(ts.URL+"/api/cloud/download/batch", "application/json", strings.NewReader(`{"urls": `+urlsArrayJSON(2)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("batch 2 URLs: expected 200, got %d", resp2.StatusCode)
	}

	// 组：3 个 URL 超过配置上限 2 → 400
	resp3, err := http.Post(ts.URL+"/api/cloud/groups", "application/json", strings.NewReader(`{"name":"g","urls": `+urlsArrayJSON(3)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusBadRequest {
		t.Fatalf("group 3 URLs: expected 400, got %d (%s)", resp3.StatusCode, body3)
	}
	if !strings.Contains(string(body3), "maximum 2 URLs per group") {
		t.Fatalf("expected error mentioning limit 2, got: %s", body3)
	}
}

// TestCloudHandler_CreateGroup_NormalizesURL 回归测试：cloudCreateGroup 校验后必须把
// 规范化后的 URL/Filename 传给 SubmitAndStartGroup。旧代码丢弃规范化结果，组路径用
// 原始 URL 做去重与文件名推导，与单条/批量路径不一致（同一内容不同拼写在组路径会
// 生成重复下载、组内冲突判定与 UI/CLI 本地预检偶发不一致）。
func TestCloudHandler_CreateGroup_NormalizesURL(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	// 大写 scheme 应被规范化为小写 http://；端口 1 使下载连接被拒、快速失败，
	// 不依赖外部网络（断言只看任务创建时的 URL）
	body := strings.NewReader(`{"name":"g1","urls":[{"url":"HTTP://127.0.0.1:1/file.zip"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/groups", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, respBody)
	}

	var group CloudTaskGroup
	if err := json.Unmarshal(respBody, &group); err != nil {
		t.Fatal(err)
	}
	if len(group.TaskIDs) == 0 {
		t.Fatal("expected group to have tasks")
	}

	// 组内子任务的 URL 应为规范化后的值
	mgr.mu.RLock()
	var taskURL string
	for _, tid := range group.TaskIDs {
		if t2, ok := mgr.tasks[tid]; ok {
			taskURL = t2.URL
			break
		}
	}
	mgr.mu.RUnlock()
	if taskURL != "http://127.0.0.1:1/file.zip" {
		t.Fatalf("expected normalized URL %q, got %q", "http://127.0.0.1:1/file.zip", taskURL)
	}
}
