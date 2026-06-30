// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
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
