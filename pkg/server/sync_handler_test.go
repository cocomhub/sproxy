// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/syncexec"
)

// newSyncTestEnv 创建带 SyncManager 的测试服务。remoteURL 是 sync_remotes[0].url。
func newSyncTestEnv(t *testing.T, remoteURL string, modifyCfg func(*Config)) (h *Handlers, baseURL string) {
	t.Helper()
	dir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = dir
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	h = RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux: mux, CfgPtr: &cfgPtr, Version: "test", BuildAt: "test", Logger: slog.Default(),
	})
	remotes := []syncmgr.RemoteConfig{{
		Name: "r1", URL: remoteURL, AccessKey: "test-ak", AccessKeySecret: strings.Repeat("a", 64),
	}}
	sm := syncmgr.NewManager(cfg.UploadsDir, h.SyncQuotaStore(), int(CategoryUserFiles), remotes,
		syncexec.NewExecutor(cfg.UploadsDir, h.logger), h.logger,
		&syncmgr.Config{MaxConcurrent: 3, TaskTTL: 24 * time.Hour})
	h.SetSyncMgr(sm)

	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		sm.Stop()
		_ = h.Close()
	})
	return h, ts.URL
}

// emptyRemote 返回一个空的远程 sproxy（GET /api/files 空列表、stat 404）。
func emptyRemote(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[],"total":0}`))
	})
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// blockingListRemote 的 GET /api/files 阻塞直到 blockCh 关闭或请求 ctx 取消。
func blockingListRemote(t *testing.T, blockCh chan struct{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-blockCh:
		}
		http.Error(w, "aborted", http.StatusGatewayTimeout)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func doSyncJSON(t *testing.T, method, url, body string) (int, []byte) {
	t.Helper()
	var req *http.Request
	var err error
	if body != "" {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	} else {
		req, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func TestSyncAPI_CreateTask_Push(t *testing.T) {
	srv := emptyRemote(t)
	h, base := newSyncTestEnv(t, srv.URL, nil)
	// seed 本地源文件，使 push 任务真正可成功（审查 M-2：src 不存在会快速 failed，
	// 硬断言 pending 属时序侥幸，产生 flake）。
	if err := os.WriteFile(filepath.Join(h.cfgPtr.Load().UploadsDir, "x.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"x.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("解析失败: %v, body=%s", err, body)
	}
	if task.ID == "" || task.Direction != "push" || task.Remote != "r1" {
		t.Fatalf("任务字段不符: %+v", task)
	}
	// 状态为异步流转（响应时可能 pending/syncing/completed）；seed 后不应 failed
	if task.Status == "failed" || task.Status == "" {
		t.Fatalf("任务状态不应为 failed/空（seed 后 src 存在）: %+v", task)
	}
}

func TestSyncAPI_CreateTask_InvalidDirection(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"sideways","remote":"r1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("非法 direction 应返回 400，got %d: %s", code, body)
	}
}

func TestSyncAPI_CreateTask_RemoteMissing(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"nope"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未配置 remote 应返回 400，got %d: %s", code, body)
	}
}

func TestSyncAPI_CreateTask_StorageFull(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, func(c *Config) { c.MaxStorageBytes = 1024 })

	// pull 方向预留 1 GiB 占位 → 存储不足 → 507
	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"pull","remote":"r1","src":""}`)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("存储不足应返回 507，got %d: %s", code, body)
	}
}

func TestSyncAPI_ListAndGet(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"x.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}

	// 列表
	code, body = doSyncJSON(t, "GET", base+"/api/sync/tasks", "")
	if code != http.StatusOK {
		t.Fatalf("列表应返回 200，got %d: %s", code, body)
	}
	var listResp struct {
		Success bool `json:"success"`
		Tasks   []syncmgr.SyncTaskMeta
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatal(err)
	}
	if !listResp.Success || len(listResp.Tasks) != 1 {
		t.Fatalf("列表不符: %s", body)
	}
	if listResp.Tasks[0].ID != task.ID {
		t.Fatalf("列表任务 ID 不符: %+v", listResp.Tasks[0])
	}

	// 单查
	code, body = doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
	if code != http.StatusOK {
		t.Fatalf("单查应返回 200，got %d: %s", code, body)
	}
	var got syncmgr.SyncTask
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != task.ID {
		t.Fatalf("单查任务不符: %+v", got)
	}

	// 未知 ID → 404
	code, _ = doSyncJSON(t, "GET", base+"/api/sync/tasks/does-not-exist", "")
	if code != http.StatusNotFound {
		t.Fatalf("未知任务应返回 404，got %d", code)
	}
}

func TestSyncAPI_CancelTask(t *testing.T) {
	blockCh := make(chan struct{})
	defer close(blockCh)
	srv := blockingListRemote(t, blockCh)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	// pull src="" → GET /api/files 阻塞 → 任务保持 syncing
	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"pull","remote":"r1","src":""}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}

	// 等待 syncing
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
		var cur syncmgr.SyncTask
		_ = json.Unmarshal(b, &cur)
		if cur.Status == "syncing" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	code, body = doSyncJSON(t, "POST", base+"/api/sync/tasks/"+task.ID+"/cancel", "")
	if code != http.StatusOK {
		t.Fatalf("取消应返回 200，got %d: %s", code, body)
	}
	if !strings.Contains(string(body), "cancelled") {
		t.Fatalf("取消响应应包含 cancelled: %s", body)
	}

	// 复查状态
	_, b := doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
	var cur syncmgr.SyncTask
	_ = json.Unmarshal(b, &cur)
	if cur.Status != "cancelled" {
		t.Fatalf("取消后状态应为 cancelled，got %q", cur.Status)
	}
}

func TestSyncAPI_DeleteTask(t *testing.T) {
	srv := emptyRemote(t)
	_, base := newSyncTestEnv(t, srv.URL, nil)

	code, body := doSyncJSON(t, "POST", base+"/api/sync/tasks",
		`{"direction":"push","remote":"r1","src":"x.txt"}`)
	if code != http.StatusCreated {
		t.Fatalf("创建应返回 201，got %d: %s", code, body)
	}
	var task syncmgr.SyncTask
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}

	code, body = doSyncJSON(t, "DELETE", base+"/api/sync/tasks/"+task.ID, "")
	if code != http.StatusOK {
		t.Fatalf("删除应返回 200，got %d: %s", code, body)
	}
	if !strings.Contains(string(body), "deleted") {
		t.Fatalf("删除响应应包含 deleted: %s", body)
	}

	// 再查 → 404
	code, _ = doSyncJSON(t, "GET", base+"/api/sync/tasks/"+task.ID, "")
	if code != http.StatusNotFound {
		t.Fatalf("删除后单查应返回 404，got %d", code)
	}
}

func TestSyncAPI_NotConfigured(t *testing.T) {
	// 不注入 SyncManager → 400
	dir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = dir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{Mux: mux, CfgPtr: &cfgPtr, Version: "test", BuildAt: "test", Logger: slog.Default()})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	code, body := doSyncJSON(t, "POST", ts.URL+"/api/sync/tasks",
		`{"direction":"push","remote":"r1"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未配置 sync 应返回 400，got %d: %s", code, body)
	}
}
