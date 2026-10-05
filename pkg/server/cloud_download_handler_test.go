// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
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

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
	"github.com/cocomhub/sproxy/pkg/volume/secretdata"
)

func setupCloudTestServerWithSSRF(t *testing.T, allowPrivate bool) (*httptest.Server, *cloud.CloudDownloadManager) {
	t.Helper()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  allowPrivate,
	}
	// 装配租户布局（cloudArchiveTask/cloudArchiveGroup 读取任务源文件经 cloudDirFor 解析）
	h := newAssemblyTestHandlers(t, dir)
	h.storageMgr = sm
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{UploadsDir: dir, Storage: cloudStorageManager{m: sm}, TenantFor: h.tenantFor, ChecksumStoreFor: h.checksumStoreFor, ListTenants: h.listTenantIDs, Logger: testLogger(), Config: cfg})
	h.cloudMgr = mgr

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

func setupCloudTestServer(t *testing.T) (*httptest.Server, *cloud.CloudDownloadManager) {
	t.Helper()
	return setupCloudTestServerWithSSRF(t, true)
}

func setupCloudTestServerWithSSRFEnforced(t *testing.T) (*httptest.Server, *cloud.CloudDownloadManager) {
	t.Helper()
	return setupCloudTestServerWithSSRF(t, false)
}

func TestCloudHandler_CreateDownloadTask(t *testing.T) {
	ts, _ := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"url": "https://example.com/file.zip", "filename": "file.zip"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download", contentTypeJSON, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var task cloud.CloudTask
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

// TestCloudHandler_CreateDownloadTask_ForceIntegrity 任务5：POST /api/cloud/download 的
// force_integrity 字段解析 → TaskParams.ForceIntegrity（服务端接收链路最后一跳）。
func TestCloudHandler_CreateDownloadTask_ForceIntegrity(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	body := strings.NewReader(`{"url": "https://example.com/force.png", "force_integrity": true}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download", contentTypeJSON, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var task cloud.CloudTask
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		t.Fatal(err)
	}
	if task.ID == "" {
		t.Fatal("expected non-empty task ID")
	}
	// 落点断言：请求 struct 的 ForceIntegrity 必须进入 TaskParams → task 字段。
	taskInMgr, ok := mgr.GetTask(task.ID, "")
	if !ok {
		t.Fatalf("task %s not found in manager", task.ID)
	}
	if !taskInMgr.ForceIntegrity {
		t.Fatalf("force_integrity=true 未落到 TaskParams.ForceIntegrity（task=%+v）", taskInMgr)
	}
}

func TestCloudHandler_ListTasks(t *testing.T) {
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100, "", cloud.TaskParams{Save: true})
	mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200, "", cloud.TaskParams{Save: true})

	resp, err := http.Get(ts.URL + "/api/cloud/tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var listResp struct {
		Tasks []*cloud.CloudTask `json:"tasks"`
		Total int                `json:"total"`
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

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100, "", cloud.TaskParams{Save: true})

	resp, err := http.Get(ts.URL + "/api/cloud/tasks/" + task.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var got cloud.CloudTask
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

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100, "", cloud.TaskParams{Save: true})
	task.Status = "downloading"

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/"+task.ID+"/cancel", contentTypeJSON, nil)
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

	task, _ := mgr.CreateTask("url", "https://example.com/file.zip", "file.zip", 100, "", cloud.TaskParams{Save: true})
	task.Status = "completed"

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/cloud/tasks/"+task.ID, nil)
	resp, err := testHTTPClient(t).Do(req)
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

	t1, _ := mgr.CreateTask("url", "https://example.com/a.zip", "a.zip", 100, "", cloud.TaskParams{Save: true})
	t2, _ := mgr.CreateTask("url", "https://example.com/b.zip", "b.zip", 200, "", cloud.TaskParams{Save: true})
	t1.Status = "completed"
	t2.Status = "failed"

	resp, err := http.Get(ts.URL + "/api/cloud/tasks?status=completed")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var listResp struct {
		Tasks []*cloud.CloudTask `json:"tasks"`
		Total int                `json:"total"`
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
			resp, err := http.Post(ts.URL+"/api/cloud/download", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	sm := capacity.NewStorageManager(dir, 50, nil, testLogger())
	cfg := &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
	}
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)

	h := &Handlers{cloudMgr: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 请求 100 字节，超过 50 字节上限
	body := strings.NewReader(`{"urls": [{"url": "https://example.com/big.zip"}]}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, body)
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

	resp, err := http.Post(ts.URL+"/api/cloud/tasks/nonexistent/cancel", contentTypeJSON, nil)
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
	resp, err := testHTTPClient(t).Do(req)
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

	group := helperGroupCreateGetListArchive_createGroup(t, ts, srvA, srvB)

	// 等待子任务完成
	for _, tid := range group.TaskIDs {
		waitTaskDone(t, mgr, tid)
	}

	helperGroupCreateGetListArchive_getDetail(t, ts, group)
	helperGroupCreateGetListArchive_list(t, ts)
	helperGroupCreateGetListArchive_archive(t, ts, mgr, group)
}

// helperGroupCreateGetListArchive_createGroup 创建包含两个子任务的组并
// 断言响应返回 2 个 task。
func helperGroupCreateGetListArchive_createGroup(t *testing.T, ts *httptest.Server, srvA, srvB *httptest.Server) cloud.CloudTaskGroup {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"name": "handler-group",
		"urls": []map[string]string{
			{"url": srvA.URL, "filename": "a.bin"},
			{"url": srvB.URL, "filename": "b.bin"},
		},
	})
	resp, err := http.Post(ts.URL+"/api/cloud/groups", contentTypeJSON, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 creating group, got %d", resp.StatusCode)
	}
	var group cloud.CloudTaskGroup
	if err2 := json.NewDecoder(resp.Body).Decode(&group); err2 != nil {
		t.Fatal(err2)
	}
	resp.Body.Close()
	if len(group.TaskIDs) != 2 {
		t.Fatalf("expected 2 tasks in group, got %d", len(group.TaskIDs))
	}
	return group
}

// helperGroupCreateGetListArchive_getDetail 查询组详情并断言状态
// completed、任务数为 2。
func helperGroupCreateGetListArchive_getDetail(t *testing.T, ts *httptest.Server, group cloud.CloudTaskGroup) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/cloud/groups/" + group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 getting group, got %d", resp.StatusCode)
	}
	var detail struct {
		Group *cloud.CloudTaskGroup `json:"group"`
		Tasks []*cloud.CloudTask    `json:"tasks"`
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
}

// helperGroupCreateGetListArchive_list 查询组列表并断言恰好 1 个组。
func helperGroupCreateGetListArchive_list(t *testing.T, ts *httptest.Server) {
	t.Helper()
	resp, err := http.Get(ts.URL + "/api/cloud/groups")
	if err != nil {
		t.Fatal(err)
	}
	var listResp struct {
		Groups []cloud.CloudTaskGroup `json:"groups"`
		Total  int                    `json:"total"`
	}
	if err2 := json.NewDecoder(resp.Body).Decode(&listResp); err2 != nil {
		t.Fatal(err2)
	}
	resp.Body.Close()
	if len(listResp.Groups) != 1 {
		t.Fatalf("expected 1 group in list, got %d", len(listResp.Groups))
	}
}

// helperGroupCreateGetListArchive_archive 组归档：断言归档成功、归档文件
// 真实落盘，且 archive_file 已持久化到组对象。
func helperGroupCreateGetListArchive_archive(t *testing.T, ts *httptest.Server, mgr *cloud.CloudDownloadManager, group cloud.CloudTaskGroup) {
	t.Helper()
	archiveBody := `{"archive_name": "handler-group.tar.gz"}`
	resp, err := http.Post(ts.URL+"/api/cloud/groups/"+group.ID+"/archive", contentTypeJSON, strings.NewReader(archiveBody))
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
	// 归档响应 File 只含归档名；磁盘落在 <root>/anonymous/archive/ 下（未认证 owner 空）。
	archivePath := filepath.Join(mgr.UploadsDir(), anonymousOwner, "archive", filepath.FromSlash(arch.File))
	if _, err2 := os.Stat(archivePath); err2 != nil {
		t.Fatalf("expected archive file on disk: %v", err2)
	}

	// archive_file 已落库到真实组对象
	resp, err = http.Get(ts.URL + "/api/cloud/groups/" + group.ID)
	if err != nil {
		t.Fatal(err)
	}
	var detail2 struct {
		Group *cloud.CloudTaskGroup `json:"group"`
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
	resp, err := http.Post(ts.URL+"/api/cloud/download", contentTypeJSON, body)
	if err != nil {
		t.Fatal(err)
	}
	var task cloud.CloudTask
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
	resp, err = http.Post(ts.URL+"/api/cloud/tasks/"+task.ID+"/resume", contentTypeJSON, strings.NewReader(`{"force": true}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 resuming failed task, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// resume 不存在任务 → 404
	resp, err = http.Post(ts.URL+"/api/cloud/tasks/nonexistent/resume", contentTypeJSON, strings.NewReader("{}"))
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
	sm := capacity.NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	cfg := &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		MaxBatchURLs:  2,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)
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
	resp, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, strings.NewReader(`{"urls": `+urlsArrayJSON(3)+`}`))
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
	resp2, err := http.Post(ts.URL+"/api/cloud/download/batch", contentTypeJSON, strings.NewReader(`{"urls": `+urlsArrayJSON(2)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("batch 2 URLs: expected 200, got %d", resp2.StatusCode)
	}

	// 组：3 个 URL 超过配置上限 2 → 400
	resp3, err := http.Post(ts.URL+"/api/cloud/groups", contentTypeJSON, strings.NewReader(`{"name":"g","urls": `+urlsArrayJSON(3)+`}`))
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
	resp, err := http.Post(ts.URL+"/api/cloud/groups", contentTypeJSON, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, respBody)
	}

	var group cloud.CloudTaskGroup
	if err := json.Unmarshal(respBody, &group); err != nil {
		t.Fatal(err)
	}
	if len(group.TaskIDs) == 0 {
		t.Fatal("expected group to have tasks")
	}

	// 组内子任务的 URL 应为规范化后的值（走领域 API：装配层不触碰领域内部状态）
	var taskURL string
	if snaps := mgr.SnapshotTasks(group.TaskIDs, ""); len(snaps) > 0 {
		taskURL = snaps[0].URL
	}
	if taskURL != "http://127.0.0.1:1/file.zip" {
		t.Fatalf("expected normalized URL %q, got %q", "http://127.0.0.1:1/file.zip", taskURL)
	}
}

// TestCloudHandler_CreateDownloadTask_507OnTenantQuota 锁定审查 C 缺口 6：
// 配额满时 POST /api/cloud/download 返回 507（InsufficientStorage）+ 无任务残留 + Scope 0。
//
// 说明：handler 提交恒为未知大小（totalSize=-1），创建期同步配额门是全局 storageMgr
// 的 1 GiB 占位预留（m.storage.TryReserve(cloudReservePlaceholder)）；租户 Scope 的容量
// 预检仅在已知大小路径（totalSize>0）触发，未知大小的租户配额在写盘流 NewQuotaWriter
// 占位预留时执行（异步失败，不返回 507）。因此本测试把全局 storageMgr 上限压到占位之下
// 触发同步 507，并断言 Scope 不因失败的创建留下任何占用（双轨无泄漏）。
func TestCloudHandler_CreateDownloadTask_507OnTenantQuota(t *testing.T) {
	dir := t.TempDir()
	// 全局 max = 512MB < 1 GiB 占位 → 未知大小任务创建即 507。
	sm := capacity.NewStorageManager(dir, 512*1024*1024, nil, testLogger())
	cfg := &cloud.CloudDownloadConfig{
		SyncThreshold: 20 * 1024 * 1024,
		MaxConcurrent: 3,
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 1 * time.Hour,
		AllowPrivate:  true,
	}
	mgr, h := newCloudTestManager(t, dir, sm, cfg)
	setTestOwnerQuota(h, "alice", 100) // 租户配额也满（100 字节，无法容纳任何下载）

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/cloud/download", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		h.cloudCreateDownload(w, r)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	body := strings.NewReader(`{"url": "https://example.com/big.bin", "filename": "big.bin"}`)
	resp, err := http.Post(ts.URL+"/api/cloud/download", contentTypeJSON, body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("配额满应 507, got %d", resp.StatusCode)
	}

	// 无任务残留（创建失败未入 tasks）。
	tasks, _ := mgr.ListTasks("", -1, 0, "alice")
	if len(tasks) != 0 {
		t.Fatalf("507 后应无任务残留, got %d", len(tasks))
	}

	// Scope 0（双轨无泄漏：storageMgr 与租户 Scope 均无占用）。
	if got := sm.Usage(); got != 0 {
		t.Fatalf("507 后 storageMgr Usage()=%d want 0", got)
	}
	cloudB := h.quotaBucketFor("alice", "cloud")
	if cloudB == nil {
		t.Fatal("alice cloud 桶 Scope 应为非 nil")
	}
	if got := cloudB.Usage(); got != 0 {
		t.Fatalf("507 后 cloud 桶 Usage()=%d want 0", got)
	}
	if got := cloudB.Reserved(); got != 0 {
		t.Fatalf("507 后 cloud 桶 Reserved()=%d want 0", got)
	}
}

// TestCloudHandler_BatchCreateDownload_TransferACLDenied R1 回归：batch 路径转存
// 目标卷 ACL 校验（CLI 链式主路径走 batch——此前漏检可跨租户转存写入）。
// checkTransferACL 是单条/batch 共用校验（单条 handler 与 batch handler 均调用）；
// 此处验证 helper 对 ACL 拒绝卷 fail-closed + 放行卷放行。
func TestCloudHandler_BatchCreateDownload_TransferACLDenied(t *testing.T) {
	t.Parallel()
	h := &Handlers{volSet: newTransferACLSet(t)}
	// ACL 拒绝卷（无授权）→ fail-closed
	msg := h.checkTransferACL("ownerX", &cloud.TransferSpec{Volume: "private-vault"})
	if msg == "" {
		t.Fatal("ACL 拒绝卷应返回错误")
	}
	// 未装配卷 → fail-closed
	msg = h.checkTransferACL("ownerX", &cloud.TransferSpec{Volume: "missing"})
	if msg == "" {
		t.Fatal("未装配卷应返回错误")
	}
	// 无 transfer → 放行
	if msg := h.checkTransferACL("ownerX", nil); msg != "" {
		t.Fatalf("无 transfer 应放行，got %q", msg)
	}
}

// newTransferACLSet 构造含 ACL deny 卷的 registry.Set（R1/C1 测试用）。
// private-vault：ModeAllow + 空白名单 = 默认拒绝（所有 owner 拒），模拟「私有卷」。
// public-vault 由调用方经 AddExternalVolume 注入（external 卷才在 transferFS 解析面内）。
func newTransferACLSet(t *testing.T) *registry.Set {
	t.Helper()
	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	vols := []volume.Volume{
		{
			// ModeAllow + 空白名单 = 默认拒绝（所有 owner 拒），模拟「私有卷」。
			Name: "private-vault", Type: volume.TypeLocal, RootDir: root,
			ACL: volume.ACL{Mode: volume.ModeAllow, Owners: map[string]struct{}{}},
		},
	}
	vs := registry.NewSet(vols, map[string]*storage.Root{"private-vault": rt}, nil, nil, "private-vault")
	t.Cleanup(func() { _ = vs.Close() })
	return vs
}

// TestCloudHandler_CreateGroup_PassesThreeParams C1 回归：组创建请求透传三参 → 服务端真实
// 解析。此前 createGroupEntry 硬编码 Save:true，组链 save 参数在服务端最后一跳被丢弃。
// 本测试验证：真空洞组合（save=false + download_local=false + 无 transfer）在组路径
// fail-closed 拒绝（旧硬编码 Save:true 会绕过校验静默做错）；save/download_local 落点
// 断言见 TestCloudHandler_CreateGroup_TransferParam（已装配 transfer 卷）。
func TestCloudHandler_CreateGroup_PassesThreeParams(t *testing.T) {
	t.Parallel()
	ts, mgr := setupCloudTestServer(t)
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{
		"name": "three-param-group",
		"urls": []map[string]string{
			{"url": "https://example.com/c1.bin", "filename": "c1.bin"},
		},
		"save":           false,
		"download_local": false,
	})
	resp, err := http.Post(ts.URL+"/api/cloud/groups", contentTypeJSON, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	// 真空洞（save=false + download_local=false + 无 transfer）应 fail-closed 拒绝——
	// 证明 C1 修复后组路径的语义空洞校验生效（旧硬编码 Save:true 会绕过校验静默做错）。
	if resp.StatusCode != http.StatusBadRequest {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 (void semantics) for save=false+dl=false+no transfer, got %d: %s", resp.StatusCode, b)
	}
	resp.Body.Close()
	_ = mgr // setupCloudTestServer 返回 mgr 供落点断言；真空洞路径无需再用
}

// TestCloudHandler_CreateGroup_TransferParam C1：组请求 transfer 参数落点——装配 volSet 与
// transferFSFor 的 mgr 经 checkTransferACL/checkTransferVolumePreflight 放行后，子任务带
// Transfer 目标（此前组创建无 transfer 路径）。external 卷用 secretdata backend 注入。
func TestCloudHandler_CreateGroup_TransferParam(t *testing.T) {
	t.Parallel()
	vs := newTransferACLSet(t)
	// 注入 secretdata external 卷（public-vault）：registry.External 按名返回 → transferFS 解析成功。
	// 注册 "secretdata" 协议供 SchemeOf/checkTransferVolumePreflight 使用（t.Cleanup 解绑）。
	// 类型用 "secretdata" 本身：SchemeOf(vol.Type) 按类型反查，注册 "secretdata-c1" 类型
	// 会让 SchemeOf("secretdata") 查不到（协议已声明但类型不匹配）。pkg/server 测试进程
	// 无 cmd/sproxy 的 secretdata 生产注册，无冲突。
	registry.RegisterBackend("secretdata", func(context.Context, volume.Volume) (registry.ExternalBackend, error) {
		return nil, fmt.Errorf("unused")
	}, "secretdata")
	t.Cleanup(func() { registry.UnregisterBackendForTest("secretdata") })
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	be, err := secretdata.NewBackend(t.Context(), volume.Volume{Name: "public-vault", Type: "secretdata"}, inner, secretdata.Options{Secret: []byte("c1-test-key-000"), TempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := vs.AddExternalVolume(volume.Volume{Name: "public-vault", Type: "secretdata"}, be); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{volSet: vs}
	sm := capacity.NewStorageManager(t.TempDir(), 10*1024*1024*1024, nil, testLogger())
	mgr := cloud.NewCloudDownloadManager(cloud.CloudManagerOptions{
		Storage: cloudStorageManager{m: sm},
		Logger:  testLogger(),
		Config:  defaultCloudDownloadConfig(),
		// transferFSFor 用 volSet 装配：public-vault 开放卷 → (FS, scheme, shared)
		TransferFSFor: func(volumeName string) (syncpkg.FS, string, bool) {
			be := vs.External(volumeName)
			if be == nil {
				return nil, "", false
			}
			vol, ok := vs.ByName(volumeName)
			if !ok {
				return be.FS(), "", false
			}
			shared := vol.ACL.Mode == volume.ModeDeny || vol.ACL.Mode == "" || len(vol.ACL.Owners) > 1
			return be.FS(), registry.SchemeOf(vol.Type), shared
		},
	})
	h.cloudMgr = mgr

	body, _ := json.Marshal(map[string]any{
		"name": "transfer-group",
		"urls": []map[string]string{
			{"url": "https://example.com/c2.bin", "filename": "c2.bin"},
		},
		"transfer":       map[string]any{"volume": "public-vault", "path": "tgt/c2.bin"},
		"save":           false,
		"download_local": false,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	h.cloudCreateGroup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with transfer, got %d: %s", rec.Code, rec.Body.String())
	}
	var group cloud.CloudTaskGroup
	if err := json.NewDecoder(rec.Body).Decode(&group); err != nil {
		t.Fatal(err)
	}
	task, ok := mgr.SnapshotTask(group.TaskIDs[0], "")
	if !ok {
		t.Fatalf("task %s not found", group.TaskIDs[0])
	}
	if task.Transfer == nil || task.Transfer.Volume != "public-vault" || task.Transfer.Path != "tgt/c2.bin" {
		t.Fatalf("C1: 组子任务 Transfer 落点错误 = %+v", task.Transfer)
	}
	// save/download_local 落点：请求 save=false + download_local=false → 子任务同值
	// （C1 修复前硬编码 Save:true + download_local=false 恒真——save 参数被丢弃）。
	if task.Save {
		t.Fatal("C1: 组子任务 save 应 false（请求 save=false 被硬编码 true 覆盖）")
	}
	if task.DownloadLocal {
		t.Fatal("C1: 组子任务 download_local 应 false")
	}
}
